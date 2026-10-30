# Issue #105 — Reuse PostgreSQL across integration fixtures

Tracker: https://github.com/dac-plane/dac-plane/issues/105
Branch: `feature/105-shared-postgres-fixture`, from `main` at `00fbf30`.
Observed during #102. Test support only; no production migration or persistence change.

## Current state

`internal/testsupport/postgres.Start(t)` runs a new pinned `postgres:16.15-alpine` container per
call and terminates it in `t.Cleanup`. Every fixture in nine packages calls it, and every caller then
migrates the empty database itself (`Up`, `UpTo(version)`, or no migration at all):

| Package | Integration tests | Fixture shape |
|---|---|---|
| `internal/application` | 164 | `UpTo(version)` then `store.Open` |
| `internal/platform/postgres` | 38 | `Up`; `pool_integration_test` needs an empty database |
| `internal/worker` | 21 | `Up` |
| `internal/platform/migrations` | 15 | empty, `Up`, `UpTo(10)` then data then `Up`, two concurrent locked `Up` |
| `internal/adapterwire` | 13 | `Up` |
| `internal/httpapi` | 9 | `Up` |
| `internal/provision` | 7 | `Up` |
| `internal/bootstrap` | 4 | `NewLockedProvider().Up`; `New()` against an empty database |
| `internal/authn` | 1 | `Up` |

No integration test calls `t.Parallel`, no package defines `TestMain`, and all integration test
files carry `//go:build integration`. Container start and stop dominate: about 260 containers per
full integration run, serialised within each package.

Isolation facts that make a shared server safe:

- Migrations and the store create only database-local objects. No `CREATE ROLE`, `ALTER SYSTEM`,
  `ALTER DATABASE`, extension, tablespace or `pg_stat_activity` use was found.
- Every advisory lock (goose `PostgresSessionLocker`, `pg_advisory_xact_lock` in attention,
  governed actions, migration admin and the surface-event trigger) is keyed within the current
  database, so fixtures on separate databases cannot contend.
- The container user is the image superuser, so it may `CREATE DATABASE` and `DROP DATABASE`.

## Prior art

- Testcontainers for Go guidance and the Storj engineering write-up: one container per package in
  `TestMain`, one database per test when tests write data; Ryuk reaps containers if the process dies
  before cleanup.
- `peterldowns/pgtestdb` and `pgdbtemplate`: clone a migrated template with
  `CREATE DATABASE ... TEMPLATE`, keyed by a migration hash; drop on success.
- Keycloak's JUnit test framework: the framework owns the database lifecycle with a shared scope and
  opt-in reuse; tests declare what they need instead of starting infrastructure.

This plan adopts the first pattern unconditionally and gates the template step on measurement.

## Implementation

1. **Baseline.** Before changing code, time the recipes below on the same machine, with images
   pre-pulled and nothing else running, and record exact commands, Go and Docker versions and
   wall-clock times:
   - `time just test-integration ./internal/platform/postgres/`
   - `time just test-integration ./internal/worker/`
   - `time just test-integration-race ./internal/application/` (the ~14 min shard from #102)
   - `time just test-integration-other-race`
2. **Shared server.** In `internal/testsupport/postgres`, start the container lazily, at most once
   per test process, behind a mutex-guarded once. A start failure is remembered and fails every
   later fixture with the same error instead of retrying.
3. **Database per fixture.** `Start(t)` keeps its signature and meaning: it returns the URL of a
   fresh, empty database. It connects to the server's maintenance database, creates
   `t_<sanitised test name>_<random hex>` (truncated to the 63-byte identifier limit and quoted with
   `pgx.Identifier`), and registers cleanup before returning. Cleanup runs
   `DROP DATABASE ... WITH (FORCE)` so a leaked pool or connection cannot block it. Because
   `t.Cleanup` runs last-registered-first, callers' `pool.Close` still runs before the drop. Callers
   keep migrating to whatever version they need, so `UpTo(version)`, empty-database and concurrent
   locked-migration tests stay meaningful with no edits.
4. **Process-exit cleanup.** Add `testpostgres.Main(m *testing.M) int`, which runs `m.Run()` and
   terminates the container if one was started. Each of the nine packages gets a
   `main_integration_test.go` (`//go:build integration`) with
   `func TestMain(m *testing.M) { os.Exit(testpostgres.Main(m)) }`. Failing tests still reach the
   termination; a panic or `-timeout` kill falls back to Ryuk, which must remain enabled (it is today).
   `Start` without `Main` fails the test with a message naming the missing `TestMain`, so a new
   package cannot leak a container silently.
5. **Isolation and race proof.** Add tests in `internal/testsupport/postgres`:
   - two fixtures in parallel subtests each create a table and row; neither sees the other's data;
   - the database is gone after its subtest's cleanup;
   - a second `Start` in the same process reuses the server (same host and port);
   - run under `-race`.
6. **Template step, only if measurement justifies it.** After step 4, profile one fixture. If `Up`
   is still a significant share of package time, add `StartMigrated(t)` that clones a template
   migrated once per process to `migrations.CurrentVersion`, and move only the `Up` callers to it.
   Empty-database and `UpTo` callers keep `Start`. Otherwise record the number and skip it.
7. **Record results.** Re-run the step 1 commands and put before/after times, environment and any
   template-step decision in the PR Evidence section. Run `just ci-fast` and `git diff --check`.
   CI already runs the integration recipes, so no new recipe is needed; the fixture's own tests run
   under the existing `test-integration*` recipes.

## Verification

- All step 1 recipes pass after the change, under race where the recipe uses it.
- `docker ps -a --filter label=org.testcontainers=true` shows no leftover PostgreSQL container
  after a passing run, after a run with a deliberately failing test, and after Ctrl-C (Ryuk).
- `internal/platform/migrations` schema-version, `UpTo(10)`, concurrent locked `Up` and rollback
  tests pass unchanged.
- Measurable improvement on each focused recipe, with exact commands and timings recorded.

## Results

Environment: macOS arm64, 10 CPUs, Docker 29.8.2, Go 1.26.10, just 1.57.0, image pre-pulled, build
cache warmed with `-run '^$'` before timing. Before ran on an export of `main` at `00fbf30`; after
ran on this branch. An unrelated long-running container shared the host during both runs.

| Command | Before | After |
|---|---|---|
| `just test-integration ./internal/platform/postgres/` | 89s | 16s |
| `just test-integration ./internal/worker/` | 51s | 11s |
| `just test-integration-race ./internal/application/` | 960s | 491s |

`just test-integration-race` over the other seven PostgreSQL packages and the fixture's own tests
passes in 29s. `just ci-fast` and `git diff --check` pass. After every run, including a deliberately
failing fixture test, only the Ryuk container remained.

Template step: skipped. A probe of `newFixture` under `-race` measured about 0.34s per fixture,
migrations included (2.8s for the first, which starts the container). Across 164 application tests
that is about 55s of 491s; the remainder is test-body time, which a template cannot reduce.

## Follow-up on this branch: parallel application tests

With a database per fixture, the remaining application time was serial test bodies. An audit found no
mutable package-level state in the application tests or the packages they drive (the only shared
test-support state, `wiretest`'s registry, is mutex-guarded and keyed per test), no `Setenv`, and
only `httptest` ports. Every top-level application integration test now calls `t.Parallel()`
(164 tests). Fixture pools default to 10 connections each on 10 CPUs, so the shared container now
runs with `max_connections=500` instead of PostgreSQL's default 100.

| Command | Serial (after #105) | Parallel, three consecutive runs |
|---|---|---|
| `just test-integration-race ./internal/application/` | 491s | 186s, 158s, 176s |

No run reported a failure, data race or connection exhaustion; only Ryuk remained afterwards.
Against the original 960s, the shard is about 5.5 times faster.

## Limits

One container per package process, not per `go test` invocation: `go test ./...` still starts one
per package, in parallel. Sharing across processes (testcontainers reuse or an external
`DATABASE_URL`) and keeping failed-test databases for inspection, as pgtestdb does, are out of scope
unless a reviewer asks for them.
