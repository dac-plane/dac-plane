# Security and lifecycle evidence — issue #28

This work follows the active architecture, ADR-0003 and ADR-0004. It is simulator
acceptance evidence, not an M1 freeze or production security certification.
Exploration remains unchanged.

## Spend approval admission

The command boundary locks the Attempt/Task scope and spend challenge before
sampling PostgreSQL `clock_timestamp()`. Caller `asserted_at` is evidence, not
authority to extend expiry. An already-consumed challenge cannot authorize a new
command nonce. The original nonce/fingerprint still replays its original result,
including after expiry. Accounting reservation and spend authorization remain
separate, and receipt plus dispatch still commit atomically.

Regression evidence:

- `TestSecuritySpendBackdatedAssertionCannotExtendExpiry` reproduced admission
  of a backdated proof after actual challenge expiry before the fix.
- `TestSecuritySpendProofCannotReplayThroughAnotherCommand` reproduced consumed
  proof admission through another nonce; original command replay is preserved.
- `TestSecuritySpendConcurrentProofHasOneWinningCommand` asserts one accepted
  command, receipt, reservation and external runtime effect.
- `TestSecuritySpendExpiryRecheckedAfterChallengeLock` blocks on an observed
  database lock, expires the challenge, then verifies no receipt or dispatch.
- `TestSecuritySpendTermsCannotBeSubstituted` refuses substitution of tenant,
  principal, Task, Attempt, operation, amount and unit without accounting writes.

## Authenticated usage accounting

`usage.reported` carries only a valid unit and bounded cumulative total, not
product record IDs. The authenticated event binding supplies Tenant/Attempt/
provider/operation. The product resolves the Task budget and its Attempt-owned
reservation, locks accounting rows, and commits accounting, audit/events and
ingestion cursor together. Missing budgets or reservations are refused rather
than treated as unconstrained. Missing or null cumulative evidence is not zero.

Equal or older cumulative observations acknowledge progress without reducing
recorded consumption. Late usage remains chargeable after terminal reservation
release and records overshoot; it does not reopen the reservation or revive work.
Integrity/accounting evidence does not prove provider-meter correctness or
implement the post-action/failure hooks still required at #30.

`TestSecurityUsageLateReportsRemainAccountedAfterTerminal` failed before this
entry was implemented. It covers released accounting, overshoot, stream replay,
equal/older cumulative observations, wrong units and payload identity smuggling.
`TestSecurityUsageNonLiveReportsCannotChangeAccounting` covers shadow/off
accounting, lifecycle and primary-cursor isolation. The JSON Schema and Go
agreement test cover required zero, integer bounds and unknown fields.

## Restricted migration administration

[ADR-0004](../adr/0004-restricted-migration-administration.md) defines the narrow
permission and command boundary added here. Trusted setup provisions a persisted
tenant/principal permission. Its row lock covers command work and exact nonce
replay, so revocation cannot race past authorization. There is no self-grant
command. Cross-tenant records remain absent even to another tenant's administrator.

Create records intent. Preview locks distinct live harnesses, stores their exact
qualification versions and database time, and expires after five minutes. Execute
requires that same snapshot. Hold/release invalidates it; cancellation before
execution frees the tenant-wide slot. Existing domain rules refuse hold/cancel
once executing; actual transfer cancellation belongs to #60.

One reusable tenant administrative claim avoids concurrent passes and permanent
resource-key collisions. The claim's provider is an anchor, not a compute binding.
The worker explicitly acquires scheduled work only when linked to an executing
pass, and rechecks qualifications under scope locks for acquisition and renewal.
Database time is used for reaper selection and again after claim-lock waits.
Takeover advances the epoch without changing preview or pass state. There is no
administrative completion/failure method and no provider call on this path.

Migration 00007 preserves legacy pass records without fabricated previews or
permissions. Its downgrade refuses populated permission, snapshot or slot data.

## Acceptance index

Each row identifies executable evidence rather than introducing a parallel
integration framework. Existing tests retain their names and layer boundaries.

| Invariant | Executable evidence | Product/effect assertions |
|---|---|---|
| Tenant/provider/Attempt/operation identity | `TestEventForgedEnvelopesRefusedBeforeMutation`, `TestEventBindingOfAnotherAttemptRefused`, `TestGovernedActionCannotReportRuntimeLifecycle`, `TestSecurityMigrationCrossTenantReadAndHoldPreventAcquisition` | No forged bindings, primary cursor, executable operation or cross-tenant access; bounded refusal evidence is permitted |
| Partial/lost/duplicate cancellation | `TestSecurityCancelLostAcknowledgementPartialStopsAndRestart`, `TestEventStaleEnvironmentStopCannotConvergeCancellation`, `TestCancelUsesPreviouslyObservedEnvironmentStop` | Lost cancel acknowledgement redelivers; one stop cannot terminate; restart retains intent; duplicate observations release budget once |
| One budget ceiling | `TestSecurityBudgetConcurrentReservationsRespectOneCeiling` (repository), `TestSecurityReworkRaceReservesAndDispatchesOnce` (commands), `TestSecurityLateUsageRefusesReworkWithoutIncreasingCeiling` | One winning reservation/rework; unchanged ceiling and prior evidence; late overshoot cannot dispatch more work |
| Authority ceiling, replay, revocation | `TestGovernedActionAuthorityDenials`, `TestGovernedActionOneTimeGrantRacesDifferentOperations`, `TestGovernedActionStandingRewriteRevokeAndStaleToken`, `TestGovernedActionRechecksQueuedAuthority` | Denial drained through worker/reconciliation leaves no executable operation/effect; one-time authority cannot become another effect; revocation is rechecked |
| Spend proof identity and expiry | `TestSecuritySpendTermsCannotBeSubstituted`, `TestSecuritySpendConcurrentProofHasOneWinningCommand`, other `TestSecuritySpend` regressions above | No additional reservation, receipt or dispatch on refused assertions; exactly one accepted command/effect; original nonce replay survives expiry |
| Stateful action observation and serialization | `TestSecurityStatefulActionsSerializeAcrossAttemptsAndFreshRetry`, `TestGovernedActionSerialResourceAndEffectBoundary` | Two real command/worker paths share one resource; busy/stale actions have zero effects; a distinct fresh action succeeds without reviving rejected work |
| Migration permission, holds and fencing | `TestSecurityMigrationPermissionReplayAndPreviewHold`, `TestSecurityMigrationRevocationSerializesWithCommand`, `TestSecurityMigrationStalePreviewAndClaimTakeover`, `TestSecurityMigrationDowngradePreservesAdministration` | Revoked replay/read refused; expired/changed preview cannot execute; held claim remains scheduled; fresh pool/worker takeover fences old renew/complete/release and preserves preview |
| Byte integrity and crash recovery | `TestResultArtifactMismatchNeverCreatesRevision`, `TestArtifactCommitRechecksBytesAndExactManifest`, `TestSecurityArtifactRestartAfterBytesBeforeCommit`, `TestSecurityArtifactRestartAfterAdmissionKeepsExactRevision`, `TestArtifactCrashAfterStoreCommitFencesOldWorker` | Restart before bytes, after bytes, after external commitment and after metadata; zero premature admission, exact winning commit, one verification/revision and no duplicate commit effect |

[#60](https://github.com/dac-plane/dac-plane/issues/60) separately tracks actual
source/target migration execution and its recovery evidence. #11, #28 and #30
link that follow-up. No administration command may fabricate provider cutover or
claim a completed migration solely because a claimant owns a lease.

## Observed verification

The first-slice `TestSecurity` race-enabled run passed (20.548s), including
the explicit shadow/off primary-cursor checks. Existing
spend regressions also passed under race (18.790s). `just test-contracts` passed.
A temporary inversion of the cumulative guard caused the late-usage regression
to fail; the original guard was restored. The expanded security regressions also
passed under race (56.429s and 49.853s as cases were added). Final campaign and
full-CI results are recorded below when observed, not inferred from these earlier
runs. No external-model review was performed, by user choice.

The first combined campaigns exposed an old downgrade-test assertion that
hard-coded schema version 6. The database correctly preserved version 7 when
artifact downgrade was refused. The assertion now uses `CurrentVersion`; its
focused race-enabled regression passed (4.954s). Released migrations were not
changed. This failure is not attributed to production migration behavior.

Run `just test-security` and `just test-security-race` for this campaign.
The `security` workflow runs the race-enabled recipe on pull requests, main
pushes and manual dispatch. The `ci` workflow uses
`just test-integration-other-race` for the complementary top-level tests.
Both selectors share one expression across all packages, so integration tests
run once across the two workflows. `just ci` runs both partitions locally;
`just test-integration-race` remains the complete standalone suite.
Go vulnerability scanning remains a separate check in `ci`.

Final focused runs passed after the downgrade assertion correction:

| Recipe | Application | Worker | PostgreSQL | Migrations | Simulator |
|---|---:|---:|---:|---:|---:|
| `just test-security` | 245.576s | 19.478s | 32.057s | 28.826s | 0.457s |
| `just test-security-race` | 268.913s | 19.977s | 33.661s | 30.451s | 1.585s |

These uncached runs include all 19 new security test functions and the reused
acceptance cases selected by the recipes.

Final `just ci` passed on the implementation: full uncached race-enabled
integration tests passed application (580.825s), worker (59.995s), PostgreSQL
(117.969s), migrations (28.374s) and all other packages. Formatting, module
verification, vet, unit/race tests, schema guards and workflow lint passed.
The vulnerability scan found no vulnerabilities. This records local checks,
not unobserved GitHub checks or human verification.

Pipeline follow-up verification: `just ci` passed with the new split, including
uncached race-enabled application tests (315.247s complementary, 284.120s
security), PostgreSQL and migrations. Workflow lint and formatting passed;
govulncheck found no vulnerabilities. An inventory of all 412 top-level targets
confirmed 147 security and 265 complementary targets with no overlap or omission.
These are local results; GitHub execution is verified separately after pushing.
