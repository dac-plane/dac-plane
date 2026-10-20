# Issue #60 — Fenced tenant-wide harness migration

Tracker: https://github.com/dac-plane/dac-plane/issues/60
Branch: `feature/60-harness-migration`, from merged #28.

## Direction

Keep migration in the existing product lifecycle and PostgreSQL worker. A preview and execute command record authorized intent; neither proves that work moved. Source quiescence, target preparation, runtime restart, cutover, and source cleanup require exact provider receipts bound to stable stage operations and their original claim epochs. Continuity is restart-from-artifacts/context, never exact native-session resume.

## Implementation

- [x] Add typed stage request/receipt contracts and schemas; reuse the existing interrupt, provision, start-turn and stop methods.
- [x] Freeze one source Attempt snapshot atomically with the authorized execute command; do not enroll later submissions before first claim or after takeover.
- [x] Persist stage operation identities, context references, target environment reference and each original stage epoch separately from the current claim epoch.
- [x] Add a fresh target Attempt on the same Task, transfer only the unused reservation within the same budget, and preserve the source Attempt as `migrated` history.
- [x] Drive source quiescence, target environment provision, target runtime restart and source environment cleanup through registered runtime/environment ports.
- [x] Keep uncertain stage sends recoverable by replaying the same provider operation and exact stage payload; a completed simulator effect is returned without duplication.
- [x] Keep a failed/unavailable adapter or missing role port pending; never infer migration success from silence or source/target resource presence.
- [x] Reject cancellation after execution starts because this protocol cannot safely reverse a partially applied provider effect. Pre-execution cancel remains supported.
- [x] Add restart/takeover acceptance through the full four-stage path, including lost and malformed replies, cancellation refusal at each in-flight stage, stale-worker settlement, and per-stage effect-count assertions.
- [x] Add focused and race-enabled migration recipes.
- [x] Add explicit acceptance for malformed target-stage receipts, cancellation at each in-flight stage, concurrent stale-worker settlement, ineligible sources, and empty snapshots.
- [x] Add ADR and architecture implementation evidence note; document remaining limits.
- [x] Fix the reviewed budget-exhaustion path: keep the migration pass/claim active while exact stop receipts are recovered for the source and any provisioned target; only then fail Attempts and close the pass.
- [x] Run migration acceptance and race tests, contract guards, full `just ci`, review the diff, and update PR #62.

## Observed evidence so far

- Unit packages passed: contracts, lifecycle, simulator, worker, PostgreSQL and migration packages.
- `just test-migration` passed uncached after implementation; the full path uses PostgreSQL, retained simulator ledgers, fresh workers/pools and forced claim takeover between stages. It asserts each stage effect is applied once, quiesce is replayed after a lost reply, and a malformed target receipt remains uncertain until exact replay.
- `just test-migration-race`, `just test-contracts`, `just fmt-check`, and `just ci` passed. `just ci` includes unit/race tests, full integration race suites, vulnerability scanning and workflow linting.
- `TestMigrationRechecksBudgetBeforeStartingTarget` exercises delayed source usage after target provisioning, worker restart, and lost acknowledgements for both stop stages. It requires the pass and slot to remain active until exact stop receipts are recovered, with each external stop effect applied once and no target runtime start.
- On the final diff, `just test-contracts`, `just fmt-check`, `git diff --check`, `just test-migration-race`, and `just ci` passed. Full CI included uncached integration race suites, vulnerability scanning (`No vulnerabilities found`), and workflow linting.
- `TestMigrationExecutionSnapshotsTenantSourceAttempts`, `TestEmptyMigrationCompletesAndReleasesTenantSlot`, and `TestMigrationStaleWorkerCannotSettleAfterTakeover` cover execute-time scope freezing, empty/ineligible sources, and stale settlement fencing.
- The existing migration snapshot integration test passed alongside that path.
- The Command Line Tools SDK link fails on this host; integration tests use the active Xcode SDK through the repository's existing `sdkroot` recipe.

## Remaining boundaries

No production provider has been qualified. Exact native session resume is not supported. Executing migration cancellation is refused rather than pretending it can roll back an unknown external effect. An uncertain operation or unavailable provider remains pending until an exact stable-operation receipt can be recovered; no timeout converts that uncertainty into success.
