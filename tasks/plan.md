# Issue #26: verified artifacts, immutable results and review/rework

Tracker: [#26](https://github.com/dac-plane/dac-plane/issues/26).
Branch: `feature/26-artifact-results`, based on merged #25 at `34238eb`.
One feature branch carries implementation and review corrections.

## Sources and boundary

The active architecture and ADR-0003 govern the work. Exploration SPEC sections
11 and 21 explain why only an explicit adapter-reported proposed result starts
verification, and why integrity admission is distinct from semantic correctness.
Exploration files remain read-only.

A successful native turn alone creates no result. A bound result proposal plus
its successful turn activates staged evidence export. Immutable store receipts,
not object presence or runtime claims, authorize product-owned admission.
Only a fully admitted proposal creates a revision; authorized owner review
accepts, rejects or schedules a new budgeted Attempt.

No cloud store, transport, UI, retention policy, automatic acceptance, production
provider qualification or arbitrary verification runner is selected here.

## Ordered work

1. Contracts: typed result.proposed and artifact transfer/observation/commit
   payloads, integrity-verification record, typed identifiers, schemas and
   agreement tests.
2. Durability: migration 00006, immutable proposal/staged identity, separate
   writer generation, stage operation purposes, commit/verification evidence,
   revision links and guarded downgrade.
3. Application: serialized proposal/turn join in both observation orders,
   finalization gates, owner-only idempotent review, atomic budgeted rework and
   spend assertion support for review scheduling records.
4. Worker/simulators: dedicated artifact registry and routing, stable replay-safe
   stage operations, bounded fenced calls, byte-backed hashing/immutable commit,
   crash recovery without inferring admission from stat.
5. Acceptance: application/worker end-to-end and race tests, mismatch/forgery/
   cancellation/claim-loss tests, migration and immutability proof, mutation
   check, focused review, then full CI.

## Invariants

- Proposal identity comes from authenticated runtime bindings. Providers do not
  choose product identifiers or writer generations.
- Scope locking follows Attempt then Task, consistent with existing progress
  transitions. Proposal and successful-turn observations join under these locks.
- Artifact generations are independent of outbox/Attempt lease epochs.
- Upload capabilities exist in call memory only; PostgreSQL and outbox hold
  immutable manifests, identifiers and integrity metadata, never credentials.
- A receipt must match the whole staged manifest and winning store identity.
  Stat and send acceptance alone never create admitted evidence.
- Retry/fenced/mismatch/partial admission creates no ResultRevision.
- All artifacts in a new revision belong to its exact Attempt and source
  revision. Prior evidence stays attached to prior immutable revisions.
- Cancellation, stale/future writers and shadow qualification block admission.
- Request changes preserves review/revision lineage, uses the existing ceiling,
  and commits scheduling/reservation/spend records atomically.
- Runtime lookup/reconciliation excludes artifact stage operations. Runtime
  uncertainty during verification restores finalizing rather than executing.

## Verification

Run uncached focused application and worker integration/race suites, the
PostgreSQL and migration suites, contract guards, and `just ci`. macOS uses the
SDK selection in justfile. Record only observed completed results.

## Completed verification

Implementation and acceptance tests are committed on the feature branch. The
final full run passed:

```sh
SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" just ci
```

The final race-enabled integration run passed application (279.239s), worker
(39.298s), PostgreSQL (86.506s) and migrations (19.712s). Format/module checks,
vet, unit/race tests, schema coverage, vulnerability scanning and workflow lint
passed. Vulnerability scanning reported no vulnerabilities.

The focused application acceptance suite also passed with and without the race
detector. Additional focused runs proved shared initial/rework scheduling and
lease expiry during the final budget-lock wait. A temporary inversion of the
verification digest check made its contract test fail; restoring the exact gate
made it pass. The mutation was never committed.

The bounded review found and corrected proposal/turn join races, verification
failure/reconciliation phase handling, conflicting cancellation/worker lock
orders, unlocked harness demotion and aggregate stage latency exceeding a lease.
The final review pass found no concrete blockers. The separately offered external
review was explicitly skipped by the user and was not run.

No production provider, cloud storage, network transport or deployed environment
was exercised. Local full CI is not a claim about unrun GitHub checks.

## Remaining M1 boundary

Broader recovery/security campaigns remain #27/#28. The post-action/failure-hook
delivery and safe typed hook evidence requirement from #25 remains an explicit
#30 freeze gate; it is not silently claimed by artifact integrity admission.
