# Architecture exploration implementation status

- Snapshot date: 2026-10-14
- Phase: architecture exploration / phase 0 foundations
- Product implementation: not started

## Current boundary

The architecture exploration produced tested contracts, state machines and a PostgreSQL schema. It did
not produce a running product. There is no runtime adapter, sandbox integration, HTTP service or user
interface.

The foundation search is closed. OCC, Paperclip, Kelos, AgentOven, OpenFlows, workflow substrates and
the final market-scan candidates failed at least one applicable hard gate. CanDoItAll passed custom
provider routing, approval authority, restart reconciliation and cancellation, but failed the required
task/attempt budget mapping and current deployment-fit gates. The detailed evidence is indexed in
[qualification reports](qualification/README.md).

## Existing prototypes

| Path | Contents | Status |
|------|----------|--------|
| [`packages/product-api`](../packages/product-api) | Domain types, state machines, approval binding, capability tiers, JSON Schemas and OpenAPI 3.1 contracts. | Phase 0 prototype tested. |
| [`services/product-tasks`](../services/product-tasks) | PostgreSQL `product` schema and SQL constraint tests. | Schema exists; service not started. |
| [`integrations/occ`](../integrations/occ) | OCC public API mapping proposal. | Archived after Q1 and ADR-0012. |
| [`integrations/runtime`](../integrations/runtime) | Intended native session, event and approval correlation boundary. | Not started. |
| [`apps/task-workspace`](../apps/task-workspace) | Intended first-party web workspace. | Not started. |
| [`tests/integration-fit`](../tests/integration-fit) | Intended runtime and environment conformance tests. | Not started. |
| [`spike`](../spike) | Historical OCE/OpenClaw deployment scripts and evidence. | Closed; retained as qualification evidence. |

The API prototype currently includes:

- task, attempt, attention-request, environment and connection state machines;
- optimistic-version compare-and-set and terminal-state protections;
- canonical action hashing and authority-bound approval validation;
- executor-side policy version, action hash and lease-epoch revalidation;
- event and command envelopes with event-ingestion rules;
- runtime capability tiers;
- JSON Schemas and OpenAPI definitions for the proposed public endpoints; and
- tests for state transitions, approvals, schemas and OpenAPI contracts.

The PostgreSQL prototype currently includes:

- tenant-scoped composite foreign keys;
- one active attempt per task;
- one blocking attention request per attempt;
- one decision per attention-request version;
- idempotency keys and unique native event keys;
- per-task event sequencing;
- a transactional outbox;
- append-only audit records;
- immutable artifact and event records; and
- lease-fenced claim handling.

These are architecture prototypes. Their presence does not claim that a deployable lifecycle service
already exists.

## Verify the exploration code

Run from `architecture-explore/`:

```sh
npm ci
npm run typecheck
npm test
```

The SQL prototype requires PostgreSQL 16 or newer:

```sh
psql -v ON_ERROR_STOP=1 -d dacp \
  -f services/product-tasks/migrations/0001_product_core.sql
psql -v ON_ERROR_STOP=1 -d dacp \
  -f services/product-tasks/test/sql/0001_constraints.sql
```

## Next implementation slice

The first product implementation should exercise one complete lifecycle through replaceable runtime and
environment boundaries:

```text
create task
  -> dispatch attempt
  -> receive an attention request
  -> record an authorized decision
  -> resume or stop execution
  -> record artifacts and a result revision
  -> accept, reject or request rework
```

Before that slice expands, freeze the product-owned records and transitions for Task, Attempt,
AttentionRequest, Decision, Budget, Artifact, AuditEvent, ResultRevision and ResultReview. Use PostgreSQL
and a transactional outbox as the initial durability substrate. Add a workflow engine only if focused
evidence shows that this path is insufficient.

The remaining phase-0 work is to define tenant identity mapping, publish contract compatibility rules,
build a deterministic runtime simulator and create the first runtime-adapter conformance suite.
