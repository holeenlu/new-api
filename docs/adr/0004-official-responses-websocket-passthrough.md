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
the flag (their WS support is implied by type).

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
an existing connection-local continuation onto HTTP. At the connection lifetime
limit, or after any path has invalidated the connection (terminal failure,
truncated stream, cancellation, or early body close), a continuation fails
locally with a structured non-retryable state-loss/connection-limit error and
must be resubmitted with full input context. It is never sent to a replacement
connection. While the original connection remains live, upstream
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

**Affinity and failover.** A live upstream WebSocket pins the exact channel,
model, key index, and stable credential fingerprint that authenticated it.
Later turns restore that credential after normal middleware distribution and
fail closed if the key is removed, disabled, or replaced by a different
credential identity. Normal access-token refresh may preserve a provider-stable
fingerprint and does not move the live socket to another account. The binding
may be released only while the relay still proves the current turn replay-safe.

HTTP fallback is allowed only when the WebSocket upgrade is declined before an
application request frame is written. Once a write is attempted, a transport
error is ambiguous: upstream may have accepted the turn. Write failure, first
event timeout, client cancellation, and a stream that ends without a terminal
event therefore close the upstream connection and return a non-retryable error;
they never reconnect, switch credentials, or replay over HTTP. This implements
the project-wide written-request replay invariant.

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
- The optional `Generate` DTO field preserves explicit `false` and omits an
  absent value; OpenAI-compatible routing tests cover flag-on WebSocket and
  flag-off HTTP selection.

The following required evidence is still outstanding and must not be reported as
passed until dedicated tests exist and run:

- End-to-end ledger parity between WebSocket and HTTP for pre-consume, settle,
  refund, authoritative zero usage, and quota-clamp auditing.
- A `generate:false` billing test proving reported input usage is charged and
  output usage is zero without falling back to a local output estimate.
- Continuation tests for upstream `previous_response_not_found`, terminal-failure
  invalidation, flag-disabled and parameter-override admission, and every
  no-live-connection path failing locally without dialing or switching
  credentials.
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
  `DoRequest` routes Responses turns to the session only when the channel flag is
  set; the client handler (`controller.CodexResponsesWebSocket`) admits any
  channel and pins the session per turn. Covered by openai WebSocket routing
  tests (flag-on uses WS, flag-off uses HTTP).
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
