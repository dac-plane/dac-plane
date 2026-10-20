# Recovery evidence — issue #27

This implementation note supports [#27](https://github.com/dac-plane/dac-plane/issues/27)
under the active baseline and ADR-0003. It does not select a production provider,
transport or workflow engine. Exploration remains historical evidence.

## Durable work and unknown effects

Submission already moves a provider operation to `dispatched` when enqueueing it.
That lifecycle state alone cannot distinguish queued work from a send in progress.
The worker therefore writes the static `send_started` marker in the existing
outbox `last_error` metadata immediately before sending, in the claim transaction.
No provider text, credentials or action payload is added to that field.

Missing runtime wiring leaves work unclaimed. Request validation, qualification,
budget and lease checks precede the marker. A fresh-time claim/Attempt fence and
deadline check follow preparation lock waits. Calls remain outside transactions.

A transport error, invalid reply, mismatched delivery or pending durability after
entering `Send` leaves the outcome uncertain. A replacement worker never repeats
that ordinary runtime request. Already accepted operations are not resent. Legacy
processing claims or retry errors are conservatively treated as potentially sent;
upgrading does not turn missing historical markers into proof of no effect.

Only bound provider observations resolve uncertainty. Missing history, EOF and
cursor reset are not negative-effect evidence. If the provider cannot establish
the outcome, the operation remains explicitly uncertain rather than fabricating
success, failure or automatic replacement work.

Two distinct replay protocols remain intentionally separate:

- Decision steers carry the existing stable `decision_id` while naming the parent
  runtime operation. The qualified adapter must deduplicate that decision, not
  all steers by the parent operation. The simulator now distinguishes start work,
  different decisions and changed-payload replay on the same runtime instance.
  Reusing a decision ID with another parent operation is refused too.
- Artifact stages have an explicit stable-operation replay guarantee, including
  retrieval of the exact recorded commitment. Matching bytes do not substitute
  for that receipt. Retry does not admit evidence; fenced work cannot finalize.

## Validated observations and atomic recovery

Runtime and governed-action reconciliation share binding, qualification, cursor
locking and bounded rehydration. They do not share outcome authority: a runtime
operation outcome cannot settle a governed action, which still requires its exact
validated enforcement receipt and never resumes the parent runtime implicitly.

Reconciliation applies only entries returned by contract ingestion. Malformed
hashes/order, duplicates, tombstones, omitted payloads and invalid typed outcomes
cannot be mined from a raw frame to invent an effect. Attempt/Task locks serialize
the cursor recheck with ordinary application ingestion. Governed reconciliation
takes its action advisory lock before that scope, matching action-command order.

Outcome, lifecycle movement, receipt, history and cursor advancement commit
together. A test trigger fails the final cursor update after earlier writes;
the complete authoritative snapshot must remain unchanged before recovery retries
observation ingestion. This tests database atomicity, not an assumption that Go
context cancellation itself rolls back a transaction.

Ordinary reconnect resumes the durable cursor. A cleared generation persists its
new generation and explicitly reopens replay. Cursor-too-old opens a local replay
cursor from zero while preserving the durable high-water mark. Replay settlement
rechecks that watermark and operation version under locks. A concurrent change
ends the pass rather than overwriting newer knowledge. An old subscription is
closed when its generation becomes stale; its remaining frames are never relabelled
as observations in the newer generation. Rehydration reopens at most once per pass.

Shadow/off qualification is checked under scope and harness locks. Shadow
observations write digest-only comparison evidence, never primary lifecycle or
cursor progress. Entries and tombstones are deduplicated by their bound projection
independently of batching, replay flags and delivery timestamps. Off observations
apply nothing. A demoted uncertain operation remains unresolved until authoritative
reconciliation is permitted; shadow evidence is not silently promoted.

Shadow cursor loss opens one bounded replay using a subscription-local cursor.
Cleared generations and replay progress never update the primary cursor; only
deduplicated comparison digests survive restart. Off mode does not reopen replay
or record comparison evidence. Missing history and repeated resets remain
non-authoritative and cannot settle an uncertain operation.

Decision delivery uses the same qualification boundary. A queued decision in
shadow/off mode retains its pending delivery intent without steering the runtime
or changing the attention state. If qualification changes during an in-flight
steer, settlement leaves its result unresolved. Every post-send decision path,
including retry, malformed-reply exhaustion and lease release, locks Outbox →
Attempt → Task → harness and rechecks both fences using current server time.
Losing either fence, even at a final settlement check, rolls back as a no-fault
decision-delivery result so remaining rows in the tenant's batch still run.

## Acceptance index

Tests live beside the owning application, worker, simulator and repository suites.
Product restarts reopen the database pool and create new workers; external ledgers
survive. Direct SQL is limited to qualification and precise crash/fault boundaries.

| Guarantee | Executable evidence |
|---|---|
| Original command identity; changed payload conflicts | Existing submit/decision/governed replay tests; `TestRecoveryCancelAndReviewCommandPayloadConflicts` |
| Waiting state and independently delivered decisions survive restart | `TestRecoveryRestartWaitingAndTwoDecisions`; simulator `TestRecoveryRuntimeDistinctDecisionsAndChangedReplay` |
| No blind repeat after loss or crash | `TestRecoveryRuntimeTransportLossNeverResends`, `TestRecoveryCrashBoundariesPreserveUncertainty`, `TestRecoveryPendingAndMalformedRepliesRemainUncertain` |
| Accepted evidence only; monotonic cursors | `TestRecoveryReconcileRejectsMalformedAndDuplicateOutcomes`, `TestRecoveryConcurrentCursorPreventsStaleOutcome`, `TestRecoveryStaleSubscriptionCannotRelabelGeneration` |
| Reconnect, explicit replay and bounded reset loops | `TestRecoveryReconnectResumesDurableCursor`, `TestRecoveryReconcileReopensLostCursor`, `TestRecoveryRehydrationResetLoopIsBounded` |
| Protocol negotiation before provider work | `TestRecoveryReconnectNegotiatesBeforeProviderWork` uses the existing Gate in a test-only adapter, including fresh negotiation after restart |
| Stale send/decision writes emit no partial state or history | `TestRecoveryStaleSendSettlementWritesNothing`, `TestRecoveryStaleDecisionAcknowledgementWritesNothing`; existing persistence lease/maintenance fencing tests |
| Exact governed receipt; no authority revival | `TestRecoveryQueuedPolicyGenerationBlocksAfterRestart`; existing governed lost-reply, receipt-rehydration, crash-marker and queued-authority regression tests |
| Artifact replay preserves exact admission | `TestRecoveryArtifactRetryAndFencedAdmission`; existing artifact lost-commit-reply, stale/future-writer and post-commit crash tests |
| Shadow restart preserves primary authority | `TestRecoveryShadowObservationsCannotSettlePrimary`, `TestRecoveryShadowGovernedReceiptPreservesPrimary`, `TestRecoveryShadowEvidenceIsDigestOnlyAndBatchIndependent`, `TestRecoveryOffObservationsDoNotMutate`, `TestRecoveryShadowResetReopensLocalReplay` |
| Outcome/cursor transaction is all-or-nothing | `TestRecoveryOutcomeAndCursorRollbackTogether` |
| Decision dispatch and settlement respect qualification and expiry | `TestRecoveryDecisionQualificationBeforeSend`, `TestRecoveryDecisionDemotionBeforeSettlement`, `TestRecoveryDecisionErrorsRespectFences` |
| Lost decision fences do not abort the tenant batch | `TestRecoveryLostDecisionFenceKeepsBatchMoving`, including expiry injected after the initial settlement fence check |

Run `just test-recovery`, `just test-recovery-race`, `just test-contracts` and
`just ci`. Focused integration recipes bypass the cache and use the same macOS
SDK setup as full CI. Full CI already includes these packages, so it does not run
the focused campaign a second time. Observed results are recorded in `tasks/plan.md`.

The completed local `just ci` run passed, including uncached race-enabled
application, worker, PostgreSQL and migration tests. Contract guards, module
checks, vet, vulnerability scanning and workflow lint passed. A temporary inverted
outcome-entry gate was caught by its recovery regression and restored before
commit. The campaign adds 27 recovery test functions alongside existing evidence.

## Limits and remaining gates

These are deterministic simulator/PostgreSQL results, not production-provider
qualification or a universal exactly-once guarantee. The non-deduplicating runtime
fixture specifically prevents a simulator's reply cache from hiding unsafe resend.
The provider ledger survives product restart; this does not prove provider-side
durability after the provider itself loses its storage.

No database migration, public wire field, service split or dependency was added.
#28 owns the broader security/lifecycle adversarial campaign; #29 evaluates whether
this SQL/outbox evidence is sufficient. Post-action/failure-hook delivery and safe
typed hook evidence remain an explicit #30 freeze gate.
