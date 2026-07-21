# ADR 0005: Subscription-OAuth usage-limit cooldown

Status: Implemented.

## Context

Subscription-OAuth channels (currently Codex and Claude Code) return HTTP 429
for two very different conditions:

- A **short burst rate limit** ("slow down, retry shortly") that clears in
  seconds to minutes.
- A **plan/usage-limit exhaustion** (for example, a five-hour or weekly cap —
  e.g. Codex's "you've reached your usage limit", `你已达到使用上限`) that only
  resets in hours to days.

The credential retry policy treated every 429 the same: it classified it as
`UpstreamRateLimited` (transient, never quarantined — correct, since the account
recovers on its own) and cooled the credential down for
`min(Retry-After, 15 minutes)` (`maximumSubscriptionOAuthRetryAfter`). For a
weekly-cap exhaustion that cap is far too short: the credential's circuit
reopened within 15 minutes, routing re-selected the exhausted account, and the
client saw the usage-limit error again — repeating every ≤15 minutes for days.
Operators had to manually disable the channel to stop it (auto-disable does not
apply, because a 429 must not flip a channel's enabled state).

## Decision

Classify usage-window exhaustion as `upstream_usage_limit`. It remains a
temporary, credential-level circuit state rather than a persistent channel
quarantine, but it receives its own retry transition and reset-aware cooldown.

- `IsSubscriptionOAuthUsageLimit(channelType, err)` recognizes a usage-limit 429
  by two independent signals: (a) message markers (`usage limit`, `weekly limit`,
  `monthly limit`, `plan limit`, `使用上限`, …), deliberately excluding generic
  "rate limit" phrasing; and (b) reset magnitude — a `429` whose parsed reset
  (`err.RetryAfter`) exceeds the 15-minute burst cap
  (`maximumSubscriptionOAuthRetryAfter`) is a usage window regardless of wording,
  because a window resets in hours-to-days while a genuine burst resets in
  seconds-to-minutes. This keeps a real burst limit on the short transient
  cooldown while catching an exhausted window whose 429 carries no usage-limit
  text.
- Claude Code inference classifies the response that actually rejected the
  model request. A generic `rate_limit_error` with a seconds-level `Retry-After`
  (e.g. an acceleration/burst limit) stays `upstream_rate_limited`. A `429` is
  `upstream_usage_limit` when it carries usage-window semantics
  (`usage_limit`, `usage limit`, `weekly limit`, an explicit provider code) OR a
  reset window longer than the burst cap (from a structured reset field, the
  `Retry-After` header, or Anthropic's `anthropic-ratelimit-unified-reset`
  header). Structured reset fields, unified headers, and `Retry-After` determine
  the cooldown when present; a usage limit without reset metadata uses the
  one-hour fallback.
- The gateway still does not actively call
  `GET https://api.anthropic.com/api/oauth/usage`: it is not reliable evidence
  about the model request that just failed. It does, however, read the
  `anthropic-ratelimit-unified-*` headers that Anthropic returns *passively on
  the failed inference response itself* (`-status`, per-window `-5h-status` /
  `-7d-status`, and the Unix-second `-unified-reset`). Those headers are only
  treated as a usage-window reset when the unified status marks the account
  exhausted (`rejected`, or a window `exceeded` / `rate_limited`), so a 429 whose
  window is still available (`allowed`) does not inherit a multi-hour cooldown.
- Codex can likewise return an opaque wrapper such as `exceeded retry limit,
  last status: 429 Too Many Requests` after a subscription window is exhausted.
  The controller correlates that ambiguous 429 with a fresh snapshot from the
  credential's Wham usage endpoint. Only `limit_reached`, `allowed=false`, or a
  100% primary/secondary window upgrades it to `upstream_usage_limit`; lookup
  failure, stale evidence, and an available window retain transient-429
  behavior. Codex Wham correlation uses the same bounded 30-second evidence and
  five-second negative-cache policy.
- `RelayErrorHandler` extracts `resets_at`, `resets_in_seconds`,
  `reset_after_seconds`, and `retry_after` from structured error wrappers, then
  reads Anthropic's `anthropic-ratelimit-unified-reset` header (Unix seconds,
  only when the unified status shows the window exhausted), and finally falls
  back to the `Retry-After` header. An absolute `resets_at` takes precedence.
  Values must be positive and are bounded to eight days, which covers a weekly
  window without allowing malformed input to sideline a credential indefinitely.
- `SubscriptionOAuthCredentialCooldownForError` honors a valid exact reset
  delay. If the upstream supplies no usable reset evidence, it uses a one-hour
  fallback. Ordinary burst 429s (reset within the 15-minute cap) keep the
  existing maximum 15-minute cooldown.
- The 429 retry path applies this cooldown both to the credential circuit
  (`failCurrentSubscriptionOAuthCredential`) and to the value returned to the
  caller.
- Before downstream output, a usage-limit response always excludes the current
  credential and selects another compatible credential in the request's frozen
  group. This transition is independent of the generic "429 cross-account
  retry" setting. Explicit-channel requests and requests that already emitted
  downstream output still stop instead of being replayed. A Responses WebSocket
  channel pin is internal session state rather than an explicit-channel request:
  on a replay-safe credential failure the session releases that binding before
  the relay selects another same-group credential. Other WebSocket failures
  retain the existing retry-safety checks.
- A status-only OAuth 401/403 excludes and briefly cools the credential for the
  current request, but does not persistently disable a channel. Codex first
  performs one CAS-protected access-token refresh and safely replays the same
  credential before output; an expired access token alone is never quarantine
  evidence. Persistent quarantine requires the token endpoint to reject the
  refresh credential as revoked/invalid or require reauthorization, or an
  explicit account/organization disablement or permanent billing-credit
  exhaustion. Claude has no equivalent refresh path.
- HTTP errors, Codex Responses SSE terminal errors, and Responses WebSocket
  handshake errors all feed the same error classifier and retry state machine.
- The process-local credential circuit preserves the cooldown reason. Requests
  that encounter an already-cooled credential return HTTP `429` with
  `upstream_rate_limited` or `upstream_usage_limit`, the remaining
  `Retry-After`, and a cause-specific message. Only active local concurrency or
  a non-rate-limit transient circuit state uses HTTP `503`; cooldowns no longer
  degrade into a generic `oauth_channel_concurrency_limit` / "credential busy"
  response.
- Channel-management operations (account information, credential refresh,
  upstream model discovery, and an administrator's manual minimal channel
  test) are outside inference concurrency, request-start pacing, cooldown, and
  recovery state. Their success or failure does not clear, extend, or probe the
  inference circuit, so administrators can inspect and repair a cooled or
  saturated channel. Both scheduled and administrator-triggered batch channel
  tests exclude every subscription OAuth channel; only the administrator's
  one-channel minimal test is available.
- HTTP 200 Responses streams may carry a terminal `response.failed` rather than
  an HTTP error status. Before any downstream event is committed, the relay
  treats usage-window exhaustion, OAuth 401/403 semantics, account disablement,
  and permanent quota exhaustion as credential-switch failures. This applies to
  HTTP/SSE and WebSocket-to-SSE transport alike.
- Stream terminal errors derive their upstream status from structured status,
  type and code fields. Unknown terminal failures become `502`, not a fabricated
  `429`; overload remains `503`, authorization remains `401/403`, and explicit
  rate/usage/model-capacity signals remain `429`.
- Capacity cycling is reserved for active process-local concurrency saturation.
  A credential already in rate-limit or usage-window cooldown is excluded
  immediately and cannot enter a capacity replay cycle. Model-capacity exclusion
  is request-local, so it cannot suppress unrelated models on the same account.
- Each credential retains its configured five-attempt budget, and one request
  is additionally capped at ten relay attempts across all eligible
  credentials. The last real upstream error is returned when that bound is hit.
- Anthropic responses retain a protocol-compatible `error.type` and expose the
  stable gateway classification in `error.code`, allowing clients and the UI to
  localize behavior without parsing provider prose.
- `CLAUDE_CODE_OAUTH_LOCAL_LIMITS_ENABLED=false` is an operational pause switch
  for Claude Code's process-local concurrency, minimum request interval and
  cooldown gate. It does not disable upstream error classification, replay
  safety, same-group retry boundaries, or persistent isolation for explicit
  revoked/disabled/permanent-credit failures. The default remains `true`; a
  restart is required after changing it.

Because the credential is only cooled (not quarantined), the next probe after
the cooldown expires self-corrects: if the account is still limited it is cooled
again; once the plan window resets it recovers with no manual intervention.

## Alternatives considered

- **Reclassify usage-limit as quota-exhausted → quarantine.** Rejected as the
  default: quarantine removes the credential from routing until an admin
  manually re-enables it, which is operational toil for a cap that resets on its
  own. Quarantine remains correct for genuinely dead credentials when the
  upstream explicitly reports an invalid, expired, or revoked token, a disabled
  account or organization, or permanently exhausted balance/quota. A bare
  `401/403` is not sufficient evidence.
- **Raise the global `maximumSubscriptionOAuthRetryAfter`.** Rejected: it would
  lengthen the cooldown for genuine burst limits too, hurting recovery latency
  for the common transient case.
- **Persistently disable every raw 401/403.** Rejected: status alone does not
  prove a credential is permanently dead and caused valid Claude accounts to
  be disabled after recoverable provider or proxy authorization responses.
- **Manual-only (operator disables the channel).** Rejected as the primary
  answer (it is the pre-existing workaround), but still available.

## Consequences

A usage-limit-exhausted subscription account is skipped by routing for its
reset window (or one hour when unknown, with an eight-day safety bound), and
traffic fails over to other channels instead of the client repeatedly hitting
the exhausted account every ≤15 minutes. No manual disable/re-enable is
required.

The channel remains enabled in the database. After the cooldown expires, the
existing half-open single probe verifies recovery: another usage-limit response
opens a new cooldown; a successful request closes the circuit. Providers that
omit a reset field, remove a window, or change its duration therefore recover
without an administrator editing the channel.

Administrative availability is intentionally separate from inference
availability. A cooled credential remains editable and queryable in `/channels`;
only client model requests are excluded from routing until reset.

An ordinary upstream rate limit remains a `429` end to end. While its local
cooldown is active, new requests receive the remaining `Retry-After` instead of
an unrelated `503` concurrency error. Compatible backup credentials are still
selected by the shared capacity retry path when available.

## Verification

- `TestIsSubscriptionOAuthUsageLimit`: usage-limit markers (English/Chinese)
  match only on a 429 on a subscription channel; a generic burst rate limit, a
  non-429, and a non-subscription channel do not.
- `TestSubscriptionOAuthCredentialCooldownForError`: usage-limit 429s use the
  1h fallback only when reset evidence is absent, honor exact five-hour and
  weekly windows, and cap malformed values at eight days; burst
  429s keep the ≤15m transient cooldown.
- `TestParseUpstreamRetryDelay*`: exact and relative reset metadata, wrapped
  WebSocket payloads, stale values, header fallback, and bounds.
- Claude policy tests verify that the observed Anthropic account-rate-limit
  response remains `upstream_rate_limited`, while explicit subscription usage
  messages become `upstream_usage_limit`, without calling a management API.
- Codex Wham tests verify opaque-429 correlation, reset propagation, fresh
  evidence requirements, and preservation of transient 429 behavior when the
  account remains available.
- Retry-state tests verify that usage-limit failover bypasses the generic 429
  toggle, releases an internal Responses WebSocket pin, and never bypasses
  downstream-output or explicit-channel safety guards.
- Capacity tests verify that management calls remain available during a usage
  cooldown and inference saturation without touching concurrency, pacing,
  recovery probes, or cooldown state.
- Capacity tests verify that a cached upstream rate limit remains HTTP `429`
  with `upstream_rate_limited` and the remaining `Retry-After`; active
  concurrency remains HTTP `503` with `oauth_channel_concurrency_limit`.
- Management tests verify that model discovery and manual inspection cannot
  quarantine or cool inference credentials, and scheduler selection excludes
  both Codex and Claude Code.
- Codex policy tests verify refresh-first handling and durable token-endpoint
  evidence; retry tests verify the ten-attempt request bound, cooldown/capacity
  separation, and request-local model-capacity exclusion.
