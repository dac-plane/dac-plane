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

## Alignment with `architecture-explore/` (checked 2026-10-18)

The first draft of this plan was checked against the exploration evidence. Seven points
changed; each row names the evidence and the revision below.

| # | First draft | Evidence | Revision |
|---|---|---|---|
| 1 | Flat entry names (`binding`, `turn_end`, `attention`) | SPEC §14 event families and the prototype `EVENT_TYPES` (`runtime.started`, `turn.started`, `turn.finished`, `attention.requested`, `decision.consumed`, ...) | Use the documented dotted names (Decision A) |
| 2 | Every unknown kind refused | SPEC §14 and prototype `classifyEventType`: unknown optional progress is stored generically; unknown permission or state-changing semantics are rejected; "an observation cannot grant permission by name" | Three-way classification (Decision A) |
| 3 | `turn_end completed` finalizes the Attempt and proposes a result | SPEC §11: an end-of-turn moves to verification only when the adapter reports a proposed result; idle, token limit and user-cancel are not success. Prototype `taskEventForStopReason` | A completed turn is recorded and changes no state; result proposal is #26 |
| 4 | An attention entry is the only waiting signal; no limit | SPEC §11: one blocking attention request per attempt in v1; §12 step 5: persist the request before displaying it | `attention.requested` creates the request and the wait; a second open blocking request is refused |
| 5 | Decision resumes work once delivered | SPEC §13: "An answer is not proof that the runtime resumed"; redeliver with the same decision ID; prototype event `decision.consumed` | Resume only on an observed `decision.consumed` (Decision D) |
| 6 | Cancel moves the Task and Attempt only | SPEC §13/§22: cancelling invalidates pending authorizations and native waits, revokes unused grants; ADR-0011(b): from any state with possibly live execution the task stays `cancelling` until the stop is confirmed | Cancel also cancels open attention, revokes unused grants, and the dispatch moves the Task and Attempt out of directly-cancellable states (Decision C) |
| 7 | Attention native ref and turn operation as natural keys | Prototype `0002_native_correlation.sql`: one native approval id maps to exactly one attention request, kept in a correlation table; our `attention_request` has no native ref and `turn_end` has no operation | Migration `00004` adds an attention correlation table; a turn end links to its operation through its turn binding (Decision B) |

Confirmed unchanged by the evidence: the ingestion boundary supplies or verifies tenant and
attempt identity instead of trusting the payload (SPEC §14); deny resumes work and blocks only
that action (ADR-0011(b)1, SPEC §11); late or stale terminal observations are retained for
audit and never applied (ADR-0011(b)3, SPEC §11); duplicates are deduplicated and never
executed twice, unknown external outcomes are never blindly repeated (SPEC §22).

## Decision A: a product-defined entry vocabulary (SPEC §14 names)

Providers report observations; the product builds every record from the **bound event's**
identities (tenant, Attempt, provider, operation); a payload never names or overrides them.
New `internal/contracts/entries.go` plus `contracts/jsonschema/adapter/entry-*.schema.json`,
guarded by both contract coverage tests and the vendor-name test. Inline payloads above
256 KiB (the SPEC §14 proposed default) are refused; larger content travels as
`payload_omitted` with its hash.

| Entry kind | Payload (provider observation) | Product effect |
|---|---|---|
| `runtime.started` | `native_ref` (session) | `Binding` runtime_session (`CheckBinding` refuses a session bound to another Attempt); Attempt `provisioning -> starting -> executing` |
| `turn.started` | `native_ref` (turn), `session_ref` | `Binding` runtime_turn, refused unless the session is bound to this Attempt |
| `turn.finished` | `session_ref`, `turn_ref`, `outcome`, typed `failure`, `ended_at` | `TurnEnd`; binds the turn from the event's operation if `turn.started` was not seen, so the failure keeps its operation. `completed`, `awaiting_input`: recorded, no transition. `failed`: Attempt `fail`, Task `execution_failed`, code and details preserved. `interrupted` during cancellation: the runtime stop |
| `runtime.stopped` | `had_active_run` | The runtime stop acknowledgement during cancellation (Plane-Bot interrupt shape) |
| `environment.observed` | `resource_ref`, `state`, `sequence`, `observed_at` | `EnvironmentObservation` (newer sequence supersedes; resource must be bound or is bound from the event); `absent`/`hibernated` during cancellation: the environment stop |
| `attention.requested` | `native_ref`, `kind`, `action_digest` for an approval, `expires_at` | The product creates the `AttentionRequest` (responders: the task owner; current policy generation; provider and operation from the event) plus its correlation row; Attempt and Task `await_input`; refused while another blocking request is open |
| `decision.consumed` | `native_ref` of the request | Attention `consume`; Task `input_resolved`; Attempt `resume` |
| `operation_outcome` | `state` | Unchanged #21 worker semantics (name kept for compatibility); additionally resolves the Attempt from `reconciling` |
| `message.delta`, `tool.proposed`, `tool.finished`, `checkpoint.created`, `progress` | object | Known progress: acknowledged, no record, no transition |

Unknown kinds follow SPEC §14: a kind with a state-changing prefix (`task.`, `attempt.`,
`attention.`, `decision.`, `action.`, `result.`, `environment.`) or naming approval, grant or
permission is refused with evidence; any other unknown kind is acknowledged as generic
progress.

## Decision B: ingestion is a provider-authenticated command

`application.Ingest(ctx, pool, peer contracts.Peer, event contracts.Event) (Ingestion, error)`:
the transport authenticates `peer`; the command loads the operation, runs `BindEvent`, loads or
starts the cursor, runs `Ingest`, applies accepted entries in order, and commits records,
transitions, audit (actor: the provider) and the new cursor in one transaction. Idempotency
comes from the stream, not a client nonce: entries at or below the cursor are skipped, and every
effect also has a natural key (binding native ref, turn end session/turn, attention correlation
native ref, observation sequence), so a rehydration replay converges without repeating
transitions. Cleared and cursor-too-old reset the cursor and report `Rehydrate`; missing
history is never read as success.

Migration `00004_attention_native.sql` (additive): `attention_native (tenant_id, provider_id,
native_ref, attention_id)`, unique on the native key and on the attention, mirroring the
prototype's correlation table without changing the frozen `AttentionRequest` record.

Production subscription transports stay out of scope; tests drive `Ingest` with simulator
frames, and the worker's reconcile loop stays the only subscriber.

## Decision C: dispatch, unknown outcomes and cancellation

- **Dispatch leaves the cancellable-directly states.** Enqueueing a runtime dispatch (in
  `Submit` and `Assert` from #23) now also applies Task `dispatch` (`queued -> running`) and
  Attempt `provision` (`allocated -> provisioning`) in the same transaction: once a dispatch
  may be sent, execution may be live, so cancellation must wait for stops (ADR-0011(b)2).
  A governed submission stays `queued`/`allocated` until its receipt enqueues the dispatch.
- **Unknown outcomes.** An operation that becomes `uncertain` moves its Attempt to
  `reconciling` (`outcome_unknown`) and supersedes its open attention request (SPEC §13: a
  dead native process supersedes its request); an `operation_outcome` resolves the Attempt with
  `reconciled` to the observed state.
- **`CancelTask` command** (principal, nonce, expected Task version), one transaction:
  - `queued` (nothing enqueued): Task `cancel -> cancelled`, Attempt `request_cancel ->
    terminated`, reservation released;
  - possibly live: Task `cancel -> cancelling`, Attempt `request_cancel ->
    cancellation_requested`, an `attempt/request_cancel` outbox row with a `CancelReason`
    dispatch body, open attention `cancel`, unused allow-once grants of the Attempt revoked.
- **Convergence.** The Attempt terminates only after both stops (runtime: `runtime.stopped`
  or `turn.finished interrupted`; environment: `environment.observed absent/hibernated`), or a
  reconciliation: an Attempt that never bound an environment resource reconciles the
  environment stop, and a dispatch that was never delivered reconciles the runtime stop. On
  termination the Task gets `stop_confirmed` and the reservation is released exactly once
  (#15). Late `turn.finished`/`operation_outcome` after cancellation are refused and recorded.

## Decision D: attention and decisions through the command boundary

`DecideAttention` command (principal, nonce, request version, current policy generation):
store `DecideAttention`, then a `DeliverDecision` outbox row (stable id: the decision id, so
redelivery reuses it) that the worker sends as a runtime `steer_turn` carrying the decision
kind and answer digest; a delivery reply moves the request to `delivered`; only an ingested
`decision.consumed` moves it to `consumed` and resumes work (Task `input_resolved`, Attempt
`resume`). Deny follows the same path and resumes work with the action blocked (ADR-0011(b)1).
Approve-once also issues the allow-once grant derived by `AllowOnceGrant`; enforcing it at
dispatch is #25.

## Delivery: two PRs against #24

- **PR A, ingestion and reconciliation:** entry vocabulary and schemas, `Ingest`, cursor
  handling, bindings, turn ends, environment observations, dispatch moving Task and Attempt,
  unknown-outcome reconciliation. Tests named `TestEvent*` and `TestReconcile*`.
- **PR B, attention, decisions and cancellation:** migration `00004`, `attention.requested`,
  `DecideAttention`, decision delivery and `decision.consumed`, `CancelTask`, stop convergence,
  grant revocation and reservation release. Tests named `TestAttention*`, `TestDecision*` and
  `TestCancel*`.

## Acceptance tests (both PRs)

- Forged envelopes (another provider's peer, another tenant, an operation of another
  Attempt) and observations whose session or resource is bound to another Attempt are refused
  before any mutation, with refusal evidence.
- A duplicate rows frame, a duplicate tombstone and a full replay after `cleared` repeat no
  transition and create no second record; the cursor only moves forward within a generation.
- `cleared` and `cursor_too_old` report rehydration and change no lifecycle state; heartbeats
  and progress kinds change nothing; an unknown `attempt.*` or `*approved*` kind is refused.
- `turn.finished completed` changes no state; `failed` preserves code, summary, details digest
  and the provider operation.
- A lost reply moves the Attempt to `reconciling` and supersedes open attention; an
  `operation_outcome` resolves it.
- Cancellation: a queued governed task cancels directly; a dispatched one waits; one stop
  leaves `cancellation_requested`; both terminate, confirm the Task stop and release the
  reservation once; an Attempt with no environment binding reconciles that stop; a late
  `turn.finished` after cancel is refused and recorded; open attention is cancelled and unused
  grants revoked.
- Attention: `attention.requested` creates the request and waits; a second one is refused; a
  decision is delivered, consumed only via `decision.consumed`, and resumes work; deny resumes
  with the action blocked; a stale-version decision conflicts.

## Boundaries

- No artifacts, result revisions or reviews (#26); no governed-action enforcement (#25).
- No production event transport; `Peer` comes from the caller as the transport's
  authentication result.
- The frozen #17 contracts and the 00001 to 00003 schema are not changed; entry payload
  schemas and migration `00004` are additive.
- #23's `Submit`/`Assert` change only by adding the Task and Attempt dispatch transitions to
  the enqueue (Decision C); its acceptance tests are updated for the new states.
