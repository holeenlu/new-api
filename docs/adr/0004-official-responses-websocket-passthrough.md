# ADR 0004: Official Responses WebSocket passthrough

Status: Implemented transport; targeted verification remains incomplete. Warm-up
(`generate:false`) turns use the ordinary usage-based settlement path, so no
warm-up-specific billing state was added. A live upstream WebSocket pins one
channel, model, and credential. A turn that may have reached upstream is never
transparently replayed. Dedicated WebSocket-vs-HTTP ledger-parity and warm-up
billing tests remain release follow-up and are not inferred merely from shared
code paths.

## Context

OpenAI officially documents a WebSocket transport for the Responses API
(`wss://api.openai.com/v1/responses`). A client keeps one persistent, stateful
connection and continues each turn by sending a `response.create` event with a
`previous_response_id` plus only the new input items. The documented contract:

- Client sends `response.create` events; payload mirrors the HTTP create body
  minus transport fields (`stream`, `stream_options`, `background`).
- `previous_response_id` chains turns against a connection-local cache; an
  uncached id returns `previous_response_not_found`, and a failed turn (4xx/5xx)
  evicts the referenced response.
- `generate: false` warms the connection: it prepares request state and returns
  a response id without producing model output.
- One connection processes requests **sequentially** (one in-flight response);
  parallelism needs multiple connections.
- Connection lifetime is capped at **60 minutes**; reconnect at the limit.
- Compatible with `store=false` / zero data retention (state is memory-only).
- Server events and ordering match the existing Responses streaming model.

new-api already contains a Responses WebSocket bridge, but it is
ChatGPT/Codex-subscription-specific:

- Client side: `controller.CodexResponsesWebSocket` accepts a client WebSocket on
  `GET /v1/responses`, but rejects any channel whose type is not
  `ChannelTypeCodex`.
- Upstream side: only the Codex adaptor opens an upstream WebSocket, dialing the
  private `/backend-api/codex/responses` path with
  `OpenAI-Beta: responses_websockets=2026-02-06`. The session type lives in the
  `relay/channel/codex` package (`codex.ResponsesWebSocketSession`).

So regular Responses-capable channels cannot use the transport at all, and the
one path that can is pinned to a private endpoint. The goal is full official
support: client WebSocket ↔ new-api ↔ upstream WebSocket for any channel whose
upstream implements the standard protocol, delivering the documented latency
benefit for tool-heavy agent workflows.

Non-negotiable constraints:

- Per-turn billing invariants (`AGENTS.md`) must hold on every WebSocket turn:
  pre-consume → settle → refund with no cross-turn leak, refund-exactly-once, no
  double-charge, saturation safety, and subscription-OAuth lease release on every
  exit path.
- The existing Codex WebSocket path and all HTTP relay paths must not regress.
- JSON marshal/unmarshal goes through the `common.*` wrappers.

## Decision

**Channel configuration (single state owner).** Add a per-channel boolean, e.g.
`ChannelOtherSettings.ResponsesWebSocketEnabled`, that declares "this upstream
implements the standard Responses WebSocket protocol." Channel other-settings is
the sole owner. Codex subscription channels keep their existing behavior without
the flag (their WS support is implied by type). For a standard channel, the flag
controls admission of new upstream WebSocket connections. It does not take
ownership away from a WebSocket that is already live: that connection remains
the transport owner for later self-contained and continuation turns until it is
invalidated or the downstream session ends.

**Shared transport.** Extract the Codex `ResponsesWebSocketSession` into a shared
session (e.g. `relay/common` or a new `relay/responsesws` package) parameterized
by the adaptor-supplied WebSocket URL, headers, and lease strategy. The Codex
adaptor refactors onto it (behavior-preserving); standard OpenAI-compatible
adaptors dial `<base_url>/responses`. This consolidates rather than duplicates
the transport, including the handshake, HTTP fallback probe, idle timeout, and
lease lifecycle already implemented.

**Client side.** Generalize `CodexResponsesWebSocket` into a
`ResponsesWebSocket` handler that admits any selected channel which either is
Codex or has `ResponsesWebSocketEnabled`. A channel supporting neither WS nor a
usable HTTP fallback is rejected as today.

**Protocol.** Normalize client `response.create` / `response.append` frames into
upstream `response.create` events; pass `previous_response_id` through unchanged
so the upstream owns continuation state; support `generate: false` warm-up
turns; keep the sequential guarantee. The controller owns the sole downstream
read pump and processes one turn to completion before starting the next. The
session mutex protects connection and affinity state transitions; it does not
hold a lock for the lifetime of the returned response body and is not, by itself,
the turn-serialization boundary. The upstream payload never contains HTTP-only
transport fields.

Only a self-contained turn may establish a replacement upstream connection. A
turn carrying `previous_response_id` requires the same live upstream connection
that owns that id. This rule is evaluated again from the final outbound JSON
after channel parameter overrides: an override-injected continuation cannot
inherit the self-contained turn's retry permission. It also takes precedence
over `ResponsesWebSocketEnabled`; disabling new WebSocket upgrades cannot move
an existing connection-local continuation onto HTTP. The same ownership rule
applies to a self-contained turn: a runtime true-to-false flag change keeps using
the live socket rather than creating an HTTP response id beside it. At the
connection lifetime limit, or after any path has invalidated the connection
(terminal failure, truncated stream, cancellation, or early body close), a
continuation fails locally with a structured non-retryable
state-loss/connection-limit error and must be resubmitted with full input
context. Admission that detects the lifetime boundary synchronously closes the
expired socket and clears its response ids before returning that error, so a
client cannot retain an expired upstream descriptor by sending only rejected
continuations. A continuation is never sent to a replacement connection. With
new upgrades disabled, a self-contained turn may degrade to HTTP only after the
old socket is closed; that binding then remains in HTTP fallback across later
turns and flag changes so ids from the two transports cannot be mixed. The only
release is the existing relay retry boundary: after proving a self-contained
turn was not written upstream, `ResetChannelForRetry` may discard the whole
binding and let the retry establish a fresh WebSocket on a newly selected
credential. It also discards every old connection id, so this exception cannot
move a continuation.
The session records a bounded FIFO set of response ids observed on the current
connection: at most 4096 entries, 4 KiB per id, and 1 MiB of id payload in total.
The oldest id is evicted when the entry-count bound is full. A single-id or
cumulative-byte violation invalidates the connection and clears the registry;
it cannot turn an attacker-controlled upstream string into unbounded
session-lifetime memory. A continuation is admitted only when that exact id is
still owned by that connection generation; eviction therefore fails closed
locally, and the existence of an unrelated replacement socket is insufficient.
While the original connection remains live, upstream
`previous_response_not_found` and failed-turn semantics are surfaced without
gateway replay.

**Billing (reuse, do not fork).** Each `generate: true` turn is one billed unit
routed through the existing per-turn Relay lifecycle (`relayInfo.Billing`):
pre-consume → settle → refund, exactly as an HTTP Responses turn. No cross-turn
billing state is introduced; the connection only pins channel/model/credential.
A `generate: false` warm-up turn produces no model output and MUST NOT be charged
as a generation. It settles from the upstream's authoritative usage like any
other turn: reported input usage is charged and output usage is zero. No
warm-up-specific price path or cross-turn billing state is introduced.

This per-turn rule is specific to the Responses transport in this ADR. The
legacy OpenAI Realtime endpoint remains one long-lived relay invocation that may
observe several `response.done` events. That separate handler progressively
extends its `BillingSession` reservation from cumulative usage at each completed
response, but it neither consumes quota nor writes a log at those boundaries.
After the connection's read pumps stop, it returns the accumulated usage for one
final `PostWssConsumeQuota` settlement. The two WebSocket protocols therefore do
not share a cross-turn billing ledger or silently apply one another's settlement
boundary.

**Affinity and failover.** A live upstream WebSocket pins the exact channel,
model, key index, and stable credential fingerprint that authenticated it.
The model binding comes from the provider-ready outbound JSON itself (including
raw-body passthrough), not from pre-conversion mapping metadata, and comparison
is exact so two payload models cannot hide behind one mapped name.
Later turns restore that credential after normal middleware distribution and
fail closed if the key is removed, disabled, or replaced by a different
credential identity. Normal access-token refresh may preserve a provider-stable
fingerprint and does not move the live socket to another account. The binding
may be released only while the relay still proves the current turn replay-safe.

HTTP fallback is allowed before an application request frame is written when the
WebSocket upgrade is declined, when a standard channel starts with new upgrades
disabled, or when an already-bound session loses its socket and a later
self-contained turn arrives while new upgrades remain disabled. The last case
is routed through the session state machine and permanently marks that session
binding as HTTP fallback; ordinary later turns and flag re-enablement cannot
redial. A relay retry may clear the entire binding and redial only while its
attempt-state proof says the current self-contained request was never written;
this is the same replay-safe affinity-release boundary used for a rejected
WebSocket handshake. Once a WebSocket write is attempted, a transport error is
ambiguous: upstream may have accepted the turn. Write failure, first-event
timeout, client cancellation, and a stream that ends without a terminal event
therefore close the upstream connection and return a non-retryable error; they
never reconnect, switch credentials, or replay over HTTP. This implements the
project-wide written-request replay invariant.

**Lease and reader lifecycle.** Subscription-OAuth capacity is leased per turn,
not for the lifetime of an idle connection. Closing a response body before a
terminal event cancels its sole upstream reader and invalidates the connection
before releasing the lease. A later turn cannot start a second reader on the
same socket.

## Alternatives considered

- **Level A — client WebSocket, HTTP upstream only.** Terminate the client WS at
  new-api and relay each turn over HTTP. Rejected as the primary goal because it
  omits the upstream-WS latency benefit, but retained when WebSocket support is
  disabled for a standard channel and as a pre-application-write fallback when
  an attempted upgrade cannot be used.
- **Per-adaptor duplicate WebSocket sessions.** Rejected: it re-copies the Codex
  session (handshake, lease, streaming, fallback). A shared transport is the
  consolidation this project already prefers.
- **Charge warm-up as a full turn.** Rejected: `generate:false` produces no
  output; charging it as a generation misrepresents cost.
- **A second source of truth for "supports WS" (env var or global setting).**
  Rejected: WebSocket support is an upstream property, so it belongs on the
  channel, not a global toggle.

## Consequences

Regular Responses channels gain the official WebSocket transport, and the Codex
path consolidates onto one shared session. New maintenance surface: the protocol
contract (60-minute lifetime, sequential processing, `previous_response_id`,
warm-up, eviction) must track OpenAI's spec. WebSocket failover semantics differ
from HTTP: binding is credential-local, and ambiguous written turns stop rather
than fail over. Invalidating a connection also invalidates every continuation id
owned by it; the gateway fails later continuations locally instead of silently
trying those ids on a new socket. This is only effective when the deployment is
built from this codebase.

Must be monitored during rollout: per-turn billing parity with HTTP (no
double-charge, refund-once), warm-up billing correctness, and connection/lease
leaks under the 60-minute close, client disconnect, upstream close, and error
paths.

## Verification

Current focused automated coverage proves:

- WebSocket payloads remove `stream`, `stream_options`, and `background`, while
  the untouched HTTP fallback body retains those transport fields.
- A persistent session rejects channel, credential, and model changes; focused
  controller tests distinguish self-contained turns from continuations when
  applying the retry/channel pin.
- A write failure and a silent fresh connection after `response.create` do not
  fall back or retry; the lifetime test permits a self-contained reconnect and
  rejects a continuation.
- Closing a response body before its terminal event invalidates the old
  connection before a later turn can obtain another reader, and per-turn lease
  tests cover release across successful turns.
- A response id observed on an invalidated connection is rejected after a
  self-contained turn creates a replacement connection; an id emitted by the
  replacement remains usable there.
- Connection-local response-id tracking is capped at 4096 entries, 4 KiB per id,
  and 1 MiB of id payload, with FIFO eviction at the entry bound; an evicted id
  fails closed while a retained id remains usable, and a byte-bound violation
  invalidates the connection.
- The optional `Generate` DTO field preserves explicit `false` and omits an
  absent value; OpenAI-compatible routing tests cover flag-on WebSocket and
  flag-off HTTP selection. A runtime true-to-false transition keeps an existing
  live WebSocket as the sole owner. If that bound socket is later lost, the
  disabled path enters persistent HTTP fallback without a second WebSocket dial,
  including after the flag is re-enabled; a later continuation fails locally.
  A focused reset contract also proves that an explicitly replay-safe relay retry
  releases the whole fallback binding and may dial a fresh WebSocket.
- Separate legacy OpenAI Realtime aggregation coverage proves that multiple
  completed responses contribute once to the connection total. This is a scope
  guard for the distinction above, not evidence for Responses WebSocket ledger
  parity.

The following required evidence is still outstanding and must not be reported as
passed until dedicated tests exist and run:

- End-to-end ledger parity between WebSocket and HTTP for pre-consume, settle,
  refund, authoritative zero usage, and quota-clamp auditing.
- A `generate:false` billing test proving reported input usage is charged and
  output usage is zero without falling back to a local output estimate.
- Continuation tests for upstream `previous_response_not_found`, terminal-failure
  invalidation, parameter-override admission, and the remaining protocol-error
  paths failing locally without dialing or switching credentials.
- Full lifecycle/race coverage for client cancellation, upstream close,
  truncated streams, lifetime expiry, early close, nil leases, and exact
  credential restoration, followed by the relevant package race suites and the
  repository-wide test/build checks.

## Implementation phases

- **Phase 1** — Add `ResponsesWebSocketEnabled` to channel other-settings and
  extract the shared session transport, refactoring the Codex adaptor onto it
  with no behavior change.
- **Phase 2** — Standard OpenAI-compatible channel WS dial (`<base>/responses`)
  and protocol behind the flag: `previous_response_id` passthrough, warm-up
  admission, sequential enforcement, client-handler generalization.
- **Phase 3** — 60-minute lifecycle, `previous_response_not_found`/eviction
  semantics, replay-safe state-loss handling, and warm-up settlement through the
  existing billing path. The direct billing-parity evidence listed above remains
  outstanding.

## Implementation status

All three transport phases are present in code. Verification is bounded by the
focused coverage and explicit gaps above; this status does not claim that the
outstanding billing, lifecycle, race, or repository-wide gates have passed.

- **Phase 1** — `ChannelOtherSettings.ResponsesWebSocketEnabled` added; the shared
  session/transport extracted to `relay/responsesws` with a `Driver` interface;
  the Codex adaptor refactored onto it and covered by the Codex WebSocket suite.
- **Phase 2** — Standard OpenAI-compatible driver
  (`relay/channel/openai/responses_websocket_transport.go`) dials
  `<base>/responses` with Bearer/Azure auth and no capacity lease; the openai
  `DoRequest` admits a new upstream WebSocket only when the channel flag is set
  and keeps an already-live socket as the session owner if the flag changes; the
  client handler (`controller.CodexResponsesWebSocket`) admits any channel and
  pins the session per turn. Covered by openai WebSocket routing tests (flag-on
  uses WS, initially flag-off uses HTTP, a live session survives a runtime flag
  disable, and a bound session whose socket is lost remains on HTTP without
  redialing or mixing connection-owned ids).
- **Phase 3** — 60-minute lifecycle: `Session` recycles a connection at
  `MaxConnectionLifetime` (default 55 min) before the upstream cap only for a
  self-contained turn; a connection-local continuation fails closed instead of
  moving to a new connection. `previous_response_id` passes through unchanged. The
  `generate:false` warm-up flag is carried by a new pointer field
  `dto.OpenAIResponsesRequest.Generate` (explicit false forwarded, absent
  omitted), covered by a DTO round-trip test. Warm-up billing uses the existing
  usage-based per-turn settlement (input tokens; output zero), so it needed no
  new billing state. When a live connection remains available,
  `previous_response_not_found` is upstream-driven and relayed to the client.
  When the gateway has invalidated that connection, a later continuation fails
  locally and is never submitted to a replacement socket.

The shared session removes `stream`, `stream_options`, and `background` before
writing an upstream WebSocket frame. It binds the exact credential and uses one
reader per turn; early response-body close invalidates the connection before
the per-turn lease is finalized. Only a pre-write upgrade rejection degrades to
HTTP. Post-write silence and transport failures are returned with retry disabled.

Both transports return an SSE-framed response to the same `PostConsumeQuota`
lifecycle, so transport selection does not intentionally create a second billing
path. That structural reuse is not a substitute for the outstanding ledger-parity
tests listed above.
