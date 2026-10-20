# Issue #28 — security and lifecycle acceptance

Tracker: https://github.com/dac-plane/dac-plane/issues/28
Branch: `feature/28-security-lifecycle`, from merged #27 (`77a79f6`).

## Direction

Active architecture and ADR-0003 govern implementation; exploration supplies
supporting evidence only. Preserve one product-owned lifecycle, PostgreSQL/
outbox and replaceable providers. Extend existing acceptance evidence and fix
demonstrated defects without adding a service or workflow engine.

## Ordered implementation

- [x] Create #60 for actual simulator-backed harness migration execution and
  link it to #11, #28 and #30 without removing existing requirements.
- [x] Reproduce expired/backdated and cross-command spend assertion defects.
  Require fresh server time under locks, preserving exact command-nonce replay.
- [x] Add typed cumulative `usage.reported`, schema and agreement tests; derive
  budget/reservation identity from authenticated Attempt lineage. Prove terminal
  late usage, overshoot, replay, wrong-unit refusal and shadow/off isolation.
- [ ] Extend tenant/operation forgery, authority denial/revocation/replay,
  partial/lost/duplicate cancellation and serialized action acceptance tests.
- [ ] Prove same-budget reservation races at the store boundary and competing
  rework commands through the application. No ceiling increases or additional
  reservations/receipts/dispatch from invalid assertion replay.
- [ ] Add restricted authenticated migration administration, migration 00007
  and ADR-0004. Prove permission, preview/hold invalidation and claim fencing.
- [ ] Map artifact corruption and crash boundaries to existing evidence; add
  only missing full-path restart cases, including after metadata admission.
- [ ] Add `just test-security` / `just test-security-race` and an architecture
  evidence note. Run focused, race, contracts and final `just ci` sequentially.

## Migration administration boundary

Actual source quiescence, target preparation/cutover and cleanup belong to
[#60](https://github.com/dac-plane/dac-plane/issues/60). Administrative execute
records intent only. No method may mark a migration completed without that
engine's execution evidence, and #28 does not change active provider bindings.

Use a narrow persisted tenant/principal administrator permission provisioned
by trusted setup, not caller allowlists, self-grant commands or general RBAC.
Current permission is required even for replay reads; lock it to serialize
revocation with mutation. Commands use the existing nonce/expected-version
envelope: create, preview, hold, release, execute and cancel.

Preview captures source/target qualification versions under deterministic
locks and server time. Require distinct live providers, a bounded five-minute
preview lifetime, and invalidate on hold/release. Execute rechecks the snapshot.
Define initial claim acquisition explicitly: the existing reaper only selects
claimed work and cannot start scheduled work. Serialize migration intent per
tenant with one reusable maintenance resource, fence prior holders, and check
the linked executing pass before claim settlement. Hold/cancel prevents
settlement. Claim ownership alone never proves a provider transfer completed.

Add migration 00007 without editing released migrations. Historical records
remain readable; do not fabricate permission/preview evidence. Refuse downgrade
that would discard new administration data. Exploration and accepted ADRs stay
unchanged. Review this design before implementing the new boundary.

## Evidence and verification

Record test names, layer, observed results and external effects for every
acceptance criterion. Reuse sufficient #25–#27 tests. Safe refusal audits and
denial records are permitted; refused actions change no authoritative lifecycle,
accounting or executable dispatch and cause no external effects.

Use bounded synchronization and explicit expiry rather than correctness sleeps.
Keep provider effect ledgers across fresh worker/pool restarts. Distinguish
lower-layer budget proofs from complete command/adapter acceptance. Shadow/off
observations must not advance primary cursors or lifecycle. No HTTP/UI, cloud
provider, production identity ceremony, penetration-test or DR campaign.
Post-action/failure hooks stay #30; SQL/outbox sufficiency stays #29.

Observed first-slice results: both spend defects and ignored late usage failed
new regressions before fixes. The expanded security tests passed under race
(20.548s); existing spend integration regressions passed under race (18.790s).
Contract schemas and coverage guards passed. Inverting the cumulative-usage
guard made the late-usage regression fail; the guard was restored. These results
do not establish the unfinished campaign or full-CI completion.

Demonstrate defects before accepting fixes. Integration runs are uncached and
use the repository's macOS SDK setup. Do not run competing heavy test campaigns
or execute focused recipes twice in CI, which already covers their packages.
