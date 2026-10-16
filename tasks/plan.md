# Implementation plan: issue #21 fenced dispatch and reconciliation

Tracker: [GitHub issue #21](https://github.com/AssahBismarkabah/dac-plane/issues/21) (epic #11)

## Objective

Turn committed outbox work into recoverable provider operations without
allowing stale workers to mutate lifecycle state. Pick-up, retry, fence,
give-up and reconcile are explicit and inspectable. Depends on #20 (merged).

## Sources

- `docs/architecture/README.md` ownership rules; `docs/architecture/
  review-2026-10-15.md` invariants (fenced leases; cancellation is an intent
  until both stops are observed; unknown outcomes enter reconciliation, never
  blind retries; shadow harnesses never mutate authoritative state).
- ADR-0002 modular monolith (worker is a module of the one control-plane
  process); ADR-0003 provider-contract semantics (demand is a scheduling hint;
  typed failures; protocol cancellation; unknown-durability reconciliation).
- `internal/lifecycle` pure machines (`execution.go` operation machine with
  `uncertain → reconcile` only; `claim.go` epoch fencing; `attempt.go` cancel
  path; `attention.go` decide + delivery path; `harness.go` shadow/live).
- `internal/contracts` (`frame.go` Request/Cancel/Reply + Demand;
  `port.go` Port/Stream; `stream.go` BindEvent/Ingest).
- `internal/platform/postgres` (#20: version-CAS everywhere, `appendHistory`
  atomic commit, `resume.go` readers with no consumer yet).
- `architecture-explore/` evidence only: prototype outbox (`attempts`,
  `available_at`, idempotency unique) and SPEC §22 (bounded backoff, up to 3
  attempts; cancel contract) justify the per-row shape; no policy table, no
  cancel table, no exhausted operation state, no demand column and no shadow
  table exist in any prototype.

## Design

- New package `internal/worker/` (dispatch, reconcile, cancel driver, claim
  reaper, scheduler, exhaustion classifier, loopback Port test seam),
  supervised from `internal/bootstrap` `Application.Run`.
- One additive migration `00002_dispatch.sql` (never edit `00001`): outbox
  `attempts`, `next_attempt_at`, `last_error`, `exhausted_at`, `demand`
  columns + `outbox_ready` partial index + append-only `shadow_result` table.
  No cancel table (intent is `cancelling`/`cancellation_requested` + cancel
  outbox command; ack is terminal states + stop observations). No new
  operation state (exhaustion is an outbox marker). No policy table (bound is
  worker config, default 3).
- Store helpers live with their rows: `outbox.go` (claim/ack/retry/exhaust
  with lease-epoch predicates; `OutboxReadyEntries`), `attempt_lease.go`
  (claim/renew/release on the existing `attempt.lease_*` columns), `shadow.go`
  (append-only record/list). `OutboxPayload` gains `Demand` (defaults to
  `background`).
- Pure helpers with unit tests: `Scheduler.Select` (person first, bounded
  background share, deterministic tie-break) and `Exhaustion.Classify`
  (non-retryable codes exhaust immediately; transient codes back off; budget
  exhaustion is terminal).

## Ordered tasks

1. Rewrite this plan (done, committed separately).
2. PR-21a (domain + storage foundation): `00002_dispatch.sql` +
   `CurrentVersion 1 → 2` + schema-test updates; `OutboxPayload.Demand`;
   `outbox.go`, `attempt_lease.go`, `shadow.go`, `OutboxReadyEntries`;
   `scheduler.go` + `exhaustion.go` with unit tests; migration coverage for
   the new columns/index/table.
3. PR-21b (`internal/worker/` + bootstrap): supervisor, dispatch loop with
   pre-dispatch checklist (live harness, budget reserve, no executing
   migration, lease held), reconciler, cancel driver over the existing
   machines, claim reaper, loopback Port, bootstrap wiring.
4. PR-21c (integration): one test per #21 acceptance bullet + race gate.
5. Full gate per PR: `gofmt -l cmd internal`, `go vet ./...`,
   `go vet -tags=integration ./...`, `go test ./...`,
   `just test-integration-race ./internal/worker/`,
   `just test-contracts`, `git diff --check`.

## Verification

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
just test-contracts
just test-integration ./internal/platform/postgres/ ./internal/platform/migrations/ ./internal/worker/
just test-integration-race ./internal/worker/
just ci
git diff --check
```

## Boundaries

- No Surface API endpoint, no HTTP frame, no second process.
- No edits to `00001_product_schema.sql`; all additive change in `00002`.
- No simulator implementation (#22); loopback Port stays in
  `internal/worker`, promoted only if #22 needs it.
- No changes to lifecycle machines, contract families, or store CAS patterns;
  #21 only consumes them.
- No baseline, diagram, or accepted-ADR edits; `architecture-explore/`
  read-only.
