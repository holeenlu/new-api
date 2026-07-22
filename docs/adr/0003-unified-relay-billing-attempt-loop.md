# ADR 0003: Unified relay billing attempt loop

Status: Partially implemented. Phase 1 (shared per-attempt channel-error
accounting) and the independent billing safety fixes described below are done.
Phase 2 (a single shared attempt loop owning the billing settle) is deferred —
see "Implementation status" below.

## Context

The relay attempt loop — select a channel, track the retry attempt, apply relay
data-policy headers, invoke the upstream, classify channel errors, and run the
pre-consume / settle / refund billing lifecycle — is currently implemented more
than once:

- `controller/relay.go` `Relay()` (~254 lines) is the canonical path. It runs an
  ordinary retry loop and a separate subscription-OAuth retry loop, sharing the
  primitives `getChannel`, `trackRetryAttempt`, `addUsedChannel`,
  `ApplyRelayDataPolicyHeaders`, `clearStaleRetryAfter`, and
  `processChannelError`, plus the `relayInfo.Billing` pre-consume/settle/refund
  lifecycle.
- `controller/codex_alpha_search.go` `CodexAlphaSearch()` (~230 lines)
  reimplements the same attempt loop against the same primitives, with its own
  `service.SettleBilling` / `Billing.Refund` settlement and its own
  subscription-OAuth failover handling.

Both loops must uphold the same billing invariants (a failed attempt refunds
exactly once; a settled attempt never double-charges; pre-consume rejects a
saturated oversized quota; subscription-OAuth leases release on every exit
path). Because the logic is duplicated, a fix or invariant applied to one loop
can silently miss the other. The subscription-OAuth/Codex surface is still
growing (WebSocket turns already route through `Relay`; alpha-search does not),
so each new feature widens the drift.

Non-negotiable constraints:

- Billing invariants in `AGENTS.md` (single refund, no double-charge, saturation
  safety, lease release) must hold identically on every path.
- The alpha-search endpoint has a different request/response contract from
  `/v1/responses` and cannot simply call `Relay()` verbatim.
- Except for the explicit billing safety fixes below, ordinary relay behavior
  for non-Codex traffic must not change.

Terminal provider-policy violations are a billing exception to the ordinary
"failure means full refund" rule. The configured fee must replace the request's
normal charge without creating a refund/recharge race.

Three adjacent paths expose the same ownership problem. Responses compact
pricing is not final until the provider-ready JSON exists, so its final reserve
cannot inherit an earlier trusted-wallet funding bypass. A legacy OpenAI
Realtime connection can contain several `response.done` events, so it needs
progressive availability checks without treating each event as a separate
settlement. An asynchronous task failure must reverse its persisted task marker,
funding reservation, and token reservation without leaving a partially refunded
ledger that a reconciliation worker cannot safely retry.

## Decision

Introduce one relay attempt-loop primitive owned by `service/` (working name
`service.RunRelayAttempts`) that owns the channel-selection + retry + billing
lifecycle. It takes:

- the request context and `RelayInfo`,
- a retry strategy value (`ordinary` | `subscriptionOAuth`) that decides
  continue/stop rules and lease handling, and
- an `attempt` callback that performs the per-attempt upstream call and returns
  a typed result (usage / `*types.NewAPIError`).

`Relay()` and `CodexAlphaSearch()` become thin adapters: each builds its
`RelayInfo`, supplies its endpoint-specific `attempt` callback, and delegates
the loop, pre-consume, settle, refund, and `processChannelError` accounting to
the shared primitive. The billing lifecycle (`EstimateBilling` → pre-consume →
settle/refund) lives in exactly one place; `codex_alpha_search.go` stops owning
a second settlement copy.

State ownership: the shared primitive owns the retry ledger, the used-channel
set, and the billing settlement flag for one request. Adapters own only their
request/response translation.

For a terminal violation-fee error, `Relay` resolves the effective group ratio
and replaces the ordinary request charge through the existing `BillingSession`.
Before settlement, the session must hold the complete fee through
`ReserveStrict`; this bypasses neither wallet/subscription availability checks
nor token quota where applicable, even when the original request used the trusted-user
pre-consume bypass. Settlement can therefore only keep the complete reservation
or return its excess (`delta <= 0`); a violation fee never performs an
unconditional positive wallet delta.

If no billing session exists (for example, an otherwise free model), a strict
pre-consume factory applies the user's normal wallet/subscription preference and
fallback rules. Ordinary and strict wallet reservations use an atomic
conditional database debit. Wallet quota mutations are always applied
synchronously to that database row, so it remains authoritative across
processes under every batch setting; the batch updater aggregates only
`used_quota`, request-count, and channel-used-quota statistics. An eligible
subscription fallback may still reserve transactionally. An existing
reservation that already covers the fee requires no supplemental debit.

Token quota has separate pre-use and committed-use database operations.
`ReserveTokenQuota` conditionally updates a limited token only when
`remain_quota >= amount`; an unlimited token is exempt from that availability
condition. After upstream usage has already happened, `ConsumeTokenQuota`
records the complete positive settlement, legacy, or asynchronous-task delta
even when that leaves a limited token in debt. This prevents committed usage
from disappearing and makes subsequent authentication fail closed. Reservations,
committed debits, and rollbacks for both limited and unlimited tokens bypass the
batch updater, so the database row remains the cross-process ledger.

Redis is only a disposable identity snapshot. Successful token mutations
invalidate it, while a cold, expired, partial, or unavailable cache may fall
back to the database. Security-sensitive token admission also reads the database:
normal authentication checks status, expiry, and exhaustion there, and read-only
authentication checks its disabled-state gate there. The trusted-user
optimization applies only to funding; token reservation is never skipped. A
funding rejection restores the authoritative token row directly.

Responses compact pricing is frozen from the final provider-ready JSON after
model mapping, disabled-field removal, privacy filtering, and channel parameter
overrides. If that target exceeds the request's earlier reservation,
`BillingSession.Reserve` reserves both the token and funding deltas before the
upstream write. This ordinary reserve does not reapply the trusted-wallet
pre-consume bypass: even a session whose initial funding reservation was zero
must prove the complete final target. Supplemental wallet funding uses
`DecreaseUserQuotaIfEnough` against the same authoritative database row,
regardless of whether statistical batch updates are enabled. Strict reservation
means that trust cannot skip this pre-write debit; it is not a ceiling on final
usage. If authoritative compact usage exceeds the estimate, settlement retains
the positive funding and token delta as committed debt.

The legacy OpenAI Realtime handler has one usage-aggregation owner for the
connection. Each `response.done` folds either authoritative upstream usage or
the local fallback for that response into the cumulative usage. It then extends
the existing `BillingSession` reservation to the cumulative quota, providing a
progressive balance gate without recording consumption or a consume log for
that intermediate response. After either socket terminates, both read pumps are
stopped and joined; the handler returns the final accumulated usage to
`WssHelper`, which invokes `PostWssConsumeQuota` once. Final settlement and its
consume log therefore occur once per connection rather than once per
`response.done` plus once again at disconnect.

An asynchronous task receives a durable `PENDING_SUBMIT` marker after local
validation and pre-consume but before the first upstream write. Marker insertion
failure therefore stops locally; once insertion succeeds, the marker becomes
the single refund owner and the request-scoped `BillingSession.Refund` path is
not allowed to race it. Pending markers are excluded from ordinary polling but
remain timeout-reconcilable. Each retry and accepted response renews the
pending marker's `submit_time` lease; the timeout CAS includes that generation,
so a sweep selected before renewal cannot refund a live upstream attempt. An
accepted response first transitions the marker to `SUBMITTED`, making its
upstream ID pollable, then
`model.SettleTaskQuotaAtomically` advances wallet or subscription funding,
token usage, and the marker's separate funding/token reservation snapshot in
one main-database transaction. Any failed post-submit adjustment leaves the
original exact reservation on the accepted task.

Successful completion-stage quota recalculation uses the same
`model.SettleTaskQuotaAtomically` boundary rather than independently mutating
the funding, token, or task ledgers. The operation returns the committed funding
delta derived from the locked persisted reservation plus a `changed` flag. A
caller must never derive secondary accounting from its potentially stale task
snapshot. Only a caller receiving `changed=true` may write the task billing log
or update user used-quota/request-count and channel used-quota statistics, using
that returned delta. Duplicate, concurrent, or stale callers for the same target
receive `changed=false` and must produce none of those effects. A stale caller
with a different target commits only the delta from the current locked
reservation. The persisted task reservation is therefore the single task-ledger
owner for accepted submission settlement, completion recalculation, and terminal
failure refund.

For a failed task, `model.RefundTaskQuotaAtomically` conditionally changes the
non-zero `quota` marker to zero only from `FAILURE` and reverses exactly those
persisted funding and token reservations in the same transaction. Any error
rolls the marker back so reconciliation can retry; a committed zero marker makes
repeated callers a no-op. User/token cache updates occur only after commit. A
historically deleted token is logged as a ledger anomaly but does not block
returning the user's wallet or subscription funding.

This task-specific transaction does not make the general
`BillingSession.Refund` path durable. That path still marks an in-memory session
refunded before dispatching sequential asynchronous funding and token restores;
it has neither a persisted refund intent nor an idempotency key. Crash recovery
and retryable partial failure remain structural follow-up rather than a claimed
exactly-once guarantee.

Insufficient wallet balance, insufficient subscription quota, a closed billing
session, or any quota saturation means the fee is not charged. The caller then
refunds the ordinary reservation, and user/channel usage plus the fee consume
log remain unchanged. A saturated fee has no consume log on which to attach
`other.admin_info.quota_saturation`, so that rejection is instead emitted as a
request-correlated backend warning. Only a successfully committed fee writes a
consume log. If returning reservation excess commits at the funding source but
the later token-ledger adjustment fails, the final fee is still financially
committed and is recorded while the reconciliation warning surfaces the token
failure.

Failure/rollback: if the extraction regresses, the adapters can be reverted to
their inline loops without touching the shared primitive's callers, because the
primitive is additive. The compatibility trigger for removing any transitional
inline code is "all relay entry points route through `RunRelayAttempts`."

## Alternatives considered

- **Keep both loops duplicated**: rejected. The duplication is the defect; each
  new subscription-OAuth feature must be applied twice and can drift on billing
  invariants.
- **Route `CodexAlphaSearch` through `Relay()` verbatim**: rejected. Alpha
  search has a distinct request/response contract and a per-call price mode;
  forcing it through the `/v1/responses` handler would require special-casing
  inside `Relay()`, moving the duplication rather than removing it.
- **Extract only helper functions (no shared loop)**: rejected as insufficient.
  The primitives are already shared; the drift risk is the loop control flow and
  billing lifecycle, which only a shared loop consolidates.

## Consequences

Billing pre-consume/settle/refund and subscription-OAuth failover live in one
tested primitive, so an invariant fix applies to every relay entry point at
once. `codex_alpha_search.go` shrinks to an adapter and loses its second
settlement path. Except for the explicit billing safety fixes recorded here, the
ordinary relay path for non-Codex traffic is unchanged.

Violation fees now reuse the request reservation and settlement ledger. This
removes the temporary balance gap between asynchronous refund and a separate fee
charge, and the full-reservation gate prevents low-balance users from being
driven negative by settlement. Otherwise-free requests create a strict funding
session with the same wallet/subscription ownership and fallback rules as paid
requests. Low balance deliberately forgoes the policy fee instead of weakening
the no-overdraft invariant.

For limited tokens, every pre-use reservation now treats the conditional
database update as the authorization decision rather than trusting a cached
balance before an unconditional update. Once usage is committed, the database
ledger records its full delta, including debt, instead of silently dropping it.
The global batch setting does not change either operation; unlimited-token
accounting uses the same immediate database ledger but is exempt from the
availability condition. The trusted-user funding optimization no longer skips
token reservation.

Compact attempts now reserve against the payload they actually send. A trusted
initial estimate cannot turn into an unfunded final compact request; a
supplemental wallet reserve uses the authoritative conditional database debit
before upstream work begins, including while statistics are batched. Legacy
Realtime keeps the same long-lived connection contract while removing its
intermediate consumption/log writes: cumulative quota is progressively reserved
and financially settled once. Failed task refunds use their persisted quota
marker as a retryable idempotency claim and cannot commit only one of the task,
funding, and extant-token ledger changes. Successful completion recalculation
uses that same task ledger and returns its committed delta plus whether it
changed; secondary accounting never uses a caller's stale snapshot.

Must be monitored during rollout: refund-once and no-double-charge behavior on
alpha search, subscription-OAuth lease release on every exit path, and parity of
retry counts/`processChannelError` accounting between old and new loops.

## Verification

- Table tests for `RunRelayAttempts` covering: success on first attempt; retry
  then success; exhausted retries; pre-consume rejection of a saturated quota;
  in-process duplicate-refund suppression on failure; no settle-after-refund.
- Subscription-OAuth strategy tests: lease released on error, on nil body, and
  on success; failover suppressed once the downstream response has started.
- Golden parity test: alpha-search and `/v1/responses` produce identical
  billing ledger entries (pre-consume, settle, refund) for the same simulated
  upstream outcomes, before and after the change.
- Regression: existing `controller` and `service` billing/task test suites pass
  unchanged.
- Violation-fee regression: a reserved failed request ends with exactly the fee
  deducted from wallet/subscription and token quota, records one fee log, and
  does not perform a later asynchronous refund or a second charge.
- Violation-fee rejection regressions: insufficient supplemental wallet quota,
  insufficient subscription quota, and saturated fee conversion perform no
  settlement or fee-ledger write and leave the ordinary reservation refundable.
- Strict reservation regressions: competing conditional wallet reservations
  and competing limited-token reservations cannot both exceed their balances; a
  refunded session cannot settle; wallet and token quota remain database-backed
  while used-quota, request-count, and channel-used-quota statistics are
  batched; and a token reservation is restored without publishing a partial
  cache when the funding source rejects the full fee.
- Token-ledger regressions: a stale high Redis snapshot cannot authorize beyond
  the persisted limited-token balance; competing reservations commit at most the
  available database quota even with batch updates enabled; and a cold or partial
  identity cache is safely rebuilt because token deltas are never pending outside
  the database. Stale enabled/non-exhausted snapshots cannot authorize normal or
  read-only requests contrary to the persisted security state, and committed
  post-use deltas remain recorded even when they cross zero.
- A real non-playground wallet session can extend an ordinary limited-token
  reservation through `ReserveStrict`, settle the exact target once, and cannot
  refund after settlement.
- Compact regressions: the final provider-ready model, request-expression input,
  and prompt estimate select the reservation; an initially trusted wallet still
  reserves that full target, and a supplemental wallet reserve conditionally
  debits the authoritative database row before the upstream write even when
  statistics are batched.
- Legacy Realtime regression: several `response.done` events extend the
  cumulative reservation, authoritative usage replaces only its response-local
  fallback, and connection termination produces one final settlement/consume
  log rather than charging each response twice.
- Task-settlement regressions: accepted submission and successful completion
  recalculation both use `SettleTaskQuotaAtomically`; concurrent or same-target
  stale completion callers yield exactly one `changed=true`, and every
  `changed=false` caller writes no task billing log, user used-quota/request-count
  statistic, or channel used-quota statistic. A stale different-target caller's
  log and statistics use the committed delta from the locked reservation rather
  than its snapshot.
- Task-refund regressions: wallet and subscription funding, the task quota
  marker, and an extant token reservation commit together; a transaction error
  rolls all of them back, a non-`FAILURE` task is rejected, a repeated claim is a
  no-op, and a deleted historical token does not strand the user's funding.

## Implementation status

**Phase 1 (done).** The per-attempt channel-error accounting block —
`ApplyChannelErrorPolicy` followed by either a subscription-OAuth
capacity-failover log or `processChannelError` — was duplicated verbatim in
`Relay()` and `CodexAlphaSearch()`. It is now a single `recordChannelAttemptError`
helper in `controller/relay.go` used by both. This is the highest-drift-risk
shared code (a divergence here wrongly disables or spares a channel), and the
extraction is behavior-preserving by construction. This phase itself did not
change billing behavior.

**Independent violation-fee safety fix (done).** A provider-policy violation no
longer triggers an asynchronous refund followed by a separate charge. `Relay`
requires a complete strict reservation before synchronously settling the
configured fee. Otherwise-free requests use a strict billing-session factory;
existing paid sessions supplement their reservation only when the selected
funding source and any limited token can cover the entire difference through
atomic conditional debits. Wallet quota remains a synchronous,
database-authoritative ledger while only usage statistics are batched. Rejection
restores any strict token delta, keeps the ordinary reservation refundable, and
emits no consume ledger. Checked fee saturation is rejected consistently on
every path and is recorded in the request-correlated backend audit because no
successful consume log exists. A funding source that already committed the
final fee is recorded and never refunded after a later token-ledger adjustment
error.

**Independent token authorization hardening (done).** Ordinary and strict
limited-token reservations use the same conditional database boundary even when
batch updates are enabled. Settlement fallbacks and asynchronous-task deltas
record committed usage through an unconditional database debit, so a failed
availability check cannot erase usage that upstream already served. Rollback and
unlimited-token accounting use the immediate database ledger too; no token delta
remains process-local while another request reads the row. Redis can be rebuilt
after a miss but cannot authorize a request: authentication reads persisted
status and quota state. The trusted-user bypass is funding-only and never skips
token reservation.

**Independent final-payload compact reservation fix (done).** Compact request
conversion now freezes billing from the provider-ready JSON. Its ordinary
`BillingSession.Reserve` supplements token and funding reservations even when
the initial request used the trusted-wallet bypass. Supplemental wallet funding
uses the conditional database debit under every batch setting before the request
is sent upstream.

**Independent legacy Realtime settlement fix (done).** A single connection-local
aggregator owns usage from both read pumps. Each completed response extends the
cumulative reservation, but no intermediate response writes consumption or a
log. After termination closes both sockets and joins both pumps, the relay
settles the accumulated usage once through `PostWssConsumeQuota`.

**Independent task settlement/refund transaction fix (done).** A
`PENDING_SUBMIT` marker is inserted before upstream I/O and takes over refund
ownership from the request session. Accepted-task funding, token, and marker
adjustments commit together; a failed adjustment retains the prior exact
funding/token snapshot. Failure refunds reverse that snapshot and the non-zero
marker in one transaction. The zero marker supplies retry idempotency, cache
synchronization follows commit, and any pre-commit failure leaves the marker
available to the reconciliation sweep. Successful completion recalculation also
calls `SettleTaskQuotaAtomically`; its returned locked-reservation delta supplies
secondary accounting and its `changed` result is the sole gate for the task
billing log and user/request/channel statistics. Same-target duplicate, stale,
and concurrent polling therefore cannot reproduce those effects. Submit
settlement, completion recalculation, and failure refund share the persisted
task reservation as their single ledger owner.

**Phase 2 (deferred).** Collapsing both attempt loops into one shared primitive
that also owns the billing settle was evaluated against the two real loops and
found to trade little duplication for real regression risk, because the loops
differ in six behavior-bearing ways:

1. First attempt: alpha-search pins the channel via `CacheGetChannel`; `Relay`
   always calls `getChannel`.
2. `RetryIndex`: `Relay` branches on ordinary vs subscription-OAuth; alpha-search
   always uses `AttemptIndex`.
3. Settle point: `Relay` settles inside the relay handlers
   (`PostConsumeQuota`); alpha-search settles in the loop because it reads and
   returns the upstream body directly.
4. Retry limit and continue/break: `Relay` runs an ordinary path
   (`shouldRetryOrdinaryRelay` + `IncreaseRetry`) and a subscription path;
   alpha-search is subscription-only.
5. Error response emission: `Relay` uses a deferred `newAPIError` handler with
   per-format encoding; alpha-search emits the error inline.
6. Per-attempt body reset: `Relay` re-binds `c.Request.Body` from body storage;
   alpha-search re-marshals a fresh payload.

A shared loop would need a strategy branch for each, leaving it about as complex
as the two loops combined while concentrating billing-critical control flow in
one place that cannot be exercised end-to-end by unit tests alone. Phase 2
should be undertaken only with relay integration-test infrastructure that can
assert the golden billing-ledger parity above; until then the pre-consume →
deferred-refund → settle lifecycle stays owned by each entry point, which
already shares its primitives (`getChannel`, `trackRetryAttempt`,
`addUsedChannel`, `recordChannelAttemptError`, `processChannelError`).
