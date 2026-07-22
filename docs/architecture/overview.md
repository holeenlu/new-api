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
- `relay/responsesws/session.go` owns the upstream WebSocket connection and its
  channel/model/credential binding. The controller restores that exact binding
  for later turns and may release it only after the shared retry state machine
  declares the turn safe to replay. Capacity leases remain per-turn, and an
  early response-body close invalidates the connection before another reader
  can start.
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
