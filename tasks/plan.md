# Issue #83 — Expose migration administration and authority revocation

Tracker: https://github.com/dac-plane/dac-plane/issues/83
Branch: `feature/83-migration-administration-api`, from `main` at `ef636c4`.
Parent: #65 (M2: Deployable control plane).

## Direction

Extend Surface API v1 over the existing transport-neutral migration administration and authority
revocation commands. Handlers remain thin and never access PostgreSQL. This issue adds no domain
behavior, permission model, database migration, listener or provisioning endpoint.

The historical connection revocation in `architecture-explore` is evidence only and is not the
authority-grant contract adopted by the current application.

## Implementation

1. **Surface contract**
   - [x] Add schemas for migration creation, empty commands, preview evidence and migration views.
   - [x] Extend OpenAPI 3.1 with migration create/read/preview/hold/release/execute/cancel and
     authority-grant revocation.
   - [x] Keep `Idempotency-Key` on every mutation; require `If-Match` only for migration transitions.
2. **HTTP layer**
   - [x] Project `application.MigrationView` as the pass plus optional preview evidence; do not expose
     the maintenance claim.
   - [x] Add thin handlers for every route with the established strict JSON, ETag, Problem Details,
     visibility and replay rules.
   - [x] Conceal pass-specific administrator denial and unowned authority grants as not found.
3. **Verification**
   - [x] Extend schema/Go agreement and bidirectional OpenAPI route conformance tests.
   - [x] Exercise migration administration through HTTP, including completion, cancellation,
     concealment, permission revocation and nonce replay.
   - [x] Exercise standing-authority revocation through HTTP and verify durable revocation evidence.
   - [x] Run `just test-contracts`, `just test-surface`, `just test-surface-race`, `just ci` and
     `git diff --check`.
4. **Delivery**
   - [x] Commit as `feat(api): expose migration administration and authority revocation`.
   - [x] Open a focused PR using the repository template with `Closes: #83`.

## API

All paths are under `/v1/tenants/{tenant}`.

| Route | Application call | Success |
|---|---|---|
| `POST /migration-passes` | `CreateMigration` | `201`, `Location`, `ETag` |
| `GET /migration-passes/{migration}` | `GetMigration` | `200`, `ETag` |
| `POST /migration-passes/{migration}/preview` | `AdministerMigration(preview)` | `200`, `ETag` |
| `POST /migration-passes/{migration}/hold` | `AdministerMigration(hold)` | `200`, `ETag` |
| `POST /migration-passes/{migration}/release` | `AdministerMigration(release)` | `200`, `ETag` |
| `POST /migration-passes/{migration}/execute` | `AdministerMigration(execute)` | `202`, `ETag` |
| `POST /migration-passes/{migration}/cancel` | `AdministerMigration(cancel)` | `200`, `ETag` |
| `POST /authority-grants/{grant}/revoke` | `Revoke` | `204` |

Creation takes `from_provider_id` and `to_provider_id`. Other mutations take a strict empty JSON
object. The migration response contains `migration` and an optional `preview` with
`from_version`, `to_version` and `previewed_at`.

## Boundaries

No new permissions, role model, provider administration, grant read route, provisioning over HTTP,
public maintenance claim, provider-transfer claim or architecture decision. Migration execution
remains asynchronous and evidence-backed by the existing worker.

## Observed verification

- `just test-contracts`: passed.
- `just test-surface`: passed.
- `just test-surface-race`: passed.
- `just ci`: passed, including integration race partitions, adapter conformance, vulnerability scan
  and workflow lint.
- Review mutation: inverting administrative-resource concealment made both new HTTP integration
  scenarios fail with exposed `403` responses; restoring it returned the scenarios to green.
- `git diff --check`: passed.
