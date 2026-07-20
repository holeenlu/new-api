# ADR 0001: Official API pricing catalogues

Status: Superseded — the hardcoded catalogues (`controller/official_api_price_catalog.go`
and the `OFFICIAL_*_PRICING_*` sync sources) were removed. Model-price sync now
relies only on live sources: the models.dev preset, the official ratio preset,
and other new-api/one-api instances that expose a pricing endpoint. A hardcoded
per-model catalogue proved unmaintainable — every new upstream model required a
source-code edit and redeploy before it could be priced, which is exactly the
"incomplete prices" failure this record's Consequences warned about. The
sections below are retained as historical context.

## Context

Model pricing sync previously depended on third-party catalogues or a selected
channel's pricing endpoint. ChatGPT/Codex and Claude Code subscription OAuth
channels do not expose a reliable, supported per-token pricing endpoint. Using
their credentials to infer prices is both inaccurate and an unnecessary OAuth
request.

Administrators still need an explicit source for OpenAI API and Anthropic API
token prices that can be reviewed and applied through the existing model-price
sync workflow.

## Decision

`controller/official_api_price_catalog.go` owns two synthetic, read-only
pricing sources: OpenAI official API pricing and Claude Code-compatible
Anthropic official API pricing.
Each source is a versioned local catalogue of public API USD-per-million-token
prices and has an official documentation URL for display.

The existing `ratio_sync` controller converts those entries into local input,
completion, cache-read, and cache-create ratios, then returns them through the
same difference and administrator-confirmed save flow as every other source.
The catalogue owns no persistent state. Existing model-pricing options remain
the sole persistence owner.

Actual Codex and Claude Code OAuth channels remain excluded from price sync.
The Anthropic catalogue is compatible with Claude Code model names only where a
matching public Anthropic API model and token price are documented. Subscription
fees are not converted into token prices.

An unknown model is omitted, never assigned a zero or guessed price. A
catalogue source cannot make a network request and its endpoint is not editable
in the selection UI.

## Alternatives considered

- Query the OAuth subscription upstream: rejected because it has no stable
  token-price contract and creates avoidable account traffic.
- Scrape vendor pricing webpages at sync time: rejected because page markup is
  not an API contract and can silently produce partial or stale data.
- Treat monthly subscription fees as per-token prices: rejected because usage
  limits and plan terms make that conversion misleading.

## Consequences

Price updates require a source-code catalogue update when official public API
prices change. This makes every change reviewable and deterministic, while the
administrator retains control over which differences are written.

The catalogues intentionally do not cover private, preview, future, or
subscription-only model names until an official public API price is documented.

## Verification

- Ratio-sync controller tests verify both virtual sources are listed and are
  served without an HTTP request.
- Tests verify the existing Codex and Claude Code OAuth channels remain
  excluded and never probed.
- Frontend typecheck verifies the virtual source IDs use fixed endpoints.
