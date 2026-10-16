# Implementation plan: issue #19 authoritative product schema

Tracker: [GitHub issue #19](https://github.com/AssahBismarkabah/dac-plane/issues/19) (epic #11)

## Objective

Create the first authoritative PostgreSQL 16 schema for the contracts frozen by
#18. Single Goose migration with every table, constraint, and index #20 needs
for transactional lifecycle persistence, plus an integration suite proving
fresh migrate, no-op reapply, and the issue's DB-level guarantees. No row
read/write code, no dispatch, no simulator.

## Sources

- `docs/architecture/contract-gate-18.md` per-family Go type, schema, fixture
  tables (normative field lists).
- `docs/architecture/README.md` ownership rules; `review-2026-10-15.md`
  invariants (transition plus outbox atomic, stable identities, fencing,
  uncertain state, digest-only audit).
- `docs/adr/0002-initial-application-boundary.md` (one app, PG plus outbox).
- `architecture-explore/services/product-tasks/migrations/` shape only
  (composite tenant PKs, CHECK enumerations, partial unique indexes); content
  is pre-freeze and OCC-flavored, never copied.
- Tavily research: goose v3 Provider (`-- +goose Up` required first, Down
  optional; `NewProvider(dialect, db, fs)`, `Up(ctx)`, `GetDBVersion(ctx)`,
  `embed.FS` supported) and outbox/SKIP LOCKED plus lease-epoch fencing plus
  trigger-based immutability patterns.

## Design

- `internal/platform/migrations/migrations/00001_product_schema.sql`: one
  initial migration, version 1. Public schema. All ids `uuid`, digests
  `text CHECK ^sha256:[0-9a-f]{64}$`, versions `bigint CHECK > 0`, amounts
  `bigint CHECK 0..2^53-1`, states `text CHECK IN (frozen Go lists)`.
- Tenant scoping: every row carries `tenant_id FK tenant(id)`; record PKs are
  `(tenant_id, id)` plus global `UNIQUE(id)`.
- One-active/idempotency: operation identity PKs (redelivery collides);
  `UNIQUE(tenant, provider, kind, native_ref)` on binding (native resource
  bound to one attempt); one claim row per resource with epoch fencing; one
  receipt per challenge; one cursor row per stream; single attempt-scoped
  active-operation uniqueness is deliberately NOT enforced (per-resource
  parallelism is legal; #21 serializes via version CAS).
- Retired one-time authority: partial `UNIQUE(grant_id) WHERE
  outcome='allowed' AND source='one_time_approval'` on authority_use.
- Stale generations: `BEFORE INSERT` trigger on authority_grant requiring
  `policy_generation` equal to the tenant's current policy generation.
- Immutability: single `reject_mutation()` trigger on audit_event,
  authority_use, decision, spend_receipt, result_revision, result_review,
  artifact_manifest, artifact, durable_event.
- Outbox: `uuid PK`, identity sequence, tenant/attempt scoping, pending
  partial index plus `claimed_at/claimed_by/lease_epoch/lease_expires_at` for
  `SKIP LOCKED` relay claim (#21 owns the relay).
- Down migration drops objects in reverse dependency order.
- `internal/platform/migrations/schema.go`: `//go:embed` FS plus
  `CurrentVersion = 1`; `NewProvider` signature unchanged.
- `migration_integration_test.go` (`//go:build integration`): fresh migrate,
  version observable, reapply no-op, plus probes for retired reuse, stale
  generation, cross-tenant cursor FK, expired spend CHECK, audit immutability,
  duplicate operation idempotency, duplicate claim. Unit test for embedded FS
  plus version without Docker.
- Frames, hook requests/responses, preconditions are protocol, not rows
  (digests live in audit/outbox); attempt lease columns are nullable support
  for #21 fencing.

## Ordered tasks

1. Rewrite this plan (done, committed separately).
2. Write the migration SQL.
3. Wire embed FS plus version constant; keep provider API stable.
4. Write unit plus integration migration tests.
5. Run gofmt, vet, unit tests, `just test-contracts`, integration, `just ci`.

## Verification

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
just test-contracts
go test -count=1 -tags=integration ./internal/platform/migrations/...
just ci
git diff --check
```

Issue path `internal/store/postgres/...` does not exist; the migration package
above is the correct equivalent. State Docker dependence in the PR.

## Boundaries

- No `SELECT/INSERT/UPDATE` row code (#20), no dispatch or relay (#21), no
  sqlc, partitioning, or capacity tuning.
- No baseline, diagram, or accepted-ADR edits; `architecture-explore/`
  read-only.
