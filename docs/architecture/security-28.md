# Security and lifecycle evidence — issue #28

This work follows the active architecture and ADR-0003. It is in progress, not
an M1 freeze or production security certification. Exploration remains unchanged.

## Spend approval admission

The command boundary locks the Attempt/Task scope and spend challenge before
sampling PostgreSQL `clock_timestamp()`. Caller `asserted_at` is evidence, not
authority to extend expiry. An already-consumed challenge cannot authorize a new
command nonce. The original nonce/fingerprint still replays its original result,
including after expiry. Accounting reservation and spend authorization remain
separate, and receipt plus dispatch still commit atomically.

Regression evidence:

- `TestSecuritySpendBackdatedAssertionCannotExtendExpiry` reproduced admission
  of a backdated proof after actual challenge expiry before the fix.
- `TestSecuritySpendProofCannotReplayThroughAnotherCommand` reproduced consumed
  proof admission through another nonce; original command replay is preserved.
- `TestSecuritySpendConcurrentProofHasOneWinningCommand` asserts one accepted
  command, receipt, reservation and external runtime effect.
- `TestSecuritySpendExpiryRecheckedAfterChallengeLock` blocks on an observed
  database lock, expires the challenge, then verifies no receipt or dispatch.
- `TestSecuritySpendTermsCannotBeSubstituted` refuses substitution of tenant,
  principal, Task, Attempt, operation, amount and unit without accounting writes.

## Authenticated usage accounting

`usage.reported` carries only a valid unit and bounded cumulative total, not
product record IDs. The authenticated event binding supplies Tenant/Attempt/
provider/operation. The product resolves the Task budget and its Attempt-owned
reservation, locks accounting rows, and commits accounting, audit/events and
ingestion cursor together. Missing budgets or reservations are refused rather
than treated as unconstrained. Missing or null cumulative evidence is not zero.

Equal or older cumulative observations acknowledge progress without reducing
recorded consumption. Late usage remains chargeable after terminal reservation
release and records overshoot; it does not reopen the reservation or revive work.
Integrity/accounting evidence does not prove provider-meter correctness or
implement the post-action/failure hooks still required at #30.

`TestSecurityUsageLateReportsRemainAccountedAfterTerminal` failed before this
entry was implemented. It covers released accounting, overshoot, stream replay,
equal/older cumulative observations, wrong units and payload identity smuggling.
`TestSecurityUsageNonLiveReportsCannotChangeAccounting` covers shadow/off
accounting, lifecycle and primary-cursor isolation. The JSON Schema and Go
agreement test cover required zero, integer bounds and unknown fields.

## Remaining work and migration execution boundary

The remaining acceptance matrix and implementation order are in `tasks/plan.md`.
Migration administration is not yet implemented by this slice. Its design review
identified completion evidence, initial claim acquisition, resource serialization,
preview freshness and revocation/replay requirements; those must be resolved in
the administration implementation, not delegated to an implied generic reaper.

[#60](https://github.com/dac-plane/dac-plane/issues/60) separately tracks actual
source/target migration execution and its recovery evidence. #11, #28 and #30
link that follow-up. No administration command may fabricate provider cutover or
claim a completed migration solely because a claimant owns a lease.

## Observed verification

The final first-slice `TestSecurity` race-enabled run passed (20.548s), including
the explicit shadow/off primary-cursor checks. Existing
spend regressions also passed under race (18.790s). `just test-contracts` passed.
A temporary inversion of the cumulative guard caused the late-usage regression
to fail; the original guard was restored. Full campaign and `just ci` have not
yet been run for #28. No external-model review was performed, by user choice.
