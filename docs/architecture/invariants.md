# Architecture Invariants

These rules are release-blocking unless an explicit ADR changes them.

## Safety and data handling

- OAuth tokens, refresh tokens, authorization codes, callback URLs, API keys,
  request bodies, response bodies, and SSE content must not be emitted to
  persistent logs or ordinary runtime logs.
- Codex and Claude Code OAuth requests never use global body passthrough or
  client-supplied identity metadata.
- Client IP, forwarded-IP headers, and location metadata are removed at the
  final upstream boundary unless the configured location mode explicitly
  replaces protocol location fields with a trusted network profile. The gateway
  never fabricates `X-Forwarded-For`.
- A Root-only operation must remain Root-only in both route middleware and the
  controller.

## Retry and routing

- No retry is allowed after downstream output starts, client cancellation, or
  an explicitly selected channel.
- Retry candidates must satisfy the initial request's frozen actual group and
  compatible data policy.
- A credential rejected for account, authorization, quota, model entitlement,
  or capacity reasons cannot be selected again contrary to its corresponding
  request-local and process-local policy.
- Subscription OAuth management operations cannot mutate inference capacity,
  cooldown, recovery or persisted quarantine state. Batch inference tests never
  select subscription OAuth channels.
- Codex access-token authorization failures receive at most one refresh attempt
  per client request before any durable quarantine decision.
- Subscription OAuth requests are bounded both per credential and across the
  complete request; only active concurrency saturation participates in capacity
  cycling. A deployment may explicitly pause Claude Code's process-local
  concurrency, pacing and cooldown gate, but this cannot disable request-local
  replay safety, same-group routing boundaries or durable credential isolation.
- A request that may already have been accepted upstream is not automatically
  replayed unless an explicit, reviewed idempotency policy permits it.
- A Responses WebSocket continuation is admitted only when its exact
  `previous_response_id` was emitted by the current live upstream connection;
  replacement connections and HTTP fallback never inherit those ids.
- Every upstream Responses WebSocket has one read owner for its complete
  connection generation. A terminal cannot hand later turn events to the next
  consumer, a reader error always invalidates that generation, and a stale
  continuation must pass its pre-write liveness probe without replay or
  migration.

## Billing and persistence

- Quota computation must never overflow into a negative charge or credit.
- Responses compact must price and reserve from the final provider-ready JSON.
  Its final `Reserve` cannot reuse the trusted-wallet funding bypass; a missing
  wallet delta must be conditionally reserved before the upstream write against
  the authoritative database balance.
- Legacy OpenAI Realtime may extend its reservation from cumulative usage at
  each `response.done`, but intermediate responses do not consume quota or write
  consume logs. The connection's accumulated usage settles exactly once after
  both read pumps stop.
- A terminal policy fee settles only after its complete funding and limited-token
  quota are provably reserved. A rejected fee leaves no usage or consume-log
  entry.
- Wallet quota is a synchronous, cross-process authoritative database ledger.
  Wallet reservations use atomic conditional debits; batching is limited to
  `used_quota`, request-count, and channel-used-quota statistics.
- Every token reservation, committed debit, and rollback uses the authoritative
  database row, even while unrelated counters are batched. A limited pre-use
  reservation is conditional on available quota; usage already served upstream
  is recorded in full and may leave debt. Unlimited tokens are exempt from the
  availability condition, not from the database ledger. Redis is only an identity
  snapshot; security-sensitive token authentication reads persisted state.
- An asynchronous task has a `PENDING_SUBMIT` marker before upstream I/O. The
  marker is the only refund owner, is not normally polled until accepted, and
  remains timeout-reconcilable after a crash. Retry lease renewal invalidates a
  timeout sweep's stale CAS before another upstream write. Accepted-task funding,
  token, and reservation-marker adjustments commit in one main-database
  transaction through `model.SettleTaskQuotaAtomically`; successful completion
  recalculation uses the same operation. The committed funding delta must come
  from the locked persisted reservation, never a caller snapshot. Only a
  `changed=true` caller may write a task billing log or update user
  used-quota/request-count and channel used-quota statistics, and it must use the
  returned delta. Same-target stale, duplicate, and concurrent polling callers
  with `changed=false` must not repeat those effects. A failed-task refund claims
  its non-zero marker only from `FAILURE` and reverses the persisted funding/token
  amounts in one transaction. Submit settlement, completion recalculation, and
  failure refund share one persisted task-ledger owner. Failures preserve the
  marker for retry; cache synchronization occurs only after commit.
- Subscription reservations reject use beyond the configured total, while a
  positive delta for usage already accepted upstream is retained as debt instead
  of being dropped. Exact negative committed deltas cannot underflow usage.
- A strict pre-write reservation disables the trusted-wallet bypass; it does not
  cap authoritative final usage or reject a positive committed settlement delta.
- A multi-value configuration save is atomic: validation occurs before write,
  and all affected Option values commit or roll back together.
- Channel, token, and option database changes continue to work on SQLite,
  MySQL, and PostgreSQL.
- Background tasks inherit cancellation and bounded timeouts; management scans
  must not multiply a per-credential timeout into an unbounded operation.

## Delivery

- No merge conflict marker, unmerged Git path, or failing typecheck/build may
  enter a release artifact.
- Deployment verifies the transferred artifact, running image identity, process
  identity, public endpoint, and rollback path before reporting success.
- A behavior document must describe the code that is actually executed; stale
  deployment or security documentation is a defect, not optional cleanup.
