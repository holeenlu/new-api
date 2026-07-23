# ADR 0007: Responses HTTP path hardening

Status: Implemented.

## Context

The Responses WebSocket investigation (ADR 0004) proved several failure classes
in production: connections dying unobserved, failures presented in shapes strict
clients ignore, and metadata events misclassified as semantic output. A
systematic review of the Responses HTTP path — the native SSE handler, the
Responses→Chat conversion handlers, and the shared transport layer — found the
analogous defects. Three review rounds converged on the fixes below; each item
names its owner so this does not become a second source of truth.

## Decision

**Client-context binding (owner: `relay/channel/api_request.go`).**
`DoApiRequest` and `DoFormRequest` bind the upstream request to
`c.Request.Context()`. A client that disconnects before upstream headers arrive
(or during a non-stream read) cancels the upstream call instead of letting it
generate — and bill — unobserved. Cancellation classifies as 499/SkipRetry via
the existing transport-error classifiers. `DoTaskApiRequest` is intentionally
unchanged (short-lived task submission with its own reconciliation).

**Terminal enforcement in Responses→Chat conversion (owner:
`relay/channel/openai/chat_via_responses.go`).** Both conversion handlers treat
top-level `error` as a terminal failure alongside `response.failed` /
`response.error`, and preserve upstream classification (status derivation via
`responsesStreamErrorStatus`, cooldown via `ParseUpstreamRetryDelay`).

- The buffered (non-stream) handler no longer fabricates a `completed` response
  when the stream ends without a terminal: it fails with 502 (nothing was
  written downstream; the written-request guard still governs whether the relay
  may retry). It also carries its own dual idle guard because it reads the
  upstream directly rather than through the shared scanner.
- The streaming handler enforces a terminal after the scanner ends. A committed
  stream never receives a fabricated normal ending (finalize + `[DONE]`) over a
  truncated turn, and never receives a plain JSON error appended to live SSE.
  Instead the failure is emitted in the TARGET protocol's own terminal shape:
  OpenAI format gets the official mid-stream `data: {"error":...}` chunk;
  Claude format gets the protocol's `event: error`. Formats without a standard
  in-stream error shape (Gemini) end truncated — the missing normal termination
  is the failure signal. Committed failures are recorded via
  `MarkCommittedUpstreamError` and usage settles on received output.

**Preflight metadata classification (owner:
`relay/channel/openai/relay_responses.go`, shared predicate in
`relay/responsesws`).** `codex.rate_limits` arrives first on every Codex turn;
committing the preflight on it made subscription-OAuth failover unreachable.
Connection-scoped extensions (any valid non-`response.*`, non-`error` type, per
`responsesws.IsConnectionScopedEventType` — a prefix rule, deliberately not a
whitelist) are buffered without committing; a typeless event fails the
preflight closed while failover is still safe; unknown `response.*` events
remain real output and commit. An oversized metadata buffer commits and returns
without re-sending the overflowing event.

**Scanner dual idle bound (owner: `relay/helper/stream_scanner.go`).** Any
upstream line — SSE comments included, they are legitimate keepalive — refreshes
`STREAMING_TIMEOUT`; only accepted data events refresh a second window of twice
that. A comment-only stream can therefore no longer hold a request open
indefinitely, while comment-keepalive bridging a long reasoning gap keeps
working.

**Streaming response-header timeout (owner: `common/constants.go`,
`STREAM_RESPONSE_HEADER_TIMEOUT`, default 60s, 0 disables).** Applied ONLY to
streaming `RelayModeResponses` requests on ordinary channels: a streaming
Responses upstream returns headers immediately, and with `RELAY_TIMEOUT`
defaulting to 0 a stalled one previously waited forever. It is deliberately NOT
applied to other streaming modes — some chat upstreams delay headers until the
first token, and long prefill can exceed any fixed bound; widening the scope
requires per-protocol evaluation and an ADR revision. Non-stream requests rely
on client-context cancellation instead (their headers legitimately arrive only
after full generation).

**Non-stream body bounds and integrity (owner:
`relay/channel/openai/relay_responses.go`).** All three non-stream read sites
(native, compaction, Responses→Chat) read through a 64 MiB bound (larger than
the WebSocket per-event 16 MiB because a full response can carry several base64
image outputs). A "success" body with no status, no output, and no usage (e.g.
`{}`) is rejected as 502 rather than forwarded as a zero-usage success; a body
carrying usage settles billing and passes.

## Explicitly not done

- The bare `{"type":"error"}` frame in the NATIVE Responses SSE handler stays:
  HTTP has connection close as a final termination signal, so the WebSocket
  hang evidence does not transfer; replacing the shape without HTTP client
  evidence or an official schema is unjustified. Observation item.
- The event-classification predicates are only partially unified
  (`IsConnectionScopedEventType` is shared; terminal lists still exist in the
  WS session, the native HTTP handler, and the conversion handlers, and the
  shared predicate lives in the WS package). Moving them to a transport-neutral
  Responses protocol module is approved follow-up (see
  `docs/architecture/health.md`), not done here because package restructuring
  is its own architectural change.

## Consequences

New global configuration: `STREAM_RESPONSE_HEADER_TIMEOUT` (single owner:
`common/constants.go`). No new request-lifecycle state. Regression tests cover:
cancellation propagation at the real `DoApiRequest` layer, conversion-path
terminal enforcement (both handlers, both failure kinds, OpenAI and Claude
shapes), metadata-vs-commit preflight behavior including the oversized-buffer
replay, dual idle bounds (comment-only termination and comment-bridged gaps),
the scoped header timeout, and non-stream bounds/integrity. Known test debt:
the dual-bound scanner tests use wall-clock sleeps, which conflicts with the
project test guideline; converting the scanner to an injectable clock is listed
follow-up.
