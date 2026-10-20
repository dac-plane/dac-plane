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
- [x] Extend tenant/operation forgery, authority denial/revocation/replay,
  partial/lost/duplicate cancellation and serialized action acceptance tests.
- [x] Prove same-budget reservation races at the store boundary and competing
  rework commands through the application. No ceiling increases or additional
  reservations/receipts/dispatch from invalid assertion replay.
- [x] Add restricted authenticated migration administration, migration 00007
  and ADR-0004. Prove permission, preview/hold invalidation and claim fencing.
- [x] Map artifact corruption and crash boundaries to existing evidence; add
  only missing full-path restart cases, including after metadata admission.
- [x] Add `just test-security` / `just test-security-race` and an architecture
  evidence note. Run focused, race and contract checks.
- [x] Complete final `just ci`, record its result and prepare the focused PR.

Implementation and evidence index are written. Final combined focused runs passed:
plain application 245.576s, race application 268.913s, with worker, PostgreSQL,
migration and simulator packages passing too. Final `just ci` passed, including
uncached race-enabled application integration (580.825s), worker (59.995s),
PostgreSQL (117.969s), migrations (28.374s) and all other packages. Module,
format, vet, unit/race, schema and workflow checks passed. No vulnerabilities
were found. This is not production-provider qualification or human verification.
The focused administration review found a reaper clock inconsistency, now fixed
by using database time both for selection and locked mutation. Its final review
reported no actionable production-code findings. External review was declined.

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

## Dedicated security pipeline follow-up

User requested the missing pipeline on this branch. Inspection confirmed that
`ci / Integration tests` already covers the security cases but does not expose
a dedicated acceptance check. Tavily and Context7 verified GitHub Actions event,
permission and concurrency guidance against official GitHub documentation:
https://docs.github.com/actions/using-workflows/workflow-syntax-for-github-actions

Plan: add `.github/workflows/security.yml` for pull requests, main pushes and
manual runs using the shared setup and `just test-security-race`. Partition the
race-enabled integration campaign with the same top-level test-name expression:
security runs matching tests across all packages; CI runs the complement. Keep
`test-integration-race` available as the complete suite and make `just ci` run
both partitions. Anchor selection to whole top-level names so subtest names
cannot create coverage gaps. Preserve the existing vulnerability job.

Verify workflow lint, formatting, disjoint/exhaustive top-level selection, then
`just ci` with real Docker and uncached integration tests. Update acceptance
notes with observed results. No application code or provider integration changes.

Follow-up verification passed: workflow lint, formatting, `git diff --check`,
and selection inventory (412 targets = 147 security + 265 complementary, no
intersection or omissions). `just ci` passed with uncached race-enabled
integration partitions: application 315.247s complementary and 284.120s security;
PostgreSQL 58.635s and 34.118s; migrations 2.051s and 32.830s. Unit/race, vet,
module checks and workflow lint passed; govulncheck found no vulnerabilities.
GitHub execution of the new workflow remains to be observed after pushing.
