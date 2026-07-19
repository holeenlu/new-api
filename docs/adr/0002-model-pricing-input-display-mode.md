# ADR 0002: Persist model pricing input display mode

## Context

The model metadata editor supports entering token prices either as local ratios
or as USD-per-million-token prices. Both representations intentionally produce
the same runtime ratio settings, so the prior implementation had no persisted
state from which it could restore the administrator's chosen editor view after
a refresh.

## Decision

`ModelPricingInputMode` is a validated Option map owned by the model-pricing
settings domain. It records only non-default `"price"` selections by model
name; absence means the ratio editor. It does not affect runtime charging,
model routing, or the fixed per-request `ModelPrice` mode.

The metadata editor writes the preference atomically with the price-ratio maps
through the existing `PUT /api/option/model-pricing` endpoint. Renaming or
switching away from the token-price display removes stale entries.

## Alternatives considered

- Infer the mode from `ModelRatio`: rejected because both input views write the
  same ratio and cannot be distinguished after persistence.
- Store it in browser local storage: rejected because it would differ by
  browser and administrator rather than reflecting the configured model.
- Add it to the model metadata table: rejected because it is pricing-editor
  state, while model metadata owns catalog identity and presentation fields.

## Consequences

Existing installations default to the ratio view without migration. The option
is harmless to all relay paths and is not included in upstream price sync.

## Verification

- The model-pricing endpoint validates only `ratio` and `price` values.
- The editor reloads the stored view and atomically persists it with price
  configuration updates.
