# Implementation plan: issue #15 authoritative budget accounting

Tracker: [GitHub issue #15](https://github.com/AssahBismarkabah/dac-plane/issues/15) (epic #11)

## Objective

Define the product-owned Budget contract: integer reservation before dispatch, idempotent usage
consumption, exactly-once release, ceiling enforcement with recorded in-flight overshoot, and a
provider-neutral two-phase `SpendChallenge` that never reserves or consumes budget. Pure Go types
and functions plus JSON Schema 2020-12; no persistence.

## Sources

- Baseline: Budget is one of the nine product-owned records (`docs/architecture/README.md`
  ownership rule 1); the review's contract-freeze item 2 (reservation, consumption, release and
  ceiling before and during execution).
- ADR-0003 "Spend authorization": begin challenge, assert, persist receipt; bind tenant,
  principal, Task, Attempt, provider operation, amount, unit and replay identity; expired,
  replayed or mismatched assertions fail; authorization is separate from Budget accounting; the
  ceremony and credential provider stay open.
- Historical spec §25 (input, adopted only through the above): reserve estimated budget before
  dispatch, release unused reservation, halt at the ceiling, in-flight usage can overshoot.
- Plane-Bot DEEP-DIVES-04 §1 spend ceremony (evidence for the two-phase shape only; WebAuthn and
  payment providers are not selected).

## Design (`internal/lifecycle`)

The issue names `internal/domain`; the records live in `internal/lifecycle` with the other
lifecycle records, reusing `Change`, `AuditEvent` and the typed identifiers (#14 set this).

- `Unit`: a validated lowercase unit name (`usd_micros`, `tokens`, ...). Amounts are `uint64`
  bounded to 2^53-1 so JSON consumers read them exactly; all arithmetic is overflow-checked.
- `Budget` (one per Task and unit): `Ceiling`, aggregate `Reserved` (outstanding holds) and
  `Consumed`, the largest `Overshoot` observed, and `Version`. Every change is a
  compare-and-set on `Version`, so competing reservations serialize.
- `Reservation` (one per Attempt or dispatched operation): `Amount` held, cumulative `Consumed`,
  state `held | released`.
- `Reserve`: allowed only while `Reserved + Consumed + amount <= Ceiling`; otherwise refused
  with `ceiling_reached`. Reaching the ceiling therefore stops further dispatch.
- `ReportUsage(cumulative)`: providers report a cumulative meter per reservation; the product
  applies `max(current, reported)`. A replayed or reordered report never double-counts and never
  loses a charge. Usage beyond the held amount (in flight) is still recorded and raises the
  budget's recorded `Overshoot`. Usage after release (late usage) is recorded the same way.
- `Release`: returns the unused hold to the budget exactly once; a second release is refused.
- `SpendChallenge`: `pending -> asserted | expired`, binding tenant, principal, Task, Attempt,
  operation, amount, unit, a replay nonce and expiry. `Assert` checks every bound field against
  the assertion, refuses expiry and replay, and returns a `SpendReceipt`. No budget function
  takes a challenge or receipt: authorization never substitutes for reservation.

## Ordered tasks

1. Identifiers (Budget, Reservation, SpendChallenge), audit record kinds and rejection codes.
2. Budget and Reservation with Reserve, ReportUsage and Release; table tests for non-negativity,
   overflow, competing reservations, replayed and reordered usage, release-once and overshoot.
3. SpendChallenge, assertion and receipt with expiry, replay and mismatch tests.
4. JSON Schemas and agreement tests (the contract-coverage test requires them).
5. Mutation checks and the full CI set, including uncached integration tests.

## Verification

```sh
go test ./internal/lifecycle/... -run 'Budget|Reservation|Spend'
go test -race ./internal/lifecycle/...
go test -count=1 -tags=integration ./...
```

## Boundaries

- No currency conversion, provider billing APIs, predictive pricing or payment provider.
- No cryptographic ceremony: the assertion carries a proof digest only.
- Persistence, unique constraints and the dispatch transaction that couples reservation with
  provider operation are #19 to #21.
