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

## Billing and persistence

- Quota computation must never overflow into a negative charge or credit.
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
