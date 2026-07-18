# Architecture Decision Records

Create an ADR for a new workflow, persistent state model, retry policy,
authentication contract, billing behavior, or provider integration that changes
an existing architectural boundary.

Use this format:

```md
# ADR NNNN: Short decision title

## Context

What user or operational problem exists? Which constraints are non-negotiable?

## Decision

What will the system do, and which module owns the resulting state?

## Alternatives considered

Which existing workflow, extension, consolidation, or deletion options were
considered, and why were they not selected?

## Consequences

What becomes simpler, what compatibility remains, what must be monitored, and
when can temporary code be removed?

## Verification

Which behavior, failure, migration, and rollback tests prove the decision?
```
