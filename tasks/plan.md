# Issue #30 — Freeze the M1 lifecycle contracts

Tracker: https://github.com/dac-plane/dac-plane/issues/30
Branch: `feature/30-contract-freeze`

## Completed

- [x] Preserve the active architecture: product-owned lifecycle, PostgreSQL/outbox authority,
  replaceable provider ports, and the explicitly bounded simulator-backed M1 claims.
- [x] Complete the post-action and action-failure enforcement hook path. The governed action result
  and follow-up outbox intent commit atomically; hook delivery does not repeat the action.
- [x] Persist immutable per-stage evidence under `(tenant, action operation, hook kind)`. Retain
  enriched pre-action evidence compatibly; persist only response/message/context digests and bounded
  typed usage, never hook text or action input.
- [x] Add the additive migration and refuse downgrade if it would discard new hook evidence.
- [x] Add simulator stage idempotency, contract/schema agreement, persistence and application/worker
  recovery regressions.
- [x] Publish the lifecycle traceability/evidence matrix and explicit limitations in
  `docs/architecture/contract-freeze-30.md`, linked from the architecture index.

## Observed verification

- `just test-contracts` passed.
- `just test-hook-lifecycle` passed.
- `just test-hook-lifecycle-race` passed.
- `just test-recovery-race` passed.
- `just test-security-race` passed.
- `just test-migration-race` passed.
- `just ci` passed all checks, including formatting, module checks, vet, unit/race tests, both
  uncached PostgreSQL integration-race partitions, vulnerability scan (`No vulnerabilities found`),
  and workflow lint. The general integration partition passed with application 330.320s, PostgreSQL
  68.812s, worker 32.172s; the selected race partition passed with application 316.668s, migrations
  32.781s, PostgreSQL 32.859s, worker 18.213s. All remaining packages passed or had no selected
  tests.
- `git diff --check` passed.

## Limits

Evidence is from repository contracts and deterministic simulators. It does not qualify production
providers, transport, PostgreSQL failover/backup/restore, scale, or semantic correctness of hook or
agent results. Delivery remains at least once and uncertain outcomes remain explicit. Remote PR
checks are separate from the local verification above.
