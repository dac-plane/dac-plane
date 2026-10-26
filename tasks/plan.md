# Issue #74 — Prove operational recovery and rolling upgrades

Tracker: https://github.com/dac-plane/dac-plane/issues/74
Rollout defect: https://github.com/dac-plane/dac-plane/issues/87
Recovery blocker: https://github.com/dac-plane/dac-plane/issues/88
Branch: `feature/74-operational-evidence`, from `main` at `ca20b3e`.
Parent: #65 (M2: Deployable control plane).

Confirmed with the maintainer on 2026-10-26: correct the unsafe rollout order first, then prove #74
with distinct old and next-release images. One PR closes #74 and #87. The schema-14 migration is an
operations-only fixture and never enters the production migration source.

## Findings that shape the work

1. ADR-0009 and the Compose runbook say to migrate before rolling replicas, but the first deployable
   release rejects every schema above 13. A running old replica also keeps its worker active after
   readiness notices an out-of-range schema. Issue #87 records the defect.
2. The base Compose service publishes fixed ports and the simulator dials one adapter endpoint, so
   scaling that service does not create a deterministic two-replica test topology.
3. The in-process simulator already has exact before/after-effect pauses and an effect ledger, but the
   separate-process simulator exposes neither. Operations evidence needs a test-only control surface.
4. PostgreSQL and outbox integration tests prove the mechanisms in-process; #74 must exercise them
   through deployed processes and preserve the distinction from provider qualification.

## Work, in order

### 1. Correct the rollout decision

- Add ADR-0011, superseding only ADR-0009's migration-first rollout order.
- Roll every compatible new binary while the database remains on the current schema, then run the
  explicit advisory-locked migration and verify readiness.
- Permit binary rollback before migration. After migration, permit it only if the previous binary
  declares the resulting schema in its serving range.
- Update the Compose operator runbook and architecture links without changing the remaining ADR-0009
  decisions.

### 2. Add operations-only release fixtures

- Keep the normal image at production schema 13.
- Build the old image from pinned revision `ca20b3e7438707f58d8735251f8ffb8abc5a7a32`.
- Behind an `operationstest` build tag, build a next-release image that serves schemas 13–14 and
  embeds one additive schema-14 probe migration. Tests prove the tagged source/range and prove the
  normal source/range do not contain schema 14.
- Add a dedicated Compose overlay with two named replicas, unique published ports, shared PostgreSQL,
  and operations-only simulator control. Cleanup is scoped by Compose project name and preserves
  failure logs under `.work/operations/`.

### 3. Make the simulator controllable without changing production contracts

- `DAC_ADAPTER_URLS` lets the simulator keep concurrent connections to both replicas while sharing
  one runtime, artifact store and ledger; existing `DAC_ADAPTER_URL` remains the single-endpoint form.
- `DAC_SIM_CONTROL_ADDR`, when set, starts a test-only HTTP listener for exact pause registration,
  reached/release coordination, per-operation delivery/effect counts, and artifact digest/size
  inspection. It is absent by default and exposed only on host loopback by the operations overlay.
- Validate every role, operation, boundary and request size. Return status/evidence only, never tokens
  or configuration.
- Log the random worker owner at startup so the runner can map a database claim to a replica without
  changing owner generation or adding a product endpoint.

### 4. Implement four deployed scenarios

- **Fencing:** pause after an effect, pause the claimant container, expire only the named outbox and
  attempt leases, let the peer take over, then resume the stale claimant. Assert one effect, a higher
  winning epoch and no stale settlement.
- **Restart:** pause before the effect, kill the claimant, expire its named leases, restart it and let
  valid recovery finish. Assert durable state, cursor recovery and exactly one effect.
- **Backup/restore:** stop writes at `ready_for_review`, take a custom-format logical dump, recreate
  the database from `template0`, restore as `dac_migrate`, restart both replicas while the external
  simulator store remains alive, compare task/revision/artifact metadata and bytes evidence, then
  accept the restored revision to `succeeded`.
- **Upgrade:** start two pinned old-image replicas at schema 13, begin a task, roll both replicas to
  the tagged next image one at a time, migrate to fixture schema 14, and finish the same task. Assert
  API availability, adapter reconnection, one effect, new-replica readiness and old-image refusal at
  schema 14.

### 5. Recipes, CI and evidence

- Add one recipe per scenario plus `just test-operations`; keep `just test-stack` unchanged.
- Add a separate blocking CI operations job with full Git history and bounded timeout.
- Record observed commands, versions, revisions/digests, claim owners/epochs, effect counts, artifact
  hashes, durations and failures in `docs/architecture/operations-74.md`.
- State the limits: Compose, one PostgreSQL primary, logical backup, deterministic simulators, no
  provider qualification, HA, throughput, RPO, RTO or availability claim.
- A remaining failed scenario opens a scoped issue and keeps #74 open; it is never waived.

## Verification

- Unit: tagged/default migration isolation and range; multi-endpoint simulator; pause lifecycle;
  ledger/artifact inspection; malformed control requests; control disabled by default.
- Deployed: the four operations scenarios using HTTPS, database observations, container controls and
  simulator evidence.
- Regression gates: `just test-operations`, `just test-stack`, `just image-repro`, then `just ci`.
- Before merge, review the complete diff for correctness, architecture, security, performance and
  scope; address every verified finding and record commands actually observed.

## Deviations

- The required kill/restart scenario exposed a contract conflict after implementation. A worker
  killed at the simulator's `before_effect` boundary has already committed `send_started`. The
  accepted recovery baseline therefore makes the takeover uncertain and forbids the replacement
  resend; the observed simulator effect count is zero. Issue #88 records the decision and protocol
  work needed to make this scenario converge safely. Per the plan, #74 remains open and this is not
  waived or weakened into a passing assertion.
- Because #88 blocks one of four required scenarios, the original assumption that this branch can
  close #74 and #87 in one merge no longer holds. The branch can close #87 only after review; #74
  remains open until #88 is resolved and all four deployed scenarios pass.
