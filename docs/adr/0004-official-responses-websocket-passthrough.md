# ADR 0004: Official Responses WebSocket passthrough

Status: Implemented transport; targeted verification remains incomplete. Warm-up
(`generate:false`) turns use the ordinary usage-based settlement path, so no
warm-up-specific billing state was added. A live upstream WebSocket pins one
channel and credential; the model binding may be replaced by a self-contained
turn (which rebinds the whole session), while a continuation stays pinned to the
connection that owns its `previous_response_id`. A turn that may have reached
upstream is never transparently replayed. Dedicated WebSocket-vs-HTTP
ledger-parity and warm-up billing tests remain release follow-up and are not
inferred merely from shared code paths.

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
A **self-contained model switch is legal**: the official protocol scopes a
connection to an account, not a model, and Codex clients switch models on one
client connection. The controller detects the switch after frame normalization
and — **before channel distribution** — clears its internal channel pin (an
externally requested specific-channel pin is untouched) and resets the session
binding via `ResetChannelForRetry`, so the new model re-enters ordinary channel
selection and dials a fresh upstream connection. The session relaxes only the
self-contained model check as a defense for switches the controller cannot see
(e.g. a parameter-override-injected model); a cross-model **continuation stays
fail-closed** (409) because its `previous_response_id` is owned by the old
model's connection.
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

**Idle-connection recycling and reused-turn first-event bound.** A pooled
upstream connection can be silently idle-closed by the upstream or an
intermediary between sequential turns. The first turn on a fresh connection is
protected by the handshake and a first-event probe, but a reused turn has
neither: it writes `response.create` into a possibly half-open socket — where the
write is buffered locally and appears to succeed — and then blocks its reader for
the full idle timeout (minutes). Two bounds close this gap without a replay:
(1) a **self-contained** turn whose connection is idle beyond
`ReuseIdleReconnectThreshold` (default **30s**, measured from the previous turn's
stream end; production protocol logs show the upstream pings every ~20s and
closes after ~2-3 missed pings, so 30s recycles before the upstream declares the
reader-less idle connection dead) recycles and re-dials **before** the
application frame is written,
which is replay-safe. A continuation is **never** recycled here: its
`previous_response_id` is owned by this exact connection and must not move to a
replacement socket, so it keeps the connection and — if that connection is
already dead — is failed fast by bound (2). (2) A reused turn's first upstream
event is bounded by `FirstEventTimeout` (**30s**) rather than the idle timeout, so
a connection that dies within the idle window fails in ~30s — the request is
already written and is therefore not replayed; the client resends full input
context — instead of appearing to hang for minutes. Both bounds are `var`s so
tests can shorten them.

**Downstream keepalive.** The client↔gateway WebSocket has the mirror-image
problem: between turns it carries no traffic, so an intermediary idle timeout
(reverse proxies and NAT gateways commonly use ~60s) can silently kill it; the
client's next message is then written into a dead socket and hangs until a
transport-level timeout (observed as multi-minute "thinking" stalls with zero
gateway activity). The controller therefore runs a keepalive pump per downstream
connection: a server→client WebSocket ping every 25 seconds (below the common
60s intermediary bound, with margin), written via `WriteControl` — documented
concurrency-safe alongside the SSE writer — with the 1-second control-frame
bound gorilla itself uses so a slow client cannot stall the pump. Clients answer
pongs in the protocol stack; no client change is needed. A failed ping cancels
the connection context so a dead client promptly releases the reader and any
in-flight turn instead of holding session resources.

**Turn-failure terminal shape.** A failed in-flight turn is surfaced to the
client as the protocol's standard terminal event chain, not as a bare
`{"type":"error"}` frame. Production evidence: Codex clients ignore the generic
error frame and wait indefinitely for a response-associated terminal (observed
hangs after a gateway 409 and 502 that were resolved only by the user sending
another message). Two cases, both carrying the classified error object (type,
code, retry_after):
- **Stream ended without a terminal** (partial output already forwarded): the
  writer tracks the response id observed in forwarded events and emits a single
  `response.failed` with that **real id**, so the terminal matches the in-flight
  response the client has already registered.
- **Relay HTTP error with zero streamed events**: no response lifecycle exists
  on the client yet, so the gateway synthesizes a matching pair —
  `response.created` (`status:"in_progress"`) followed by `response.failed` —
  under one id (the observed id when available, else the fixed
  `resp_gateway_failed`), satisfying clients that only match terminals against a
  registered response lifecycle.
A single `response.failed` with an unrelated synthetic id was considered and
rejected: a client matching terminals by id would discard it and hang again.
Pre-turn rejections (malformed frame, append-before-create, continuation
admission) keep the plain error frame; they have not exhibited the hang and
remain an observation item.

**Idle-period connection ownership and continuation lease.** Production logs
proved that an upstream connection idle for 84-147s dies from "keepalive ping
timeout": between turns nothing reads the connection, so gorilla never processes
the upstream's ~20s pings and no pong is ever sent. The failure-terminal chain
above resolves the *client* hang, but a continuation arriving after such an idle
period still burns a turn (fast-failure + full-context resend). The goal is
stronger than "keep the connection alive": a continuation must find the
connection **provably live before its frame is written**, and a dead or expired
connection must fail immediately — never wait, never migrate.

- **Single reader per connection.** `responsesws.Session` is the sole owner of
  connection state. Every established upstream connection runs one long-lived
  read loop that performs ALL `ReadMessage` calls for the connection's lifetime.
  During an active turn it routes business events to that turn's consumer;
  while idle it keeps processing control frames (answering upstream pings), so
  the connection no longer dies from reader absence. The read loop NEVER sets a
  read deadline — a read-deadline timeout corrupts a gorilla connection, and
  with a shared loop that corruption would outlive the turn that caused it. All
  turn-level timeouts (`FirstEventTimeout`, `idleTimeout`) move to the consumer
  side as `select`+timer.
- **Active health checking.** While idle the session pings the upstream every
  20 seconds (matching the upstream's own cadence); a ping not answered by a
  pong within 10 seconds marks the connection dead and closes it (which also
  terminates the read loop). With a permanent read loop the pong handler is
  reliable, which is what made ad-hoc probing unsafe before.
- **Continuation idle lease: 10 minutes.** A live connection is retained for
  continuations for at most 10 idle minutes, then proactively closed so
  abandoned client sessions cannot pin upstream sockets and goroutines
  indefinitely.
- **Pre-write liveness check.** A continuation is admitted only when the lease
  is unexpired and the connection is marked live; otherwise the gateway fails
  the turn immediately with the associated `response.failed` — the frame is
  never written upstream, never replayed, never migrated. This removes the
  "request may already be executing" ambiguity for the common idle-death case;
  a race where the upstream dies between health checks still falls back to the
  existing write-failure/terminal-chain path.
- **Unchanged.** Self-contained turns keep the existing 30s idle-reconnect
  policy (observation item: with keepalive working, D-triggered reconnects
  become redundant ~300ms; revisit with production frequency data). Idle-period
  business frames remain a protocol violation that invalidates the connection.
  The HTTP fallback binding has no upstream WebSocket and is unaffected.
  Connection, model, channel, and continuation state remain solely owned by
  `Session` — no second pool or state table.
- **Rollback** is by server image rollback; no permanent dual implementation is
  kept. Regression tests must cover idle ping/pong keepalive, lease expiry,
  read-loop termination, active-turn routing, and the continuation
  never-migrates invariant.

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
- **WebSocket ping/pong liveness probe before reusing a connection.** Rejected as
  implemented: reading the pong requires a deadlined `ReadMessage`, and a
  read-deadline timeout corrupts the gorilla connection, so an ad-hoc probe would
  break the very live connection it checks. A dependable probe needs a single
  long-lived read loop that demultiplexes control and data frames — a connection
  state-machine change not warranted for this fix. The idle-recycle + first-event
  bound above achieves the same protection without that rewrite. This does not
  prevent observing inbound upstream pings during an active read: the connection
  temporarily wraps Gorilla's default ping handler with identical one-second
  pong and error semantics and records only whether that pong succeeded.

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
- A persistent session rejects channel and credential changes; a self-contained
  model change rebinds (new channel selection, fresh upstream connection) while a
  cross-model continuation is rejected fail-closed; focused
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
  locally and is never submitted to a replacement socket. Reused connections are
  additionally guarded against silent idle-close: a **self-contained** turn whose
  connection is idle beyond `ReuseIdleReconnectThreshold` (default 30s, tuned
  from observed ~20s upstream ping cadence) recycles
  and re-dials before the application frame is written (replay-safe), while a
  continuation is never recycled (its `previous_response_id` is connection-owned)
  and instead relies on the fail-fast bound; a reused turn's first upstream event
  is bounded by `FirstEventTimeout` (30s) rather than the multi-minute idle
  timeout, so a connection that dies within the idle window fails fast instead of
  hanging. Both are covered by targeted reconnect / continuation-kept / fail-fast
  tests. A temporary `responses websocket timing` diagnostic (handshake,
  first-event, reused conn_age/idle_gap/write→first-event) is in place to confirm
  these bounds in production and to tune the 30s threshold. Temporary protocol
  diagnostics additionally record inbound-ping/pong outcomes, first/last event
  types, terminal presence, stream-end errors, and whether the downstream error
  frame was written. They never record request, response, SSE, or control-frame
  payloads. All temporary diagnostics are to be removed once production behavior
  is verified.

The shared session removes `stream`, `stream_options`, and `background` before
writing an upstream WebSocket frame. It binds the exact credential and uses one
reader per turn; early response-body close invalidates the connection before
the per-turn lease is finalized. Only a pre-write upgrade rejection degrades to
HTTP. Post-write silence and transport failures are returned with retry disabled.

Both transports return an SSE-framed response to the same `PostConsumeQuota`
lifecycle, so transport selection does not intentionally create a second billing
path. That structural reuse is not a substitute for the outstanding ledger-parity
tests listed above.
