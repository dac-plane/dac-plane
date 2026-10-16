# Implementation plan: issue #20 transactional lifecycle persistence

Tracker: [GitHub issue #20](https://github.com/AssahBismarkabah/dac-plane/issues/20) (epic #11)

## Objective

Persist lifecycle mutations with transactional consistency, optimistic
concurrency, and command idempotency on top of the #19 schema. State, audit,
durable event, and outbox rows commit or roll back together; stale versions
update zero rows; idempotency keys replay safely; tenants stay isolated.
No dispatcher loops (#21), no simulator (#22).

## Sources

- `docs/architecture/review-2026-10-15.md` invariants (transition plus outbox
  atomic, stable identities with dedupe, fenced leases, uncertain outcomes,
  digest-only audit).
- `docs/architecture/README.md` ownership rules; ADR-0002 modular monolith
  (`internal/` boundaries, providers never write the DB).
- `docs/architecture/contract-gate-18.md` per-family Go type and fixture tables.
- `internal/lifecycle` pure transitions returning `(record, Change, error)`;
  `Change.Audit()` / `TransitionError.Audit()` build audit events.
- `internal/platform/migrations/migrations/00001_product_schema.sql` table
  and constraint reference; `internal/platform/postgres/pool.go` connection
  and credential-hygiene pattern.
- `architecture-explore/` evidence only (subagent survey pending for
  Plane-Bot transactional and prototype store patterns).

## Design

- Location is `internal/platform/postgres/` (the issue's
  `internal/store/postgres/` path does not exist; no parallel tree).
- Hand-written pgx/v5 SQL only (no generated SQL per the issue).
- `tx.go`: `UnitOfWork` wrapping `pgx.Tx`, begin/commit/rollback, no nesting.
- Grouped repositories: `lifecycle.go` (task/attempt/attention/decision),
  `budget.go`, `authority.go`, `artifacts.go`, `observations.go`
  (cursor/binding/turn/claims/harness/migration), `history.go`
  (audit/durable-event/outbox writes). Domain logic stays pure in
  `internal/lifecycle` etc.; the store maps rows and never reimplements
  transitions.
- Every mutation: `UPDATE ... WHERE tenant_id=$ AND id=$ AND version=$`
  (zero rows means stale); insert the `Change`-derived audit, durable event,
  and outbox rows in the same tx.
- Idempotency on stable identities the schema uniques
  (`operation_id`, spend `nonce`, `durable_event(tenant,record,version)`).
- Restart resume readers (open outbox rows, held/expired claims, uncertain
  operations, cursor positions); no relay loop.

## Ordered tasks

1. Rewrite this plan (done, committed separately).
2. `tx.go` unit of work plus error mapping (stale version, cross-tenant
   invisibility, idempotent replay outcomes).
3. Grouped repositories with row mappers.
4. Atomic commit helper (state plus audit plus durable event plus outbox).
5. Idempotent replay (same key plus payload returns stored outcome;
   different payload rejected before any write).
6. Resume readers.
7. Unit tests (mappers, error mapping) plus integration tests per acceptance
   bullet behind `//go:build integration`.
8. `gofmt`, `go vet`, `go test ./...`, `just test-contracts`, integration,
   full `just ci`, `git diff --check`.

## Verification

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
just test-contracts
SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" go test -count=1 -tags=integration ./internal/platform/...
just ci
git diff --check
```

## Boundaries

- No dispatcher or claim loops (#21), no read replicas, sharding, or
  simulator work (#22).
- No new tables unless #19 proves insufficient (then a new migration
  version, never an edit to `00001`).
- No baseline, diagram, or accepted-ADR edits; `architecture-explore/`
  read-only.
