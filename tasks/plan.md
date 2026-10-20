# Issue #27 — recovery, replay, fencing and restart safety

Tracker: https://github.com/dac-plane/dac-plane/issues/27
Branch: `feature/27-recovery-safety`, from merged #26 (`3a52451`).

## Direction

The active architecture and ADR-0003 govern this acceptance campaign. Keep
PostgreSQL/outbox and one product lifecycle owner. Exploration stays read-only.
Uncertainty is not permission to resend. Artifact stages and decision delivery
retain their explicit, identity-bound replay protocols.

## Ordered work

- [x] Reproduce runtime send, frame-validation, cursor recovery and shadow gaps.
- [x] Fence ordinary runtime send uncertainty and stale settlement.
- [x] Reconcile accepted observations atomically; reopen bounded cursor recovery.
- [x] Gate shadow/off observations and persist deduplicated safe comparisons.
- [x] Make simulator decisions independently replayable on one runtime operation.
- [x] Prove restart, handshake, takeover and transaction-failure boundaries.
- [x] Add focused recovery recipes and an acceptance/evidence implementation note.
- [x] Run focused/race/contract checks, a restored mutation, and final `just ci`.

## Acceptance

Map command replay, restart while waiting, lost runtime reply, stream recovery,
stale claimant, governed recovery, artifact recovery, shadow restart and atomic
transaction failure to executable tests. Reuse already-sufficient tests. New
tests exercise application commands/ingestion and workers, with direct store
setup limited to qualification and precise crash/fault boundaries.

Use fresh workers and database pools against the same database. Keep provider
effect ledgers alive across product restart. Synchronize on bounded channels
and explicit claim expiry, not arbitrary sleeps. Assert record versions,
audit/events, outbox, receipts and external effects, not only final state.

## Boundaries

No dependency, public wire field, lifecycle state, migration, transport, cloud
provider or workflow engine is planned. #28 owns broader security coverage;
#29 evaluates SQL/outbox evidence; post-action/failure hooks remain a #30 gate.

## Verification

Observed implementation results:

- The initial recovery regressions failed on lost-send retry, raw malformed/
  duplicate outcome acceptance, missing cursor reopen and shadow mutation.
- A separate Attempt-takeover regression failed before the settlement fence fix.
- Expanded application recovery tests passed under the race detector (84.853s).
- `just test-recovery` passed: application 161.489s, worker 23.720s,
  PostgreSQL 19.257s and simulator 0.492s.
- A temporary inversion of the operation-outcome entry gate made
  `TestRecoveryRuntimeTransportLossNeverResends` fail, leaving the operation
  uncertain instead of proving recovery. The exact gate was restored.
- `just test-recovery-race` passed: application 180.083s, worker 26.388s,
  PostgreSQL 23.149s and simulator 1.492s. The selector also includes the
  `Reconciliation` phase guards, separately observed passing under race (5.977s).
- The queued stale-policy-generation restart test passed under race (4.871s).
- Final review added a decision-parent rebinding assertion: it failed before
  the cache-key correction and passed afterward, including simulator race tests.
- `just test-contracts`, format checks and integration vet passed.
- Final `just ci` passed on the completed implementation: uncached race-enabled
  integration tests passed application (403.945s), worker (64.853s), PostgreSQL
  (108.863s), migrations (31.930s) and every other package. Unit/race tests,
  schema guards, module verification, vet and workflow lint passed. The
  vulnerability scan found no vulnerabilities.

PR architecture-review follow-up:

- Added decision-delivery regressions for queued shadow/off qualification,
  in-flight demotion and transport/unknown/malformed replies after outbox or
  Attempt expiry/takeover. The regressions exposed both reviewed gaps before
  the corrections.
- Post-send decision handlers now share scope, qualification and fresh-fence
  checks. Non-live queued decisions retain their intent for later live delivery.
- `just test-integration-race ./internal/application/ -run TestRecoveryDecision`
  passed (53.108s). The worker delivery regressions passed under race (10.397s)
  after their fixture explicitly registered its live harness; the worker
  decision retry regressions also passed under race (7.667s).
- Inverting the new settlement qualification gate in a temporary Go overlay
  caused the demotion regression to fail. The tracked source was unchanged
  throughout that mutation check.
- Final follow-up `just ci` passed, including uncached race-enabled application
  (450.449s), worker (58.867s), PostgreSQL (99.715s) and migration (27.004s)
  integration suites. Vulnerability scanning found no vulnerabilities and
  workflow lint passed.

Additional PR review regressions:

- Shadow cursor resets originally left replay closed. Local-only shadow
  ingestion now opens bounded replay without creating/advancing primary cursors
  or settling operations. Tests cover cleared/expired cursors, unavailable
  replay, reset loops, restart deduplication and off mode.
- Lost decision fences originally aborted the rest of the tenant batch.
  Decision delivery now treats stale-version races as no-ops after the inner
  transaction rolls back. Batch tests cover accepted, transport-error and
  malformed replies across expiry/takeover, including expiry injected after
  the initial settlement check.
- Both regressions failed before their respective fixes.
- Race-enabled shadow reset regressions passed (23.669s), and the expanded
  decision recovery suite passed (158.568s), including the late-expiry rollback
  and same-batch continuation test. The follow-up full-CI result is recorded
  in PR #59.

The campaign adds 27 recovery test functions, retaining the existing replay,
authority, artifact and fencing tests as complementary evidence. Local full CI
is not a claim about unrun GitHub checks or production-provider qualification.

Implementation detail refined after tracing submission: `dispatched` already
means enqueued. The existing outbox metadata carries a static `send_started`
marker committed just before sending. Historical processing claims remain
conservatively uncertain; missing older markers never prove no effect.

The evidence index and boundaries are in
[`docs/architecture/recovery-27.md`](../docs/architecture/recovery-27.md).
