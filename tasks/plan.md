# Issue #29 — SQL/outbox durability decision

Tracker: https://github.com/dac-plane/dac-plane/issues/29
Branch: `feature/29-sql-outbox-decision`, from merged `main` including #60.

## Decision

Accept PostgreSQL plus the transactional outbox for the bounded simulator-backed M1 lifecycle.
PostgreSQL remains the sole product-lifecycle authority; outbox delivery is at least once; stable
operation identities, fencing, and explicit reconciliation preserve safe behavior around external
effects. This decision does not claim universal exactly-once behavior or production-provider and
database-operations readiness. See [ADR-0006](../docs/adr/0006-sql-outbox-durability-decision.md).

## Completed work

- [x] Map existing failure-injection evidence for outbox writes, replay, claims, authority, spend,
  shadow state, artifacts and migration to the decision criteria.
- [x] Run focused race-enabled recovery, security/lifecycle and migration campaigns and contract
  agreement guards.
- [x] Record the evidence-backed bounded decision in an active ADR and link it from the architecture
  baseline. Keep exploration sources unchanged.
- [x] Replace the stale `internal/integration` verification path from the issue description with the
  repository's actual `just` recipes.
- [x] Run full CI and correct the pinned Go toolchain after the vulnerability check identified that
  Go 1.26.9 was affected by GO-2026-6809; Go 1.26.10 is the pinned verification toolchain.

## Observed verification

- `just test-contracts` passed.
- `just test-recovery-race` passed uncached: application 311.166s, worker 29.053s, PostgreSQL
  25.423s, simulator 1.481s.
- `just test-security-race` passed uncached: application 265.734s, migrations 22.866s, PostgreSQL
  25.578s, worker 13.753s, with the remaining selected packages passing.
- `just test-migration-race` passed uncached: application 34.859s, worker 1.414s, PostgreSQL 7.817s,
  migrations 22.499s, simulator 1.269s.
- `GOTOOLCHAIN=go1.26.10 just ci` passed all checks, including both integration-race partitions,
  vulnerability scanning and workflow lint. After updating the `go.mod` toolchain pin to 1.26.10,
  `go version` selected go1.26.10, `just mod-check` passed and `just vuln` reported no
  vulnerabilities.
- The first plain `just ci` on the host's pinned Go 1.26.9 reached `govulncheck` and reported
  GO-2026-6809 in `crypto/x509`; it was fixed upstream in Go 1.26.10. No durability test exposed an
  implementation defect.

## Limits retained

These results cover deterministic provider simulators and the tested PostgreSQL failure boundaries.
They do not qualify production providers, database failover or backup/restore, throughput or
availability objectives, universal exactly-once external effects, or semantic correctness of
provider results. Post-action/failure-hook delivery and typed hook evidence remain for #30.
