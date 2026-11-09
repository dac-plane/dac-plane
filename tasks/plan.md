# Set a tenant's authority policy through trusted provisioning

Tracker: https://github.com/dac-plane/dac-plane/issues/115
Branch: `feat/provision-set-policy`, from `main` at `515f5e8`.

## Where the tree stands

Surveyed on 2026-11-08.

| Finding | Evidence | Consequence |
|---|---|---|
| No operation changes a tenant's policy | `internal/provision/cli.go` lists ten operations; tenant creation writes an empty generation-1 policy | Add `set-policy`. |
| Five qualification recipes update `policy` directly as `dac_migrate` | `justfile`: `qualify-codex-runtime`, `qualify-codex-artifact-store`, `qualify-task-cancellation`, `qualify-codex-containment`, `qualify-providers` | All five run the same update; switch all of them, not only `qualify-providers`. |
| The pilot script makes the same update | `deploy/compose/configure-pilot.sh` runs the published v0.1.0 image | It cannot use the command until a release carries it. Out of scope here, as the issue says. |
| `store.UpdatePolicyGeneration` already compares and sets the generation | `internal/platform/postgres/authority.go`; zero rows is `ErrStaleVersion` | The provisioner reads the policy, then updates under the generation it read. A concurrent change is a conflict. |
| The operation constraint lists every operation | `00013_qualify_harness.sql`; the principal, provider and identity constraints already require NULLs for an operation they do not name | Migration 16 changes only `provisioning_change_operation`. |
| The operations suite's rollout probe is schema 16 | `internal/platform/migrations/operationstest/00016_rollout_probe.sql`, `internal/operationstest/operations_test.go` | Move the probe to 17, as #100 and #106 did before. |
| v0.1.0 serves 13 to 15 and refuses 16 | `docs/releases/v0.1.0.md`; `TestRollingUpgradeAcrossMigration` asserts the old image refuses the next schema | Under ADR-0011 the guarantee is the other way round: the new release keeps serving 15 (`MinServeVersion` stays 13) so binaries roll before the migration. The PR says so. |

## Decisions

- `set-policy` replaces the whole policy: settings, ceiling and denies. `-denies` defaults to none.
- Validation is `identity.Policy.Validate` in `parse`, before the database opens, and again in the provisioner.
  JSON is decoded strictly: unknown deny fields and trailing data are usage errors.
- The same settings, ceiling and denies (denies in the same order) change nothing: no generation advance and no
  recorded change.
- A stale generation is `ErrConflict`. The integration test forces it through an unexported hook between the read and
  the update, set from `export_test.go`.
- Output: `{"tenant_id": ..., "generation": "N"}`.

## Steps

1. Migration 16, its down refusal, the store constant, schema tests, and the probe moved to 17.
   Verify: `go test ./internal/platform/migrations/`, `just test-operations-fixture`, the schema integration test.
2. `Provisioner.SetPolicy` and the `set-policy` CLI. Verify: unit tests for arguments; integration tests for the
   generation advance, idempotency, the recorded change and the stale-generation conflict.
3. The five recipes call `provision set-policy`; CLI reference section. Verify: `just docs-check`; the command
   applied to the dev stack.
4. `just ci-fast`, `git diff --check`, PR.
