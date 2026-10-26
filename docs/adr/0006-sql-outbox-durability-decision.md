# ADR-0006: Accept PostgreSQL and transactional outbox for the bounded M1 lifecycle

- Status: Accepted
- Date: 2026-10-21
- Issue: [#29](https://github.com/dac-plane/dac-plane/issues/29)

## Context

The active architecture uses PostgreSQL as the authority for the product lifecycle and a
transactional outbox for durable asynchronous dispatch. That choice remained provisional pending
failure evidence. Issues #26–#28 and #60 now exercise result admission, recovery, security, claims,
and migration against PostgreSQL and deterministic provider simulators.

The historical [exploration ADR-0007](../../architecture-explore/docs/adr/0007-product-durability-sql-outbox.md)
and build specification motivated the comparison with workflow engines. They are evidence, not
active requirements. [ADR-0002](0002-initial-application-boundary.md) and [ADR-0003](0003-provider-contract-semantics.md)
remain authoritative for lifecycle ownership and provider-neutral semantics. The exploration's
workflow-substrate screen rejects Temporal, Hatchet and Inngest as primary control planes; it does
not test whether any engine could support a product-owned lifecycle.

## Decision

Accept PostgreSQL plus the transactional outbox for the simulator-backed M1 lifecycle and the
failure modes exercised below. PostgreSQL remains the sole authority for product records. A
transaction persists a lifecycle change and its required audit, event, receipt/cursor or outbox
records atomically. The outbox provides durable, at-least-once delivery; it does not provide
exactly-once external effects.

Workers use stable operation identities and fenced claims. A stale claimant cannot settle work
after takeover. External calls remain outside database transactions. Before a potentially effectful
send, the product records that the send may have begun. A lost reply, transport failure or missing
authoritative observation remains uncertain and is reconciled; it is not treated as proof that no
effect occurred and does not permit an unsafe ordinary resend. Where a provider protocol explicitly
allows stable-operation replay, the same identity and payload are used. Otherwise uncertainty can
remain pending. ADR-0012 makes that permission explicit through the negotiated
`operation.replay` capability; the replay keeps the operation identity and payload and uses a fresh
delivery identity.

No workflow engine or additional service is adopted. Reconsider the substrate only if new evidence
shows that PostgreSQL/outbox cannot preserve a required product invariant within this operating
boundary. A follow-up must identify the failed invariant and compare supporting mechanisms without
transferring lifecycle ownership or inferring adoption from exploration evidence.

## Evidence and limits

| Invariant | Executable evidence | Observed result and limit |
|---|---|---|
| Outbox delivery is durable, retryable and fenced | `TestPersistenceAtomicCommitAndRollback`, `TestDispatchOutboxClaimLifecycle`, `TestDispatchOutboxRetryAndExhaust`, `TestDispatchAttemptLeaseFencing`, `TestDeliveryRedeliversAfterCrashBetweenDecideAndSend`, `TestDeliveryDuplicateDoesNotDoubleSend`, `TestWorkerRestartRecovery`, `TestRecoveryStaleSendSettlementWritesNothing`, `TestRecoveryStaleDecisionAcknowledgementWritesNothing`; [persistence tests](../../internal/platform/postgres/persistence_integration_test.go), [dispatch tests](../../internal/platform/postgres/persistence_dispatch_integration_test.go), [delivery tests](../../internal/worker/attention_integration_test.go), [worker restart tests](../../internal/worker/worker_integration_test.go), [recovery claim tests](../../internal/application/recovery_claims_integration_test.go) | State, audit, durable event and outbox commit together; an abandoned transaction leaves no task; a crash before send can be redelivered, and replay after acknowledgement does not double-send. Claim takeover and stale settlement preserve the winning state. Database failover and restore are not tested. |
| Runtime uncertainty and cursor recovery do not invent outcomes | `TestRecoveryRuntimeTransportLossNeverResends`, `TestRecoveryCrashBoundariesPreserveUncertainty`, `TestRecoveryReconcileReopensLostCursor`, `TestRecoveryOutcomeAndCursorRollbackTogether`; [recovery tests](../../internal/application/recovery_integration_test.go), [fault tests](../../internal/application/recovery_faults_integration_test.go) | Lost sends remain uncertain; replay/reset recovery uses accepted bound evidence; injected transaction failure leaves outcome and cursor unchanged. External provider durability is not proven. |
| Authority and spend receipts survive replay, expiry and concurrency | `TestGovernedActionLostReplyNeverRepeatsEffect`, `TestGovernedActionCrashMarkerNeverResends`, `TestGovernedActionRechecksQueuedAuthority`, `TestGovernedActionOneTimeGrantRacesDifferentOperations`, `TestSecuritySpendConcurrentProofHasOneWinningCommand`, `TestSecuritySpendExpiryRecheckedAfterChallengeLock`; [governed action tests](../../internal/application/governed_action_integration_test.go), [spend tests](../../internal/application/security_spend_integration_test.go) | A single authorized winner is recorded; stale or consumed authority does not create another effect. This does not qualify a production enforcement or credential provider. |
| Shadow and maintenance state remain non-authoritative and fenced | `TestRecoveryShadowResetReopensLocalReplay`, `TestRecoveryShadowEvidenceIsDigestOnlyAndBatchIndependent`, `TestSecurityMigrationStalePreviewAndClaimTakeover`; [security evidence](../architecture/security-28.md) | Shadow replay does not advance the primary cursor or settle primary state; stale maintenance holders cannot settle after takeover. Simulator behavior is not a provider qualification. |
| Artifact and migration effects retain durable recovery ownership | `TestArtifactCommitRechecksBytesAndExactManifest`, `TestArtifactCrashAfterStoreCommitFencesOldWorker`, `TestMigrationRechecksBudgetBeforeStartingTarget`, `TestMigrationStaleWorkerCannotSettleAfterTakeover`; [artifact tests](../../internal/application/result_recovery_integration_test.go), [migration evidence](../architecture/harness-migration-60.md) | Exact manifest/fence evidence is required; migration cleanup remains owned through lost stop replies and restart. This does not prove semantic result correctness or exact native-session continuity. |

The focused race campaigns passed with PostgreSQL integration tests uncached:
`just test-recovery-race`, `just test-security-race` and `just test-migration-race`.
`just test-contracts` passed. `GOTOOLCHAIN=go1.26.10 just ci` passed, including both integration-race
partitions, vulnerability scanning and workflow lint. After pinning Go 1.26.10 in `go.mod`,
`just mod-check` and `just vuln` passed; govulncheck reported no vulnerabilities.

This decision is limited to the tested simulator-backed M1 lifecycle and database operations. It
does not establish production-provider conformance, universal exactly-once behavior, database
failover/backup-restore readiness, retention policy, throughput or availability objectives, or
semantic correctness of provider results. Provider effects are outside the PostgreSQL transaction.
Post-action/failure-hook delivery and safe typed hook evidence remain open for the #30 contract
freeze and are not covered by this decision.

## Alternatives considered

- **Keep the choice provisional:** rejected because the M1 failure-injection evidence now exercises
  the required SQL/outbox boundaries and supports a bounded decision.
- **Adopt a workflow engine now:** rejected because no tested substrate invariant failed, the
  product lifecycle must remain authoritative in PostgreSQL, and the exploration only screened
  engines as primary control planes.
- **Claim general or exactly-once durability:** rejected because external provider effects cannot
  commit atomically with PostgreSQL and are governed by provider-specific idempotency and receipts.

## Consequences

- PostgreSQL and the transactional outbox remain the M1 durability substrate; no runtime,
  environment, enforcement provider or workflow engine is selected by this ADR.
- At-least-once delivery requires duplicate-safe consumers, stable operation identities, claim
  fencing and reconciliation of uncertain outcomes.
- Production operations, provider qualification, database recovery objectives, and the outstanding
  hook-evidence contract require separate evidence before they can be claimed.
