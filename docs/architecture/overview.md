# Architecture Overview

This document records the ownership boundaries used by the customized gateway.
It is intentionally about the current system, not a historical change log.

## Request path

```text
client -> middleware -> controller -> relay/service -> channel adaptor -> upstream
                    \-> model/settings -> database/cache
```

- `router/` owns route registration and authentication boundaries.
- `middleware/` owns request identity, initial channel selection, CORS, and the
  first response disclosure headers. It must not implement provider protocol
  behavior.
- `controller/` owns HTTP parsing, authorization, status mapping, and response
  serialization. Controllers should delegate persisted state and provider work.
- `service/` owns cross-channel business policy: retry eligibility, credential
  capacity, data-policy boundaries, credential refresh, notification, and HTTP
  client construction.
- `model/` owns persistence, transactions, cache invalidation, and database
  compatibility across SQLite, MySQL, and PostgreSQL.
- `relay/` owns protocol conversion, request sanitization, streaming, billing
  integration, and provider adaptors. A provider-specific rule belongs under
  `relay/channel/<provider>/` unless it is genuinely protocol-neutral.

## Customized feature domains

### Responses WebSocket transport

- `ChannelOtherSettings.ResponsesWebSocketEnabled` is the sole persisted opt-in
  for a standard OpenAI-compatible upstream WebSocket. Codex WebSocket support is
  implied by its channel type and does not add a second setting. For a standard
  channel, the setting controls only new upstream upgrades. If it is disabled
  while a downstream session still owns a live upstream WebSocket, that socket
  remains the single transport owner until invalidation or session end; later
  turns do not silently create HTTP-owned response ids beside it.
- `controller/codex_responses_websocket.go` owns the downstream upgrade, the sole
  client read pump, frame normalization, structured downstream errors, and the
  serial turn-processing loop. It restores the exact channel/key-index/credential
  binding after middleware distribution. That controller loop, not the session
  mutex alone, ensures one response is processed to completion at a time.
- `relay/responsesws/session.go` owns the upstream connection, its lifetime,
  transport-field removal, pre-write HTTP fallback decision, and live
  channel/model/credential binding. Model affinity is bound to the exact
  provider-ready JSON, including passthrough bodies, rather than mapping
  metadata. It also owns a bounded FIFO set of response ids emitted by the
  current connection generation: at most 4096 entries, 4 KiB per id, and 1 MiB
  of id payload. Entry eviction fails closed; a per-id or cumulative-byte
  violation invalidates the socket and clears the registry. A continuation must
  match that set, so a replacement socket cannot inherit an old id.
  Its mutex protects connection and affinity
  state transitions but is released when the response body is returned. Early
  body close or an ambiguous/truncated stream invalidates the connection before
  another reader can start.
- The normal Relay lifecycle owns each turn's retry and billing state; no billing
  or retry ledger persists in the WebSocket session. A subscription-OAuth lease
  remains per-turn, while an ordinary API-key driver has no lease. The controller
  may release session affinity only after the shared retry decision proves the
  current turn replay-safe.

A `previous_response_id` is usable only while the owning upstream connection is
still live. Once that connection is invalidated or reaches its lifetime, a
continuation fails locally and non-retryably; the admission check also closes an
expired socket and clears its continuation registry instead of waiting for a
later self-contained turn to perform cleanup. Only a self-contained turn may
establish a replacement connection. When new WebSocket admission has been
disabled, even a self-contained turn cannot redial: after closing an expired or
invalid socket, the session explicitly degrades that binding to HTTP and keeps
continuations fail-closed. Ordinary later turns and flag changes retain this
fallback. Only the existing replay-safe retry boundary may call
`ResetChannelForRetry`, discard the entire binding plus its response ids, and let
the newly selected credential establish a fresh WebSocket; a written request or
continuation can never use that escape hatch.

### Responses compact billing

- `relay/responses_handler.go` owns compact attempt pricing. The provider-ready
  JSON after field filtering, model mapping, disabled-field removal, and channel
  parameter overrides is the sole source for `BillingRequestInput`, the billed
  and upstream model names, the prompt estimate, and the frozen tiered snapshot.
- Each mapped attempt reserves against that frozen state before sending its
  request. When the attempt returns, client-level pricing fields are restored;
  settlement still occurs against the successful attempt's frozen state. This
  prevents the source request model from becoming a second pricing authority.
- `BillingSession.Reserve` treats that final target as a real pre-write
  reservation even when the initial estimate used the trusted-wallet bypass. It
  reserves the missing token and funding deltas without reapplying trust;
  supplemental wallet funding uses `model.DecreaseUserQuotaIfEnough` against the
  authoritative database balance under every batch setting. This strictness
  controls the trust bypass only: authoritative usage may still exceed an
  estimate, and its positive settlement delta remains committed debt.

### Legacy OpenAI Realtime billing

- `relay/channel/openai/relay_realtime.go` owns one connection-local usage
  accumulator. At each `response.done`, authoritative upstream usage replaces
  that response's local fallback and the handler extends the existing
  `BillingSession` reservation to the cumulative quota. This is an availability
  gate, not an intermediate consumption or logging boundary.
- On either socket's termination, the handler closes both connections, joins
  both read pumps, drains their usage events, and returns one final cumulative
  usage value. `relay/websocket.go` then calls `PostWssConsumeQuota` once, so the
  connection has progressive reservation but one settlement and one consume log.
- This legacy connection-level lifecycle is separate from the official Responses
  WebSocket transport, whose controller sends every turn through an independent
  Relay billing session.

### Billing sessions, task refunds, and violation fees

- `service.BillingSession` is the single owner of one request's funding-source
  reservation, token reservation, settlement, and refund state. A refunded
  session cannot settle, and a committed session cannot refund.
- The session's general `Refund` guard is process-local, not a durable refund
  ledger. It marks the in-memory session before dispatching asynchronous funding
  and token restores; there is currently no persisted intent or shared
  idempotency key for crash recovery or retrying a partial restore.
- A terminal provider-policy fee reuses that session and must reserve the full
  fee before settlement. The trusted-user bypass is not valid for policy fees;
  settlement is zero or negative delta only, so no fee can overdraw a wallet.
- `model.DecreaseUserQuotaIfEnough` owns the cross-database conditional wallet
  debit. Wallet quota debits and credits are applied synchronously to the
  database, so that row remains authoritative across processes under every
  batch setting. The batch updater is limited to `used_quota`, request-count,
  and channel-used-quota statistics. Funding preference may still select or
  fall back to a transactionally reserved subscription.
- `model.ReserveTokenQuota` is the pre-use authorization boundary: a limited
  token is reserved only when its persisted balance covers the complete amount,
  while an unlimited token is exempt from that availability condition. Once
  upstream usage has happened, `model.ConsumeTokenQuota` records the complete
  positive settlement, legacy, or asynchronous-task delta, including debt;
  `model.RestoreTokenQuota` reverses reservations and negative deltas. All three
  operations bypass the batch updater for every token, so the database remains
  the cross-process ledger under every global batch setting.
- Redis is a best-effort token identity snapshot, not an authorization source.
  Normal token authentication reads persisted status, expiry, and quota, and
  read-only authentication reads its persisted disabled-state gate. Token
  mutations synchronously invalidate the snapshot. The trusted-user optimization
  applies only to funding and never skips token reservation.
- Rejected or saturated fees do not update user/channel usage and do not create
  a consume log; they use request-correlated backend warnings. Successful fees
  alone own the consume-log entry and its admin-only billing audit fields.
- Asynchronous tasks persist a `PENDING_SUBMIT` billing marker before upstream
  I/O. That marker owns rollback from then on and is excluded from normal polling
  until the accepted response transitions it to `SUBMITTED`. Post-acceptance
  wallet/subscription settlement, token reconciliation, and the marker's
  separate funding/token reservation snapshot commit in one main-database
  transaction through `model.SettleTaskQuotaAtomically`. Successful completion
  recalculation uses that same operation. It derives and returns the committed
  funding delta from the locked persisted reservation, never from a caller's
  task snapshot. Its `changed` result is the only gate for a task billing log and
  user used-quota/request-count or channel used-quota statistics; same-target
  stale, duplicate, and concurrent callers receiving `changed=false` emit none
  of them, while a different-target caller records only the returned delta.
  `model.RefundTaskQuotaAtomically` later clears a non-zero marker only from
  `FAILURE` and reverses exactly that snapshot in the same transaction. Submit
  settlement, completion recalculation, and failure refund therefore share one
  persisted task-ledger owner. Rollback preserves the marker for reconciliation;
  a committed zero marker makes retries no-ops. A deleted historical token is
  logged but does not prevent returning the user's funding.
- Subscription pre-use reservation remains capped by `AmountTotal`. Positive
  usage accepted upstream is instead a committed debit and may leave
  `AmountUsed` above the cap, preventing the accounting delta from disappearing;
  committed negative deltas reverse an exact prior debit.

### Subscription OAuth channels

Codex and Claude Code share the subscription OAuth policy surface:

- `service/subscription_oauth_capacity.go` owns process-local credential slots,
  pacing, cooldowns, and recovery probes.
- `service/subscription_oauth_error_policy.go` owns provider-neutral OAuth error
  classification, including the distinction between permanent account failures,
  temporary burst limits, and resettable subscription usage windows.
- `service/codex_wham_usage.go` owns Codex Wham usage requests and the same
  short-lived evidence role for ambiguous inference 429 responses.
- `service/subscription_oauth_usage_cache.go` owns bounded Codex Wham evidence
  caching; it never controls credential routing or cooldown state.
- `service/subscription_oauth_retry.go` owns request-local retry decisions and
  per-credential and whole-request attempt budgets. It distinguishes active
  concurrency replay from known cooldown exclusion and request-local model
  capacity.
- `service/retry_data_policy.go` owns which channels and credentials may be
  selected for a retry.
- `relay/channel/codex/` and `relay/channel/claude/` own upstream protocol
  construction, credentials, and response-body lease release. The Claude
  adaptor also owns the deployment-scoped switch that can bypass its local
  capacity gate without bypassing the shared retry/error policy.
- `service/codex_credential_refresh.go` owns serialized, CAS-protected Codex
  credential rotation. The relay may request one refresh-first transition; it
  does not write credentials itself.

The request-local attempt record must have one owner. Gin context may expose a
lease to an adaptor, but must not become a second independent retry ledger.

### Channel management and model catalogues

- `controller/channel_query.go`, `channel_models.go`, `channel_multi_key.go`,
  and `channel_ollama.go` are the HTTP-facing channel-management slices.
- `controller/channel_model_catalog.go` normalizes model catalogues and shared
  fetch headers.
- `controller/channel_upstream_update*.go` separates one-channel checks,
  administrator APIs, and scheduled-task orchestration.
- `model/channel_model_metadata.go` persists verified upstream capability
  metadata; `model/official_model_metadata.go` provides public-specification
  fallbacks.

### Model pricing sync

- `controller/ratio_sync.go` owns the administrator-facing comparison and
  explicit sync workflow.
- `controller/official_api_price_catalog.go` owns read-only, reviewed OpenAI
  and Anthropic public API token-price catalogues. It does not query or use
  subscription OAuth credentials.
- Model-pricing Options remain the only persisted price state; a catalogue
  source only proposes differences until an administrator applies them.
- `ModelPricingInputMode` is UI-state within that pricing domain. It preserves
  the token-price editor view but never changes runtime billing.

### Privacy and upstream network profile

- `relay/common/location_privacy.go` is the final request-body privacy filter.
- `common/upstream_location.go` owns configured and discovered host/egress
  profiles.
- `service/upstream_location.go` discovers profiles for distinct channel proxy
  endpoints.
- `controller/option.go` exposes Root-only runtime profile data and mode changes.

Location discovery is observability/configuration support. It must not itself
decide whether client location reaches an upstream; the final relay filter owns
that decision.

### Operations and deployment

- Per-target `bin/deploy-*.sh` files own only target identity and deployment
  parameters.
- `bin/deploy-common.sh` owns local build, artifact transfer, public endpoint
  verification, and orchestration.
- `bin/deploy-remote.sh` owns target-local image activation, backup, rollback,
  and container health verification.

The deployment workflow has two intentional execution environments. Do not put
target-specific configuration into the common script or local build credentials
into the remote script.

## Configuration ownership

- Environment variables provide startup defaults and deployment-specific secrets.
- Database Options provide Root-managed runtime configuration.
- Channel settings provide per-upstream behavior.
- Request headers/body provide untrusted client input and never override OAuth
  identity, privacy, billing, or retry-safety invariants.

When a setting exists in more than one layer, the precedence and hot-reload
behavior must be documented next to the setting and tested.

Performance-metrics runtime configuration is published as one immutable
`config.AtomicConfig` snapshot. Record operations load one snapshot for their
enabled gate and bucket width. The flush loop has two explicit linearization
points: the snapshot loaded before sleeping selects the next scheduling
interval, while one snapshot loaded after waking supplies the enabled gate,
bucket width, and retention for that flush. Readers never assemble one
operation's decisions from independently mutating fields. Domain validation
rejects intervals or retention values that cannot be represented as
`time.Duration`, and duration accessors saturate unvalidated values as a final
overflow guard. Option bulk updates
and periodic database sync group fields by registered module before publishing;
an invalid managed module keeps its prior snapshot and OptionMap values rather
than exposing a partially parsed configuration.
