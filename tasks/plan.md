# Implementation plan: issue #14 lifecycle transitions and audit semantics

Tracker: [GitHub issue #14](https://github.com/AssahBismarkabah/dac-plane/issues/14) (epic #11)

## Why this issue is next

#14, #15 and #16 depend only on finished work (#12, #32), but #15 and #16 build on what #14
defines: budget reservations bind to Task and Attempt, review dispositions need expected-version
conflicts, and "request changes" schedules a new Attempt. #17 (adapter contracts) depends on all
three. #14 therefore sets the record, version and transition patterns the rest of M1 reuses.

## Objective

Define the authoritative lifecycle records (Task, Attempt, AttentionRequest, Decision,
AuditEvent), their legal transitions with expected-version checks, and the provider-correlation
support models, as pure Go types and functions plus JSON Schema 2020-12. No persistence.

## Sources

- Historical spec §11 and §13 state machines, as prototyped in
  `architecture-explore/packages/product-api/src/machines.ts` (input, not authority).
- Review invariants: cancellation is intent until runtime and environment stop are observed;
  unknown outcomes enter an explicit uncertain state and are never retried as no-effect.
- ADR-0003: execution bindings, typed turn failures, unknown send durability, environment
  observations, fenced maintenance claims, `off`/`shadow`/`live` harness modes, migration passes.
- Plane-Bot evidence: DEEP-DIVES-04 §1 (session/box binding, `turn_failed`, `awaiting_user_response`),
  MASTER-RECORD (send status `ACCEPTED/REJECTED/PENDING/UNKNOWN_DURABILITY`, run state
  `ABSENT/STARTING/RUNNING/HIBERNATED`, claim semantics).

## Delivery: two PRs against #14

The issue has two separable halves. Splitting keeps each contract reviewable.

### PR A: lifecycle core (`internal/lifecycle`)

- A generic transition table `Machine[S, E]`: legal targets per state and event, terminal
  states, and `Apply(current, event, expectedVersion, target)` that rejects version conflicts,
  terminal states, illegal events and ambiguous targets with typed errors. The prototype's
  `defineMachine` shape, in Go.
- Task states (spec §11 plus review-driven changes): `draft, queued, running, waiting_input,
  verifying, ready_for_review, cancelling` and terminals `succeeded, failed, rejected,
  cancelled`. `rejected` (terminal review reject, #16) is distinct from `failed` (execution).
  `waiting_input` makes attention an explicit waiting condition, never a failure.
- Attempt states: `allocated, provisioning, starting, executing, waiting_input, finalizing,
  cancellation_requested, reconciling` and terminals `completed, failed, terminated`. A lost
  lease and an unknown provider outcome both enter `reconciling` (the event records which), so
  there is one recovery state rather than the prototype's separate `lost`.
  Cancellation stays `cancellation_requested` until both runtime-stop and environment-stop are
  observed (each recorded as an optional flag, absent until observed); one observation is not
  enough. An unknown provider outcome moves to `reconciling`,
  which has no path back to dispatch.
- AttentionRequest and Decision: spec §13 machine (`pending, decided, dispatching, delivered,
  consumed` and terminals). Every decision, including a denial, is delivered and its
  consumption observed. A Decision is bound to the exact request version it answered and the
  first valid decision wins.
- Illegal or late events are refused with a typed `TransitionError` that can itself be audited,
  so a late provider observation is recorded and never applied.
- `AuditEvent`: append-only, actor-attributed record of a command or accepted observation, with
  the record, from/to state and version. Distinct types from domain events and outbox delivery
  records (#20), which this PR does not define.
- New `ids` kinds: Task, AttentionRequest, Decision, AuditEvent.

### PR B: provider support models (`internal/lifecycle` or `internal/execution`)

- Execution bindings: runtime session, runtime turn, environment resource and provider
  operation, each correlated to exactly one Attempt.
- Typed turn failures (code, summary, safe details) and `awaiting_input` turn ends.
- Send durability `accepted | rejected | pending | unknown`; `unknown` enters reconciliation and
  is never redispatched.
- Environment observation `absent | starting | running | hibernated`, recorded as observed
  provider state.
- Harness mode `off | shadow | live`; migration passes with holds and preview/execute.
- Maintenance claims: expiring, fenced by claim epoch, stale claimant rejected.

## Verification

```sh
go test ./internal/lifecycle/...
go test -race ./internal/lifecycle/...
go vet ./...
test -z "$(gofmt -l cmd internal)"
```

Each machine gets an exhaustive table test: every (state, event) pair not in the table must be
rejected, so illegal transitions are proven rather than sampled. Schema agreement tests follow
the #13 pattern, and mutation checks are recorded in the PR.

## Boundaries

- No persistence, outbox, workers or HTTP.
- No provider-specific state names; Plane-Bot names appear only as evidence.
- Budget (#15) and artifact/result/review records (#16) are out of scope; Task states reserve
  the transitions those issues drive.
