# ADR 0008: Normalize Claude cache-creation usage for tiered billing

## Context

Anthropic usage can report `cache_creation_input_tokens` as the authoritative
total while omitting, or only partially reporting, the ephemeral 5-minute and
1-hour split. The gateway's ordinary ratio billing already treats an unknown
remainder as the general cache-creation class, but the Tiered Billing path used
the split fields directly. That could make cache writes disappear from `cc`,
and could also undercount `len` and select a cheaper context tier.

The same usage may reach settlement through the normal billing-usage mapping or
directly through a caller that constructs `dto.Usage`, so one layer cannot be
the only protection.

## Decision

Claude's `cache_creation_input_tokens` is the authoritative cache-creation
total. Explicit 5-minute and 1-hour fields are known subcategories only. Both
the Claude BillingUsage mapping and `BuildTieredTokenParams` use the existing
normalization rule:

```text
remaining = max(total - known5m - known1h, 0)
normalized5m = known5m + remaining
normalized1h = known1h
```

Therefore, aggregate-only usage is billed as 5-minute/general cache creation,
partial splits retain the explicit 1-hour amount and put the unknown remainder
in 5-minute/general cache creation, and a split whose known sum exceeds the
aggregate is retained without producing a negative remainder. `len` always
includes the complete normalized cache-creation total, alongside Claude text
input and cache-read tokens.

The normalized fields remain the source for Tiered `cc` and `cc1h`. The
normalized `dto.Usage` also exposes their complete sum through
`PromptTokensDetails.CachedCreationTokens` and `InputTokens`; the nested Claude
BillingUsage preserves the raw upstream payload for audit. This decision does
not change ordinary ratio billing, OpenAI cache semantics, pre-consume, or the
settlement workflow.

## Alternatives considered

- Cherry-picking the upstream fix that only handles aggregate-only usage would
  still lose the remainder for partial splits and would not protect direct
  Tiered parameter callers.
- Adding a second cache-split helper in the billing package would create two
  normalization authorities. The existing relay conversion helper is already
  used across Claude conversions, so it is reused through the service wrapper.
- Treating every split field as authoritative would undercount aggregate usage
  whenever Anthropic omits one subcategory.

## Consequences

Cache-creation usage can no longer silently disappear from Tiered billing, and
long-context tier selection sees the full input context. Existing callers and
database formats remain compatible. A missing aggregate or split field is not
invented; only the unknown cache-creation remainder is assigned to the general
5-minute class.

## Verification

Regression tests cover aggregate-only, partial and full splits, missing
aggregate totals, known splits larger than the aggregate, complete `len`, and
final Tiered charges. The focused service, relay-conversion, and Claude relay
tests must pass, including their race variants.
