# Architecture Health

## Current change map

The customized branch adds or changes the following coherent domains:

| Domain | Main ownership | Result |
| --- | --- | --- |
| Codex OAuth and Responses | `service/codex_oauth.go`, `relay/channel/codex/`, `controller/codex_*` | Browser authorization, refresh, model discovery, usage, Alpha search, and Responses WebSocket support |
| Claude Code OAuth | `relay/channel/claude/`, `pkg/oauthcred/claude_code.go` | OAuth request authentication and response-based error classification |
| Credential reliability | `service/subscription_oauth_*.go`, `service/retry_data_policy.go`, `controller/relay.go` | Capacity slots, pacing, circuit recovery, classified errors, retry boundaries, and data-policy isolation |
| Model administration | `controller/channel_*`, `model/channel_*metadata.go` | Per-channel catalogues, model capability metadata, upstream model checks, and multi-key management |
| Privacy and disclosure | `common/upstream_location.go`, `relay/common/location_privacy.go` | Network-profile discovery, client location filtering, and controlled response disclosure |
| Pricing and options | `controller/option.go`, `controller/ratio_sync.go`, `controller/official_api_price_catalog.go`, `model/option.go`, model-pricing UI | Validated transactional model-price saves, configurable completion ratios, and reviewed official API pricing sources |
| Billing lifecycle | `service/billing_session.go`, `relay/responses_handler.go`, `relay/channel/openai/relay_realtime.go`, `model/task.go` | Final-payload reservation, progressive Realtime reservation with one settlement, and atomic idempotent task settlement/refund |
| Deployment | `bin/deploy-*.sh`, Compose/Caddy files, Dockerfiles | Per-target artifacts, checksummed transfer, backup, rollback, and endpoint verification |
| Default frontend | `web/default/src/features/channels`, `features/system-settings`, `lib` | OAuth configuration, channel governance, operational privacy, routing reliability, and localized errors |

`SOURCE_CHANGES_FOR_AUTHOR.md` is the complete user-facing functional change
catalogue. This document records structural health rather than duplicating each
feature description.

## Healthy changes already made

- Large channel-management and upstream-model-update controllers were split by
  responsibility rather than kept as monoliths.
- Target deployment wrappers were reduced to target parameters, with common
  workflow code shared.
- Channel data-governance fields and routing-reliability form data were moved
  out of the channel drawer and settings section respectively.
- Codex collaboration stream normalization now lives with the Codex adaptor,
  not in relay-wide common code.
- Model pricing writes use a validated transactional batch endpoint.
- Routing reliability now uses its own Root-only transactional batch endpoint;
  it validates the complete related setting set before any Option write.
- A written subscription OAuth request now stops on ambiguous transport failure
  instead of being replayed to the current or a backup credential.
- Responses WebSocket connection generations now have one permanent upstream
  reader, terminal acknowledgement, post-terminal metadata classification,
  connection-local liveness probes, and an active 55-minute lifetime. Long-idle
  continuations therefore either use their verified original connection or fail
  before writing; they never migrate to a replacement connection. Passive
  availability currently benefits from the observed upstream ping cadence of
  about 20 seconds; if that changes, the nonce probe preserves correctness but
  continuation availability may degrade to full-context recovery.
- Monitor concurrent idle Responses WebSocket connections under the 55-minute
  lifetime. Add a single session-owner LRU/global cap only if production data
  shows unbounded idle growth; do not introduce a second connection pool.
- The Gin request state and retry tracker share one attempt object, so lease
  generation, recovery-probe, and response-scope metadata are no longer copied
  between independent records.
- Responses compact now reserves from its provider-ready payload; an earlier
  trusted-wallet bypass cannot leave that final target unfunded. Supplemental
  wallet quota is conditionally reserved against the synchronous,
  database-authoritative ledger even while used-quota, request-count, and
  channel-used-quota statistics are batched.
- Legacy OpenAI Realtime uses progressive cumulative reservations but delegates
  the connection's actual consumption and log to one final settlement.
- Asynchronous tasks persist a non-pollable `PENDING_SUBMIT` marker before the
  upstream write. Accepted settlement, successful completion recalculation, and
  failed-task refund share its exact funding/token snapshot as the single task
  ledger owner. Settlement returns its locked-reservation delta and a `changed`
  flag; secondary accounting uses that delta, and only a changed caller may
  write the task billing log and user/request/channel statistics. Same-target
  stale, duplicate, and concurrent pollers therefore cannot repeat those
  effects. Each ledger transition uses one main-database transaction, and marker
  insertion failure cannot leave an accepted untracked task.

## Current release decision

- The merge conflicts observed at the start of this review were resolved before
  the review completed: no unmerged paths or conflict markers remained, and the
  default frontend typecheck passed. This is a release gate that must remain
  green, not a one-time cleanup.
- No unresolved release blocker is recorded by this review. Future retries that
  introduce a new idempotency mechanism require an ADR and explicit upstream
  contract evidence before changing the written-request stop rule.

## Required structural follow-up

1. Extend transactional batch saves to other related Root settings only after
   their cross-field validation is defined. Routing reliability and model
   pricing each own an explicit atomic endpoint; a blanket endpoint must not
   bypass settings-specific safety preconditions.
2. Keep `bin/deploy-common.sh` as orchestration only. If target-local activation
   grows further, split build/transfer/verification helpers into focused shell
   libraries rather than adding more nested remote shell logic.
3. Split `RelayInfo` by stable concepts before adding more provider-specific
   fields: immutable request metadata, channel selection, upstream attempt,
   streaming state, and billing state.
4. Replace duplicated routing-reliability defaults, form types, and normalized
   payload types with a schema-derived normalized payload.
5. Replace the process-local `BillingSession.Refund` guard with a durable refund
   intent and idempotency contract owned by the persistence layer. Today the
   session marks itself refunded before asynchronously restoring funding and
   token quota in sequence; a process exit can lose the work, and a partial
   failure is only logged with no safe cross-process retry key. Any durable
   design must define atomic state transitions for both ledger legs, tolerate
   worker retries, and preserve the current no-refund-after-settlement rule in an
   ADR before claiming crash-safe or exactly-once refunds.
6. Move the Responses event-classification predicates into a transport-neutral
   Responses protocol module. Today `IsConnectionScopedEventType` lives in
   `relay/responsesws` (HTTP semantics owned by the WS package) and the
   terminal-event lists are still duplicated across the WS session, the native
   HTTP stream handler, and the Responses→Chat conversion handlers. The target
   is one classifier returning metadata / turn event / clean terminal / failure
   terminal / invalid, consumed by all transports (ADR 0007).
7. Convert the stream-scanner dual idle-bound tests
   (`relay/helper/stream_scanner_test.go`, comment-only and comment-bridged
   cases) from wall-clock sleeps to an injectable clock. The behavior they lock
   is real, but the current form conflicts with the no-sleep test guideline and
   costs ~3.5s per run.

## Change review protocol

For a local bug fix, implement and verify directly. For a cross-module feature,
record the intended user outcome, existing capability, state owner, retry and
rollback behavior, and obsolete code to delete. For a new workflow, persistent
state, billing rule, authentication flow, or retry policy, create an ADR before
implementation.

Each substantial change closes with: behavior changed, state ownership changed,
code deleted or consolidated, temporary compatibility added, and verification
actually run. Architecture work is limited to the touched path unless this file
identifies a release blocker.
