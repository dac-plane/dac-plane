# ADR-0007: Product waits — SQL/outbox vs Temporal

- Status: Open (SQL/outbox used provisionally)
- Spec: §10, §26 (workflow engine row), §31 (durability)

## Context
The spec allows "SQL/outbox or Temporal, selected after a durability spike" and forbids a second
infrastructure controller.

## Provisional choice
Milestone-1 schema includes a transactional `outbox` table and per-task sequence, which is required by §10
regardless. Temporal is not introduced.

## Closure evidence
Failure-injection tests proving decisions and task state survive restarts without repeating effects (A04, A05).
