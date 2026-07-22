# ADR 0006: Claude Code (Subscription) OAuth channel

Status: Proposed — not implemented. This record captures the design and the
compliance risk so the project owner can decide whether to build it. No code has
been written. If accepted, implementation follows the "Decision" section; if
declined, this record stays as the rationale for not adding the channel.

## Context

The gateway already has a **Claude Code (OAuth)** channel
(`ChannelTypeClaudeCode = 59`) that accepts only a *static*
`CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat...`. That token is long-lived (~1 year) but
still expires, and it carries no refresh token: when it lapses an operator must
re-run `claude setup-token` and paste the new value by hand. There is no
automatic renewal.

By contrast the **Codex (ChatGPT subscription)** channel
(`ChannelTypeCodex = 57`) implements a full subscription OAuth login flow —
browser authorization → exchange for `access_token + refresh_token` → persist a
JSON credential in `channel.Key` → refresh both on a background timer and on a
401 during relay. It reuses a provider-agnostic subscription-OAuth framework
(`service/subscription_oauth_*.go`: capacity leasing, retry, error policy,
usage-limit cooldown, usage cache) that is gated on
`constant.IsSubscriptionOAuthChannel`.

Operators want the same "log in once, auto-refresh forever" experience for
Anthropic subscription accounts that Codex already gives for ChatGPT accounts.

Non-negotiable constraints:

- The existing static **Claude Code (OAuth)** channel must not change behavior.
- Relay must reuse the existing `claude.Adaptor` (`APITypeAnthropic`); no second
  Anthropic transport path.
- No second source of truth for subscription-OAuth capacity/retry/refresh state;
  reuse the shared framework and the shared `model/auth_flow.go` store.
- All JSON via `common.Marshal/Unmarshal`; credential persists in the existing
  `channel.Key` TEXT column (works on SQLite/MySQL/PostgreSQL unchanged).

## Decision

Add a new channel type **Claude Code (Subscription)**
(`ChannelTypeClaudeCodeSubscription = 60`) that layers Codex-style OAuth login +
auto-refresh onto the existing Claude transport. The channel type owns its
credential JSON in `channel.Key`; the shared subscription-OAuth framework owns
capacity/retry/cooldown state; `model/auth_flow.go` owns the transient login
flow (`Provider: "claude"`).

Verified Anthropic subscription OAuth protocol (source-level: querymt/
anthropic-auth, ben-vargas gist, opencode community + packet captures):

| Item | Value |
|---|---|
| Authorize endpoint | `https://claude.ai/oauth/authorize` |
| Token endpoint (exchange + refresh) | `https://console.anthropic.com/v1/oauth/token` |
| client_id | `9d1c250a-e61b-44d9-88ed-5944d1962f5e` |
| redirect_uri (paste mode) | `https://console.anthropic.com/oauth/code/callback` |
| scope | `org:create_api_key user:profile user:inference` |
| PKCE | S256; authorize also sends `code=true` (server echoes `code#state`) |
| Code exchange | JSON body: `{code, state, grant_type:"authorization_code", client_id, redirect_uri, code_verifier}` |
| Refresh | JSON body: `{grant_type:"refresh_token", refresh_token, client_id}` |
| Response | `access_token` (`sk-ant-oat01-`), `refresh_token`, `expires_in` (~3600s interactive) |

Inference headers and the mandatory Claude Code identity system block are already
produced by the existing `claude.Adaptor` (`BuildClaudeCodeOAuthHeaders`,
`ensureClaudeCodeIdentitySystem`) and are reused unchanged.

Implementation shape (mirrors Codex except where noted):

- **Type registration** — `constant/channel.go`: add
  `ChannelTypeClaudeCodeSubscription = 60` before `ChannelTypeDummy` (Dummy has
  no expression, so it copies `= 60`; the `for i:=1;i<=Dummy` loop at
  `controller/model.go:102` then covers it). Add to
  `IsSubscriptionOAuthChannel`, append `ChannelBaseURLs` index 60 =
  `https://api.anthropic.com`, and add the name mapping. `common/api_type.go`
  `ChannelType2APIType` → `APITypeAnthropic`. No change to
  `relay/relay_adaptor.go` or `constant/api_type.go`.
- **Adaptor reuse** — `relay/channel/claude/adaptor.go`: widen the four
  `== ChannelTypeClaudeCode` gates (identity injection, header setup, capacity
  lease) to `IsSubscriptionOAuthChannel(info.ChannelType)`. Split credential
  parsing: static type keeps `ParseClaudeCodeOAuthToken`; subscription type
  extracts `access_token` from the JSON credential (new
  `extractSubscriptionAccessToken`), with the same `sk-ant-oat` prefix check.
- **Credential** — new `dto/claude_oauth.go` (`ClaudeOAuthCredential`, mirroring
  `CodexOAuthCredential`) + `relay/channel/claude/oauth_key.go`. `account_id`
  and `email` are optional (a Claude OAuth token is not guaranteed to be a
  decodable JWT), unlike Codex which requires `account_id`.
- **OAuth service** — new `service/claude_oauth.go` mirroring
  `service/codex_oauth.go`: `CreateClaudeOAuthAuthorizationFlow`,
  `ExchangeClaudeAuthorizationCode`, `RefreshClaudeOAuthToken[WithProxy]`,
  `ClaudeOAuthUpstreamError`. Requests use JSON bodies against the endpoints
  above; env overrides (`CLAUDE_SUBSCRIPTION_OAUTH_CLIENT_ID`, etc.) parallel
  the Codex ones.
- **Auto-refresh** — new `service/claude_credential_refresh.go` +
  `_task.go`: per-channel mutex, optimistic CAS update
  (`WHERE key = previousKey`), master-node `sync.Once` background scan of
  soon-to-expire subscription channels; registered near `main.go:121`. Runtime
  capacity/timeout init reuses the existing `claude.InitOAuthRuntimeSettings`.
- **401-refresh gating** — generalize `ShouldRefreshCodexOAuthCredential` /
  `IsPermanentCodexOAuthRefreshFailure`
  (`service/subscription_oauth_error_policy.go`) to be provider-agnostic via
  `IsSubscriptionOAuthChannel`, and have
  `controller/relay.go:refreshCodexCredentialForRetry` dispatch by channel type
  to the matching `Refresh*ChannelCredential`, preserving the single-refresh
  claim (`ClaimSubscriptionOAuthCredentialRefresh`) semantics.
- **Login controller + routes** — new `controller/claude_oauth.go`
  (`StartClaudeOAuth` / `CompleteClaudeOAuth`, `model.CreateAuthFlow` with
  `Provider: "claude"`); `router/channel-router.go` adds
  `/claude/oauth/start`, `/claude/oauth/complete`, `/:id/claude/refresh`.
- **Validation / model-fetch** — `controller/channel.go` adds a JSON-credential
  validation branch (generic subscription guardrails already apply via
  `IsSubscriptionOAuthChannel`); `controller/channel_models.go` generalizes the
  "preserve full OAuth JSON" and OAuth-header model-fetch branches from
  `== ChannelTypeCodex` to the subscription predicate.
- **Frontend** (`web/src/features/channels/`) — register type `60` in
  `constants.ts` (types, order, options, fetchable, key prompt, warning);
  new `claude-oauth-dialog.tsx` cloned from `codex-oauth-dialog.tsx`; wire the
  login/refresh UI block and base-URL grouping in `channel-mutate-drawer.tsx`;
  add `startClaudeOAuth`/`completeClaudeOAuth`/`refreshClaudeCredential` to
  `api.ts` and `use-channel-credential-actions.ts`; icon in `channel-utils.ts`;
  i18n keys in `web/src/i18n/locales/*.json`.

## Alternatives considered

- **Extend the existing static Claude Code (OAuth) channel to also accept a
  refreshable JSON credential** (no new type). Rejected: it overloads one
  channel type with two credential formats and two lifecycles, complicates
  validation and the admin UI, and risks changing behavior for existing static
  deployments — violating the "must not change" constraint.
- **A new dedicated Anthropic transport / `APIType`.** Rejected: the existing
  `claude.Adaptor` already handles Anthropic relay, identity injection, and
  OAuth headers; a second path would duplicate that and drift.
- **A generic "subscription OAuth provider" plugin abstraction** covering Codex
  and Claude behind one interface. Rejected for now: only two providers exist,
  the shared `service/subscription_oauth_*.go` framework already carries the
  common state, and the per-provider auth endpoints/quirks are small. Revisit if
  a third subscription provider appears.
- **Do nothing / keep only the static token.** This remains the fallback if the
  compliance risk below is judged unacceptable.

## Consequences

- **Compliance / availability risk (primary).** Around 2026-02 Anthropic began
  blocking third-party applications that use a subscription OAuth token
  (`sk-ant-oat01-`) against `api.anthropic.com/v1/messages` (issues
  anthropics/claude-code#28091, #18340; NousResearch/hermes-agent#15080), and
  the TOS restricts subscription tokens to the official Claude Code client. Even
  with every protocol parameter correct, this channel **may stop working at any
  time and may violate the TOS / risk account suspension.** This is why the
  channel is proposed, not built. If implemented, it should carry a prominent
  admin-facing disclaimer (`CHANNEL_TYPE_WARNINGS`) and its failures should be
  treated as expected, not as gateway bugs.
- **What stays compatible.** The static Claude Code (OAuth) channel is untouched
  (separate type, separate credential parser). Rollback = remove type `60` and
  its files; no data migration, since the credential lives in the existing
  `channel.Key` TEXT column and no schema changes are made.
- **What must be monitored.** The Anthropic OAuth endpoints, client_id, scope,
  and required beta/identity headers are reverse-engineered, not officially
  documented; they can change without notice. The refresh path must never
  produce two concurrent refreshes for one channel (single-claim invariant) and
  must fail closed (a permanent 400/401/403 from the token endpoint disables the
  channel and requires re-auth).
- **State ownership.** Credential JSON → `channel.Key`; capacity/retry/cooldown →
  shared `service/subscription_oauth_*.go`; transient login flow →
  `model/auth_flow.go` (`Provider: "claude"`). No new source of truth.

## Verification

- **Unit (table-driven, testify)** mirroring the Codex tests: authorize-URL
  assembly (PKCE + `code=true`); JSON-body code exchange and refresh field/
  endpoint correctness; `extractSubscriptionAccessToken` over both JSON and
  static keys incl. `sk-ant-oat` prefix rejection; provider-agnostic
  `ShouldRefreshSubscriptionOAuthCredential` returning true for type `60`; CAS
  refresh optimistic lock (`WHERE key = previousKey`) rejecting a stale key.
- **Build/format.** `go build ./...`, `gofmt`; frontend type-check/build.
- **End-to-end** (real subscription account, mindful of the block risk): create
  channel → front-end authorize → JSON credential persisted → one `/v1/messages`
  request confirms identity block + OAuth headers injected → force `expired`
  near now to trigger background/401 refresh and confirm the key is CAS-updated
  and the retried request succeeds.
- **Rollback test.** Disabling/removing type `60` leaves existing static Claude
  Code (OAuth) channels and Codex channels functioning unchanged.
- **Cross-DB.** No schema change; credential in `channel.Key` TEXT — identical on
  SQLite, MySQL, PostgreSQL.
