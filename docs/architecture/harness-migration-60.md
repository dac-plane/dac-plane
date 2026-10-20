# Harness migration execution evidence (#60)

Issue #60 implements the execution half of the restricted migration boundary in
[ADR-0004](../adr/0004-restricted-migration-administration.md), following
[ADR-0005](../adr/0005-fenced-harness-migration.md). The product owns the Task,
Attempts, migration pass, claim, stage operation identities, budget and audit.
Runtime and environment adapters continue to own their native resources.

## Stage protocol

`execute` freezes the source scope in the same transaction as the authorized
command, before it returns. A later submission cannot be swept in during the
worker's first poll. Each eligible source is quiesced, then a distinct
target Attempt receives the remaining reservation from the same Task budget.
The worker prepares the target environment, starts the target runtime from
provider-reported context, marks the source Attempt migrated, and only then
requests source environment cleanup. The pass becomes completed only after all
captured items complete cleanup.

The target runtime and environment ports are registered separately by target
harness identity. Source cleanup uses the environment provider recorded in the
source Attempt's environment binding. A missing port leaves durable work
pending.

Provider operations use stable stage-specific IDs. Each item's current claim
epoch fences database settlement; the original epoch for each external stage is
retained separately so a replacement worker can recover the same request and
receipt. A stale worker cannot settle after claim takeover. Replaying an exact
stage operation relies on the provider's stable-operation idempotency contract;
the simulator retains its effect ledger across worker and pool restarts.

If budget coverage is lost after source quiescence, the item enters a durable
`aborting` state instead of failing the pass immediately. The worker stops the
target environment when it was provisioned, then stops the source environment.
Each stop has its own stable operation and exact receipt. The migration remains
executing and retains the tenant slot while either result is uncertain or an
environment adapter is unavailable. Only after all applicable stop receipts
are accepted does the worker fail the Attempts and pass, release the target's
unused reservation, and free the slot. This cleanup path applies both when
budget is lost before target allocation and after target environment setup.

## Evidence

`TestMigrationExecutesQuiescePrepareCutoverAndCleanup` runs the full path with
PostgreSQL and runtime/environment simulators. It loses the first quiesce reply,
then creates fresh workers and database pools and expires the maintenance claim
between stages. It asserts a completed pass/item, a migrated source Attempt, an
executing successor Attempt, retained stage epochs, and exactly one external
effect for each stage despite two deliveries of the quiesce operation.

`TestMigrationExecutionSnapshotsTenantSourceAttempts` proves the captured
source set does not grow when a submission arrives after execute but before the
worker's first claim, or after a later claim takeover. It also
proves an Attempt outside the executing/running boundary is durably unresolved,
fails the pass before a provider operation is created, and does not enroll late
work. `TestEmptyMigrationCompletesAndReleasesTenantSlot` proves a pass with no
source work completes without retaining the tenant slot.
`TestMigrationFailsCleanlyWhenReservationExhaustsDuringQuiesce` proves the
quiesced source is stopped before a source-only budget failure releases the
slot. `TestMigrationRechecksBudgetBeforeStartingTarget` injects delayed source
usage after target provisioning, loses each environment-stop reply across fresh
workers, and proves the pass and slot remain active until both exact stop
receipts are recovered; no target runtime start occurs and its reservation is
released only after cleanup.
`TestExecutingMigrationCannotBeCancelledWithoutCleanupEvidence` proves an
executing pass remains visible and retains its tenant slot when cancellation is
refused. The end-to-end test also rejects cancellation at each in-flight stage,
keeps a malformed target receipt uncertain until the stable operation replays,
and proves a stale worker cannot mutate state after a new holder settles source
quiescence. Existing migration administration tests cover preview/hold,
permission, revocation, replay and downgrade boundaries.

The migration acceptance tests include recovery from lost stop replies under
stable operation identities. Verification results for this cleanup correction
are recorded in `tasks/plan.md` and the PR checks; older baseline runs above do
not stand in for checks on the corrected head.

## Limits

This proves the product protocol against deterministic simulators, not a
production provider. Context references do not prove semantic continuity, and
restart-from-artifacts/context is not exact native-session resume. Unknown
provider outcomes and unavailable adapters remain pending rather than being
converted to success. Cancellation after execution starts is explicitly
refused; automated reversal of a partial external migration is not supported.
