# Contract gate #18: pre-persistence approval

- Status: **approved**
- Date: 2026-10-16
- Tracker: [#18](https://github.com/AssahBismarkabah/dac-plane/issues/18) (epic #11)
- Prerequisites: #13, #14, #15, #16, #17 and #32 closed and merged at `main@8851ec4`.
- Effect: persistence work (#19) is unblocked. Final M1 freeze remains with #30.

This gate reviews the pure Go plus JSON Schema 2020-12 contracts before PostgreSQL
structure makes them expensive to change. It makes no durability, simulator,
transport, or provider-selection decision.

## Contract-freeze items (review § Contract-freeze gate)

All six items have active Go contracts, schemas, and agreement tests:

1. Identifiers, tenant ownership, versions, state transitions for all nine product
   records: `internal/identity` (`Principal`, `Policy`, `AuthorityGrant`,
   `AuthorityUse`), `internal/lifecycle` (`Task`, `Attempt`, `AttentionRequest`,
   `Decision`, `AuditEvent`, `Binding`, `ProviderOperation`, `TurnEnd`,
   `EnvironmentObservation`, `HarnessQualification`, `MigrationPass`,
   `MaintenanceClaim`, `Budget`, `Reservation`, `SpendChallenge`, `SpendReceipt`,
   `ArtifactManifest`, `Artifact`, `ResultRevision`, `ResultReview`) against
   `contracts/jsonschema/identity/*.schema.json` and
   `contracts/jsonschema/lifecycle/*.schema.json`.
2. Budget reservation, consumption, release, ceiling: `Budget`, `Reservation`
   plus spend separation (`SpendChallenge`, `SpendReceipt`, `SpendAssertion`).
3. Immutable revisions and reviews bound to exact revision and reviewer
   authority: `ResultRevision`, `ResultReview`, `Artifact`, `ArtifactManifest`.
4. Adapter commands, normalized events, cancellation acknowledgement,
   replay/deduplication: `internal/contracts` (`Frame`, `Request`, `Cancel`,
   `Reply`, `Cursor`, `StreamFrame`, `Event`, `BoundEvent`, `Ingestion`).
5. Artifact upload, digest verification, provenance, registration ordering:
   `ArtifactManifest`, `ArtifactCommitOutcome`, `RetryReasons`.
6. Tenant/principal identity mapping and scoped provider credentials:
   `Principal`, `AuthorizationContext`, `ActionRequest`, `AuthorityGrant`.

## Provider-neutral families (ADR-0003)

| Family | Go | Schema | Executable fixtures |
|---|---|---|---|
| Versioned control protocol, 13 failures, person/background demand | `Frame`, `Lifecycle`, `Request`, `Cancel`, `Reply`, `Failure`, `Negotiate`, `Gate`, `Role`, `Methods` | `adapter/frame.schema.json`, `adapter/common.schema.json` | `TestNegotiateSelectsHighestCommonVersionWithoutDowngrade`, `TestGateRefusesWorkBeforeTheHandshake`, `TestGateFailsFastOnMalformedFramesAndMismatch`, `TestGateOpensOnHelloThenAdmitsWork`, `TestFrameValidation`, `TestReplyDurabilityMatchesOutcome`, `TestMethodCatalogueIsPerRole`, `TestContractConformance*` |
| Replayable stream, generations, tombstones, rehydration, unknown durability | `Cursor`, `StreamFrame`, `Entry`, `Tombstone`, `Event`, `BindEvent`, `BoundEvent`, `Ingest` | `adapter/cursor.schema.json`, `adapter/stream-frame.schema.json`, `adapter/event.schema.json` | `TestIngestCountsDuplicatesAndAdvancesTheCursor`, `TestIngestGenerationsAndRehydration`, `TestStreamFramesRejectBadPayloadsAndOrdering`, `TestEventsAreAuthenticatedAndBoundBeforeUse`, `TestAttemptUnknownOutcomeEntersReconciliation`, `TestUncertainOperationIsReconciledNeverResent` |
| Execution bindings | `Binding`, `ProviderOperation`, `TurnEnd` | `lifecycle/binding.schema.json`, `lifecycle/provider-operation.schema.json`, `lifecycle/turn-end.schema.json` | `TestCheckBinding`, `TestBindingValidate`, `TestSendStatusDrivesOperation`, `TestTurnEndValidate` |
| Environment and harness lifecycle | `EnvironmentObservation`, `MaintenanceClaim`, `HarnessQualification`, `MigrationPass` | `lifecycle/environment-observation.schema.json`, `lifecycle/maintenance-claim.schema.json`, `lifecycle/harness-qualification.schema.json`, `lifecycle/migration-pass.schema.json` | `TestEnvironmentObservationOrdering`, `TestMaintenanceClaimFencesStaleHolder`, `TestMaintenanceClaimRelease`, `TestHarnessPromotionIsStepwiseAndPrincipalOnly`, `TestMigrationExecutesOnlyAfterFreshPreview`, `TestMigrationPassMustChangeHarness` |
| Scoped authority, retirement, revocation, generations, ceilings | `AuthorityGrant`, `AuthorityUse`, `Policy`, `Authorize`, `IssueGrant` | `identity/authority-grant.schema.json`, `identity/authority-use.schema.json`, `identity/policy.schema.json`, `identity/common.schema.json` | `TestAuthorizeAllowsStandingGrantWithinExactScope`, `TestAuthorizeRetiresAllowOnceGrant`, `TestAuthorizeDenials`, `TestNeverPermissionOverridesAnyGrant`, `TestPolicyEffectiveClampsSettingToCeiling`, `TestApprovalConfersAllowOnceGrantUnderItsPolicy`, `TestAllowOnceGrantRequiresARecordedApproval` |
| Hook interception, fail-closed pre-action, digests only | `HookRequest`, `HookResponse`, `HookEvidence`, `DecidePreAction`, `EffectiveRequest` | `adapter/hook-request.schema.json`, `adapter/hook-response.schema.json`, `adapter/hook-evidence.schema.json` | `TestDecidePreActionFailsClosed`, `TestHookRewriteIsAuthorizedAsRewritten`, `TestHookRequestAndResponseShapes` |
| Two-phase spend authorization, separate from Budget | `SpendChallenge`, `SpendReceipt`, `SpendAssertion` | `lifecycle/spend-challenge.schema.json`, `lifecycle/spend-receipt.schema.json` | `TestSpendAssertionIssuesReceiptOnce`, `TestSpendAssertionMustMatchEveryTerm`, `TestSpendChallengeExpires`, `TestSpendAuthorizationDoesNotReplaceBudget`, `TestReplayedAndReorderedUsageDoNotDoubleConsume` |
| Fenced artifact admission | `ArtifactManifest`, `Artifact`, `ArtifactCommitOutcome` | `lifecycle/artifact-manifest.schema.json`, `lifecycle/artifact.schema.json` | `TestAdmitArtifactCommitsOnlyVerifiedBytes`, `TestAdmitArtifactRetryRegistersNothing`, `TestAdmitArtifactFencesStaleWriters`, `TestResultRevisionRequiresCommittedArtifactsOfItsTask` |
| Preconditions plus per-resource serialization | `ActionPrecondition`, `RequireSerial`, `CheckPrecondition` | `adapter/action-precondition.schema.json` | `TestPreconditionsRejectStaleStateAndParallelActions` |

Transition tables are enforced by construction: `TestMachinesAreExhaustivelyChecked`,
`TestEveryNonTerminalStateHasAnExit`, `TestReconciliationNeverReturnsToDispatch`,
`TestCancellationOnlyEndsTerminated`, plus `TestNewMachineRejectsMalformedTables`.
Go-vs-schema agreement is enforced both directions by
`TestEveryContractSchemaIsValidAndCovered`,
`TestEveryValidatedRecordHasASchema`, `TestSchemasAcceptValidGoRecords`,
`TestSchemaAndGoRejectTheSameInvalidDocuments`, and the enum-drift tests
(`TestSchemaEnumsMatchGoConstants`, `TestContractConformanceEnumsCannotDrift`,
`TestContractConformanceRoleMethodConditionsMatchCatalogue`).

## Enum, identity, and failure agreement

- 13 failure codes (`protocol_mismatch` through `resource_busy`) match
  `adapter/common.schema.json#/$defs/failure` via
  `TestContractConformanceEnumsCannotDrift`.
- Frame kinds, phases, demands, outcomes, stream kinds, hook kinds, permissions,
  and role-per-method catalogue match `frame.schema.json`,
  `stream-frame.schema.json`, `hook-request/response/evidence.schema.json` via the
  same test plus `TestContractConformanceRoleMethodConditionsMatchCatalogue`.
- Identity enums (scope class, permission, principal kind, grant mode, use
  source/outcome/reason) match `identity/*.schema.json` via
  `TestSchemaEnumsMatchGoConstants`.
- Lifecycle enums (task/attempt/attention/decision/audit/binding/operation/turn/
  observation/harness/migration/claim/budget/reservation/spend/artifact/review)
  match `lifecycle/*.schema.json` via the lifecycle enum test.
- Identities are UUIDv7 tenant/principal/attempt/operation/delivery records with
  exact-bytes digests (`identity.DigestOf`, `ids.DeliveryID`); unknown fields are
  rejected strictly on both sides.

## Vendor and rejected-choice sweep

`TestContractsCarryNoVendorIdentifiers` walks `contracts/` and rejects
`grok`, `grok bot`, `plane-bot`, `planebot`, `anysphere`, `webauthn`,
`connectrpc`, `temporal`, `native helper`. This PR adds the exact issue phrases
`grok bot` and `native helper` (space forms, so `nativeRef` fixtures do not
false-positive). No schema requires a vendor transport, ceremony, helper binary,
or provider identifier. `nativeRef` is an opaque provider-neutral correlation
identifier, not a native-helper requirement. ADR-0003 rejections stand unchanged.

Note: `internal/lifecycle/schema_test.go` uses `"webauthn-blob"` once as a
negative fixture proving malformed proofs are rejected. It is test code, not a
schema requirement.

## Trade-offs

None. No baseline, diagram, or accepted-ADR change was needed. Any future
reduced or changed semantic gets a new ADR before persistence.

## Verification for this gate

```sh
just test-contracts
go test ./internal/identity/... ./internal/lifecycle/... ./internal/contracts/...
just ci
git diff --check
```

The issue text cites `go test ./internal/domain/...`; no such package exists.
The commands above are the correct equivalent and are what CI runs.
