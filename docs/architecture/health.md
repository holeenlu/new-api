# Architecture Health

## Current change map

The customized branch adds or changes the following coherent domains:

| Domain | Main ownership | Result |
| --- | --- | --- |
| Codex OAuth and Responses | `service/codex_oauth.go`, `relay/channel/codex/`, `controller/codex_*` | Browser authorization, refresh, model discovery, usage, Alpha search, and Responses WebSocket support |
| Claude Code OAuth | `relay/channel/claude/`, `pkg/oauthcred/claude_code.go` | OAuth request authentication and shared subscription safeguards |
| Credential reliability | `service/subscription_oauth_*.go`, `service/retry_data_policy.go`, `controller/relay.go` | Capacity slots, pacing, circuit recovery, classified errors, retry boundaries, and data-policy isolation |
| Model administration | `controller/channel_*`, `model/channel_*metadata.go` | Per-channel catalogues, model capability metadata, upstream model checks, and multi-key management |
| Privacy and disclosure | `common/upstream_location.go`, `relay/common/location_privacy.go` | Network-profile discovery, client location filtering, and controlled response disclosure |
| Pricing and options | `controller/option.go`, `model/option.go`, model-pricing UI | Validated transactional model-price saves and configurable completion ratios |
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
- The Gin request state and retry tracker share one attempt object, so lease
  generation, recovery-probe, and response-scope metadata are no longer copied
  between independent records.

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
