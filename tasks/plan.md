# Implementation plan: issue #24 event ingestion and attempt reconciliation

Tracker: [GitHub issue #24](https://github.com/AssahBismarkabah/dac-plane/issues/24) (epic #11)

## Objective

Advance Attempts and Tasks from authenticated provider observations while preserving the
replay, uncertainty and cancellation invariants: bind and authenticate every event before any
mutation, ingest cursor-based streams idempotently, turn normalized entries into product
records and transitions, move unknown outcomes into explicit reconciliation, and converge
cancellation only after both runtime and environment stops are observed or reconciled.
Attention, decisions and cancellation enter through the command boundary from #23.

Verification (from the issue):

```sh
go test -tags=integration ./internal/application/... -run 'Event|Attention|Decision|Cancel|Reconcile'
go test -race -tags=integration ./internal/application/... -run 'Event|Cancel|Reconcile'
```

## What exists (verified on `main` at `955d7be`)

- `contracts.BindEvent` / `contracts.Ingest` (#17): bind an event to the authenticated `Peer`
  and a recorded `ProviderOperation`; advance a cursor, count duplicates, flag stale
  generations and rehydration. Frozen by the #18 gate.
- Store (#20/#21): `ApplyAttempt`, `ApplyTask`, `InsertAttention`/`ApplyAttention`/
  `DecideAttention`, `InsertBinding`/`ListBindings`, `InsertTurnEnd`, `UpsertObservation`,
  `UpsertCursor`/`GetCursor`, `ApplyOperation`; all with replay and refusal evidence.
- Worker (#21): sends dispatch rows, delivers `attempt/request_cancel` rows as protocol
  cancels (`dispatch.go:94`), marks lost replies `uncertain`, and reconciles uncertain
  operations from `operation_outcome` entries on the `dispatch` stream (`reconcile.go`).
- Application (#23): `Submit`/`Assert` leave the Task `queued`, the Attempt `allocated` and
  the runtime operation `dispatched`.
- **Gap:** nothing moves an Attempt or a Task after submission. No code applies
  `AttemptOutcomeUnknown`, `AttemptReconciled`, `AttemptRequestCancel`, the stop
  observations, or any Task event beyond `submit`. The entry vocabulary exists only in a
  simulator test (`progress`, `attention`, `turn_end`, `operation_outcome`), and that test has
  the provider send a full product `AttentionRequest`, which ownership rule 7 forbids.

## Sources

- Baseline ownership rules 2, 3, 7: runtime sessions correlate to an Attempt; the product
  stores environment correlation and observed state without becoming a compute controller;
  provider events are observations until authenticated, deduplicated and accepted.
- Review invariants: events bound to tenant, Attempt, operation and sequence before they
  affect state; cursor acknowledgement is ingestion progress, not completion; cancellation is
  intent until runtime and environment stops are observed or reconciled; unknown outcomes
  enter an explicit uncertain state and are never retried as no effect.
- ADR-0003: replayable observation stream (connected, rows, cleared, cursor-too-old,
  heartbeat, tombstones, rehydration); execution bindings; typed turn failures; environment
  observations `absent | starting | running | hibernated | unknown`.
- Evidence (shape only): Plane-Bot transcript watch frames, `turn_failed {code, summary,
  error_details}`, turn end `awaiting_user_response` (the needs-input transition), interrupt
  acknowledging `had_active_run` (DEEP-DIVES-04 §1); historical SPEC §11 (stop reasons:
  only a proposed result moves to verification; idle, token limit and user cancel are not
  success) and §13 (persist the attention request before displaying it; deliver the decision
  through the runtime; observe consumption).

## Decision A: a product-defined entry vocabulary (needs agreement)

Providers report observations; the product builds every record from the **bound event's**
identities (tenant, Attempt, provider, operation). A payload can never name or override them.
New `internal/contracts/entries.go` plus `contracts/jsonschema/adapter/entry-*.schema.json`
(guarded by both contract coverage tests and the vendor-name test):

| Entry kind | Payload (provider observation) | Product effect |
|---|---|---|
| `binding` | `kind` (runtime_session, runtime_turn, environment_resource), `native_ref`, `session_ref` for a turn | `lifecycle.Binding` from the event's ids; `CheckBinding` refuses a resource bound to another Attempt. A first `runtime_session` advances the Attempt `allocated -> provisioning -> starting -> executing` and the Task `queued -> running`. |
| `turn_end` | `session_ref`, `turn_ref`, `outcome`, `failure` (typed code, summary, details digest), `ended_at` | `lifecycle.TurnEnd`, rejected unless its session is bound to this Attempt. `completed`: Attempt `finalize`, Task `propose_result` (verification and results are #26). `awaiting_input`: Attempt and Task `await_input`. `failed`: Attempt `fail`, Task `execution_failed`, failure code and details preserved. `interrupted` while cancellation is requested: the runtime stop. |
| `attention` | `native_ref`, `kind`, `action_digest` for an approval, `expires_at` | The product creates the `AttentionRequest` (responders: the task owner; current policy generation; provider and operation from the event), then Attempt and Task `await_input`. |
| `environment_observation` | `resource_ref`, `state`, `sequence`, `observed_at` | `EnvironmentObservation` (newer sequence supersedes; resource must be bound to this Attempt). `absent`/`hibernated` while cancellation is requested: the environment stop. |
| `operation_outcome` | `state` | Unchanged worker semantics for uncertain operations; additionally resolves the Attempt from `reconciling`. |
| `progress` | free JSON object | Acknowledged (cursor advances); no record, no transition. |

Unknown entry kinds advance the cursor and are recorded as refused evidence, never applied.

## Decision B: ingestion is a provider-authenticated command

`application.Ingest(ctx, pool, peer contracts.Peer, event contracts.Event) (Ingestion, error)`:
the transport authenticates `peer`; the command loads the operation, runs `BindEvent`, loads or
starts the cursor, runs `Ingest`, applies accepted entries, and commits records, transitions,
audit (actor: the provider) and the new cursor in one transaction. Idempotency comes from the
stream, not a client nonce: duplicates at or below the cursor are skipped, and every effect
also has a natural key (binding native ref, turn end session/turn, attention native ref,
observation sequence), so a rehydration replay from the start of a generation converges
without repeating transitions. Cleared and cursor-too-old reset the cursor and report
`Rehydrate`; missing history is never read as success.

Production subscription transports stay out of scope; the worker's existing loop is the only
subscriber, and tests drive `Ingest` with simulator frames.

## Decision C: unknown outcomes and cancellation

- An operation that becomes `uncertain` moves its Attempt to `reconciling`
  (`outcome_unknown`), applied when the uncertainty is ingested or reconciled; a reconciled
  outcome resolves the Attempt with `reconciled` to the observed state. `reconciling` never
  returns to pre-dispatch states (machine rule from #14).
- `CancelTask` command (principal, nonce, expected Task version): Task `cancel`, Attempt
  `request_cancel`, and an `attempt/request_cancel` outbox row carrying a `DispatchBody` with
  `CancelReason`, all in one transaction. The worker delivers the protocol cancel.
- The Attempt terminates only after both stop observations (runtime: `turn_end` interrupted
  or an `operation_outcome` after cancel; environment: `absent`/`hibernated`), or a
  reconciliation; then the Task gets `stop_confirmed`.

## Decision D: attention and decisions through the command boundary

`DecideAttention` command (principal, nonce, request version, current policy generation):
`DecideAttention` in the store, then an outbox row that delivers the decision to the runtime
(`steer_turn` with the decision digest; the reply consumes the request), Task
`input_resolved` and Attempt `resume` once consumption is observed. Approve-once also derives
the allow-once grant from the request (#39's `AllowOnceGrant`); enforcing it is #25.

## Delivery: two PRs against #24

- **PR A, ingestion and reconciliation:** entry vocabulary and schemas, `Ingest`, cursor
  handling, bindings, turn ends, environment observations, Attempt/Task progression,
  unknown-outcome reconciliation. Tests named `TestEvent*` and `TestReconcile*`.
- **PR B, attention, decisions and cancellation:** `attention` entries, `DecideAttention`,
  decision delivery and consumption, `CancelTask`, stop convergence. Tests named
  `TestAttention*`, `TestDecision*` and `TestCancel*`.

## Acceptance tests (both PRs)

- Forged envelopes (another provider's peer, another tenant, an operation of another
  Attempt) and observations whose session or resource is not bound to the Attempt are
  refused before any mutation, with refusal evidence.
- A duplicate rows frame, a duplicate tombstone and a full replay after `cleared` repeat no
  transition and create no second record; the cursor only moves forward within a generation.
- `cleared` and `cursor_too_old` report rehydration and change no lifecycle state; heartbeats
  change nothing.
- A failed turn preserves code, summary, details digest and provider operation.
- A lost reply moves the Attempt to `reconciling`; an `operation_outcome` resolves it.
- Cancellation: one stop observation leaves the Attempt in `cancellation_requested`; both
  terminate it and confirm the Task stop; a late `turn_end completed` after cancel is
  refused and recorded.
- Attention: an `attention` entry creates the request and waits; a decision is delivered,
  consumed, and resumes the work; a stale-version decision conflicts.

## Boundaries

- No artifacts, result revisions or reviews (#26); no governed-action enforcement (#25).
- No production event transport; `Peer` comes from the caller as the transport's
  authentication result.
- The frozen #17 contracts and the 00001 to 00003 schema are not changed; entry payload
  schemas are additive.
