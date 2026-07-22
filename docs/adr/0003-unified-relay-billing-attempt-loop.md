# ADR 0003: Unified relay billing attempt loop

Status: Partially implemented. Phase 1 (shared per-attempt channel-error
accounting) and the independent violation-fee settlement safety fix are done.
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
- Except for the explicit violation-fee safety fix below, ordinary non-violation
  relay behavior for non-Codex traffic must not change.

Terminal provider-policy violations are a billing exception to the ordinary
"failure means full refund" rule. The configured fee must replace the request's
normal charge without creating a refund/recharge race.

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
and synchronously settles the request's existing `BillingSession` to the fee
quota. It does not asynchronously refund the reservation and then start a second
charge. If no billing session exists (for example, an otherwise free model), the
fee first creates a fully reserved session through the user's normal
wallet/subscription preference; it must not fall through to an implicit wallet
delta. A non-applicable fee or a settlement failure that remains refundable
falls back to the ordinary idempotent refund; a successfully settled fee is
never followed by `Refund`. If the funding source committed but the later token
ledger adjustment failed, the session is no longer refundable: the charge is
still recorded in the consume ledger while the existing reconciliation warning
surfaces the token adjustment failure.

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
settlement path. Except for the explicit violation-fee safety fix, the ordinary
non-violation relay path for non-Codex traffic is unchanged.

Violation fees now reuse the request reservation and settlement ledger. This
removes the temporary balance gap between asynchronous refund and a separate fee
charge. Otherwise-free requests now create the same funding session as paid
requests, retaining wallet/subscription selection, token-quota adjustment, and
the existing saturation audit on the computed fee.

Must be monitored during rollout: refund-once and no-double-charge behavior on
alpha search, subscription-OAuth lease release on every exit path, and parity of
retry counts/`processChannelError` accounting between old and new loops.

## Verification

- Table tests for `RunRelayAttempts` covering: success on first attempt; retry
  then success; exhausted retries; pre-consume rejection of a saturated quota;
  refund-exactly-once on failure; no settle-after-refund.
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
synchronously settles the request's existing reservation to the configured fee;
an otherwise-free request first creates a fully reserved billing session through
the user's wallet/subscription preference. A settlement that remains refundable
falls back to the ordinary refund, while a funding source that already committed
is recorded and never refunded after a token-ledger adjustment error. Fee quota
conversion uses the shared checked saturation path and attaches the existing
admin-only quota-saturation audit marker.

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
