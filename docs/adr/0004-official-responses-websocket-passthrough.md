# ADR 0004: Official Responses WebSocket passthrough

Status: Implemented (Phases 1–3). Warm-up (`generate:false`) turns are billed on
their reported usage (input tokens; output is zero), so no warm-up-specific
billing code was needed. A live upstream WebSocket pins one channel, model, and
credential. A turn that may have reached upstream is never transparently
replayed.

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
turns; keep the sequential guarantee (one downstream read pump and the session
mutex serialize turns). The upstream payload never contains HTTP-only transport
fields. At the connection lifetime limit, a self-contained turn may establish a
new upstream connection. A continuation that carries `previous_response_id`
instead fails with a structured connection-limit error and requires the client
to reconnect and submit full input context; connection-local state must not be
silently discarded. `previous_response_not_found` and failed-turn eviction are
surfaced from upstream to the client verbatim.

**Billing (reuse, do not fork).** Each `generate: true` turn is one billed unit
routed through the existing per-turn Relay lifecycle (`relayInfo.Billing`):
pre-consume → settle → refund, exactly as an HTTP Responses turn. No cross-turn
billing state is introduced; the connection only pins channel/model/credential.
A `generate: false` warm-up turn produces no model output and MUST NOT be charged
as a generation — it is billed on primed input only (or zero, to be fixed in the
billing phase), via a warm-up-specific price path validated by a parity test.

**Affinity and failover.** A live upstream WebSocket pins the exact channel,
model, key index, and stable credential fingerprint that authenticated it.
Later turns restore that credential after normal middleware distribution and
fail closed if the key was rotated, removed, or disabled. The binding may be
released only while the relay still proves the current turn replay-safe.

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
  omits the upstream-WS latency benefit, but retained as the automatic fallback
  when the upstream does not support WS.
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
than fail over. This is only effective when the deployment is built from this
codebase.

Must be monitored during rollout: per-turn billing parity with HTTP (no
double-charge, refund-once), warm-up billing correctness, and connection/lease
leaks under the 60-minute close, client disconnect, upstream close, and error
paths.

## Verification

- Protocol table tests: a `response.create` turn with HTTP-only fields removed;
  a `previous_response_id` continuation; a `generate:false` warm-up (asserting
  no output charge); `previous_response_not_found`; failed-turn cache eviction;
  self-contained reconnect and continuation rejection at the lifetime limit.
- Billing parity: a WebSocket turn and an HTTP turn produce identical ledger
  entries (pre-consume, settle, refund) for the same simulated upstream outcome;
  a warm-up turn charges input-only (or zero).
- Lease/connection lifecycle: released on terminal completion, upstream close,
  client disconnect, early body close, lifetime limit, and error — no leak,
  double-release, or concurrent reader.
- Affinity: a multi-key channel keeps the same key index and fingerprint for all
  turns; credential rotation/removal fails closed rather than using a different
  key on the existing socket.
- Replay safety: pre-write upgrade rejection may fall back to HTTP; write error,
  first-event timeout, and truncated post-write streams never replay.
- Regression: existing Codex WebSocket tests and HTTP relay/billing suites pass
  unchanged.

## Implementation phases

- **Phase 1** — Add `ResponsesWebSocketEnabled` to channel other-settings and
  extract the shared session transport, refactoring the Codex adaptor onto it
  with no behavior change. Build/test-verified against the existing Codex WS
  tests.
- **Phase 2** — Standard OpenAI-compatible channel WS dial (`<base>/responses`)
  and protocol behind the flag: `previous_response_id` passthrough, warm-up
  admission, sequential enforcement, client-handler generalization.
- **Phase 3** — 60-minute lifecycle, `previous_response_not_found`/eviction
  semantics, and the warm-up billing path, gated on the billing-parity tests
  above.

Each phase is independently build/test-verified; the billing-bearing phases do
not merge until the parity tests pass.

## Implementation status

All three phases are implemented and build/test-verified.

- **Phase 1** — `ChannelOtherSettings.ResponsesWebSocketEnabled` added; the shared
  session/transport extracted to `relay/responsesws` with a `Driver` interface;
  the Codex adaptor refactored onto it with no behavior change (existing Codex
  WebSocket tests unchanged and green).
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
  new billing code. `previous_response_not_found` and failed-turn eviction are
  upstream-driven and relayed to the client verbatim.

The shared session removes `stream`, `stream_options`, and `background` before
writing an upstream WebSocket frame. It binds the exact credential and uses one
reader per turn; early response-body close invalidates the connection before
the per-turn lease is finalized. Only a pre-write upgrade rejection degrades to
HTTP. Post-write silence and transport failures are returned with retry disabled.

Full billing parity (WS turn vs HTTP turn) follows from both transports returning
the same SSE-framed response to the same `PostConsumeQuota` lifecycle; transport
selection does not create a second billing path.
