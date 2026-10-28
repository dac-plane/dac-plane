# ADR-0013: Product-owned execution briefs

- Status: Accepted
- Date: 2026-10-28
- Issues: [#99](https://github.com/dac-plane/dac-plane/issues/99),
  [#91](https://github.com/dac-plane/dac-plane/issues/91)

## Context

The product lifecycle owns tasks, attempts, budgets, results and acceptance, but the implemented Task
record contains only identity and lifecycle state. Its runtime `start_turn` dispatch carries budget and
scheduling data without the requested outcome. Deterministic simulators can react to an operation ID;
a real agent runtime cannot perform meaningful work without an objective and acceptance criteria.

The reviewed historical architecture assigns those facts to the Task. Runtime, environment and
artifact providers remain replaceable execution roles. Putting a fixed prompt in the first runtime
adapter would therefore qualify only transport wiring and would make the adapter, rather than the
product, the accidental owner of requested work.

ADR-0011 also requires a new binary to serve the current schema before its migration runs. Schema 13
has no task-brief columns, and tasks created by older binaries carry no brief. The first implementation
must preserve those records without inventing an objective.

## Decision

DAC owns an immutable execution brief containing a bounded objective and an ordered, non-empty list of
bounded acceptance criteria. Every new task submission requires the brief. It participates in command
idempotency, appears in task and submission reads, and is delivered to every runtime `start_turn`,
including rework and migration-restart attempts.

Task storage treats the two fields as a pair. Schema 14 adds nullable columns because genuine legacy
tasks have no truthful brief. The schema-13 compatibility path stores the brief in the immutable
submission command result and the durable dispatch payload; reads use that result until the columns
exist. Migration 14 backfills only a brief that was actually recorded by a compatible binary.

Provider names, model identifiers, credentials, workspace locations and environment configuration are
not part of the brief. They remain provider registration, adapter configuration or future product
profile concerns.

## Alternatives considered

- **Inject a qualification prompt in the first adapter:** rejected. It proves a demo-specific adapter,
  not that DAC can delegate user-requested work.
- **Make schema-14 columns immediately mandatory:** rejected. Older tasks have no truthful values, and
  the new binary must run against schema 13 before migration.
- **Put runtime, model or workspace selection in the brief:** rejected. Those fields would couple the
  product-owned outcome to replaceable provider and environment choices.
- **Infer acceptance from the objective:** rejected. Acceptance criteria are owner-visible product
  facts; a runtime-generated interpretation is evidence, not authority to redefine success.

## Consequences

- Surface API clients must supply an objective and acceptance criteria for every new task.
- Runtime adapters receive meaningful provider-neutral work without reading DAC's database or calling
  an internal API.
- Legacy tasks may omit the brief and remain readable. A later contraction may require non-null storage
  only after no supported deployment contains such records.
- The #74 operations-only migration moves to schema 15 so production schema 14 and the isolated rollout
  probe remain distinct.
