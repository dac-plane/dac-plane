# M1 lifecycle contract freeze (#30)

Status: implementation contract frozen for the simulator-backed M1 lifecycle. This records tested
Go/JSON Schema/PostgreSQL behavior; it does not qualify production providers or operational
deployment.

## Decision boundary

The product remains the sole lifecycle owner in the Go modular monolith. PostgreSQL records
authoritative state and the transactional outbox; runtime, environment, enforcement and artifact
stores are replaceable ports. Delivery is at least once, with stable identities, claim/version
fences and explicit uncertainty. No universal exactly-once external-effect claim is made. This is
consistent with [ADR-0001](../adr/0001-control-plane-language.md) through
[ADR-0006](../adr/0006-sql-outbox-durability-decision.md). The `architecture-explore/` prototypes
remain historical evidence, not implementation requirements.

## Lifecycle record traceability

| Authoritative record | Go contract/domain | Schema and durable mapping | Representative evidence |
|---|---|---|---|
| Task | `internal/lifecycle/task.go` | `task` | `TestTaskHappyPathAndReviewOutcomes`, `TestPersistenceAtomicCommitAndRollback`, `TestReviewAcceptRejectAndImmutableDisposition` |
| Attempt | `internal/lifecycle/attempt.go` | `attempt` | `TestAttemptFollowsOperation`, `TestAttemptTerminatesOnlyAfterBothStops`, `TestWorkerRestartRecovery` |
| AttentionRequest | `internal/lifecycle/attention.go` | `attention_request` | `TestDecideThenDeliverAndConsume`, `TestDeliveryRedeliversAfterCrashBetweenDecideAndSend` |
| Decision | `internal/lifecycle/attention.go` | `decision` | `TestFirstDecisionWins`, `TestPersistenceDecideAttention`, `TestDeliveryDuplicateDoesNotDoubleSend` |
| Budget | `internal/lifecycle/budget.go` | `budget`, `reservation`, `usage` | `TestCompetingReservationsCannotExceedCeiling`, `TestSecurityLateUsageRefusesReworkWithoutIncreasingCeiling` |
| Artifact | `internal/lifecycle/result.go` | `artifact`, `artifact_manifest`, `artifact_verification` | `TestAdmitArtifactCommitsOnlyVerifiedBytes`, `TestArtifactLostCommitReplyRecoversExactManifest` |
| AuditEvent | `internal/lifecycle/records.go` | `audit_event` and durable lifecycle history | `TestAuditOfAppliedAndRejectedChanges`, `TestPersistenceRefusalAuditsWithoutOutbox` |
| ResultRevision | `internal/lifecycle/result.go` | `result_revision`, `revision_artifact` | `TestResultArtifactAdmissionBothObservationOrders`, `TestResultRevisionRequiresCommittedArtifactsOfItsTask` |
| ResultReview | `internal/lifecycle/result.go` | `result_review` | `TestReviewAcceptRejectAndImmutableDisposition`, `TestReviewRequestChangesSchedulesNewAttemptAndPreservesLineage` |

The mapping is tenant-scoped. Versioned lifecycle transitions and append-only records are checked
in domain tests and repository SQL. Contract/schema agreement and required schema coverage are
guarded by `just test-contracts` (`TestContractConformanceGoFixturesMatchSchemas`,
`TestSchemasAcceptValidGoRecords`, and the contract coverage guards). The migration sequence is
additive through `00009_hook_stage_evidence.sql`; historical pre-action evidence remains readable,
and downgrade refuses if enriched or follow-up hook evidence would be lost.

## Provider-neutral semantic evidence

| Contract family | Evidence | Boundary / remaining limitation |
|---|---|---|
| Negotiation and framing | `TestNegotiateSelectsHighestCommonVersionWithoutDowngrade`, `TestContractConformanceRoleMethodConditionsMatchCatalogue`, `TestRecoveryReconnectNegotiatesBeforeProviderWork` | In-process adapters only; no production transport qualification. |
| Event streams, cursors, replay and tombstones | `TestRecoveryReconcileRejectsMalformedAndDuplicateOutcomes`, `TestRecoveryReconcileReopensLostCursor`, `TestRecoveryRehydrationResetLoopIsBounded`, `TestRecoveryReconnectResumesDurableCursor` | Replay safety is proven against deterministic simulators, not arbitrary providers. |
| Runtime/environment bindings and observations | `TestEventBindingOfAnotherAttemptRefused`, `TestEventEnvironmentStopConvergesCancellation`, `TestMigrationExecutionSnapshotsTenantSourceAttempts` | Native session semantics and real environment eventual consistency remain provider-specific. |
| Harness off/shadow/live and migration | `TestWorkerShadowWithholdsSend`, `TestRecoveryShadowResetReopensLocalReplay`, `TestMigrationExecutesQuiescePrepareCutoverAndCleanup`, `TestMigrationStaleWorkerCannotSettleAfterTakeover` | Qualification is test/simulator evidence only; no real harness has passed qualification. |
| Scoped authority and governed actions | `TestGovernedActionOnceReceiptAndReplay`, `TestGovernedActionRechecksQueuedAuthority`, `TestGovernedActionLostReplyNeverRepeatsEffect`, `TestGovernedActionCrashMarkerNeverResends` | Unknown action outcomes remain uncertain until exact receipt reconciliation; no generic exactly-once guarantee. |
| Pre/post/failure hooks | `TestGovernedActionPostHookIsDurableAndSafe`, `TestGovernedActionFailureHookUsesTypedFailureStage`, `TestGovernedActionLostPostHookReplyRecoversWithoutRepeatingAction`, `TestGovernedActionMalformedPostHookReplyDoesNotAdmitEvidence`, `TestGovernedActionStaleHookClaimCannotCommitEvidence`, `TestGovernedActionReceiptRehydrates`, `TestPersistenceObservationsAndEvidence` | Stage identity is `(tenant, action operation, hook kind)`. Hook text is transient; persisted safe evidence contains digests and bounded typed usage. A recovered receipt creates the post-hook intent in the same terminal transition. Exhausted hook delivery remains visible as an exhausted outbox row. |
| Spend challenge and budget accounting | `TestSubmitSpendGovernedAwaitsReceiptThenDispatches`, `TestSecuritySpendExpiryRecheckedAfterChallengeLock`, `TestSecuritySpendCancellationWinsBeforeAssertionScopeLock` | The simulator proves protocol behavior, not financial-provider reconciliation. |
| Artifact commit and immutable results | `TestResultArtifactAdmissionBothObservationOrders`, `TestArtifactLostCommitReplyRecoversExactManifest`, `TestSecurityArtifactRestartAfterAdmissionKeepsExactRevision` | Integrity/provenance evidence is not semantic correctness or platform-run testing. |
| Stateful action preconditions | `TestSecurityStatefulActionsSerializeAcrossAttemptsAndFreshRetry`, simulator resource tests | Only the M1 simulated resource mutation is exercised. |

The pre-action hook evidence row preserves the legacy natural key and stores typed duration, usage,
and digests for response/message/context projections. Follow-up stages use an additive table and
the same parent action operation with a distinct stage key. A terminal action transition and its
follow-up outbox intent commit together. Hook delivery never repeats or reverses the governed
action. A confirmed rejection schedules `action_failure`; transport loss or unknown durability
does not.

## Verification record

Observed local verification on `feature/30-contract-freeze`:

- `just test-contracts` passed.
- `just test-hook-lifecycle` and `just test-hook-lifecycle-race` passed.
- `just test-recovery-race`, `just test-security-race`, and `just test-migration-race` passed.
- `just ci` passed: formatting, module verification/tidy, vet, unit and race tests, both uncached
  PostgreSQL integration-race partitions, vulnerability scan (`No vulnerabilities found`), and
  workflow lint. The general integration partition passed with application 330.320s, PostgreSQL
  68.812s, and worker 32.172s; the selected race partition passed with application 316.668s,
  migrations 32.781s, PostgreSQL 32.859s, and worker 18.213s. All remaining packages in both
  partitions passed or had no selected tests.

Integration recipes use uncached PostgreSQL containers. Local results and remote PR checks are
reported separately; a local pass is not a remote CI result. Real-provider behavior and remote PR
checks are not verified by these local results.

## Explicitly unresolved

Production runtime, environment, enforcement and artifact-store qualification; credential and trust
ceremonies; deployment topology; PostgreSQL failover/backup/restore; untested scale; and semantic
result correctness remain open. #29 supports only the bounded simulator-backed SQL/outbox model.
Post-action/failure hook delivery is now locally evidenced but still requires real-provider
qualification. The wider security/lifecycle campaign and the #60 migration path retain their own
evidence notes. No HTTP surface, workflow engine, service split, review UI or generic verification
runner is adopted by this freeze.
