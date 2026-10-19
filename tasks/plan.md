# Implementation plan: issue #24 PR B — attention, decisions and cancellation

Tracker: [GitHub issue #24](https://github.com/AssahBismarkabah/dac-plane/issues/24) (epic #11). PR A (ingestion and
reconciliation) was merged at `baa31cc` with the give-up fix in `a6b9728`; this plan covers its second PR.
Investigation verified the Go surface against `main` at `baa31cc` and the explore sources re-checked 2026-10-18.

## Objective

Complete the attention half of the first vertical slice: a provider raises an attention request through the
ingestion boundary, an authorized responder records a decision through the command boundary, the decision is
delivered to the runtime over the adapter, its observed consumption resumes work, approving once issues the
scoped allow-once grant, and cancellation converges only after the runtime and environment stops are observed
or reconciled — ending with the stop confirmed and the reservation released exactly once.

Verification (from the issue):

```sh
just test-integration ./internal/application/ -run 'Event|Attention|Decision|Cancel|Reconcile'
just test-integration ./internal/worker/ -run 'Attention|Decision|Cancel|Reconcile'
just test-integration-race ./internal/application/ ./internal/worker/
```

The worker also names its decision- and cancellation-path tests with the shared prefixes so the issue's own
regexes cover them wherever they live.

## What exists (verified on main at `baa31cc`)

- Lifecycle records and machines are complete and frozen: `AttentionRequest` (states pending, decided,
  dispatching, delivered, consumed, expired, cancelled, superseded; `decide` is refused outside
  `DecideAttention`; redelivery reuses the same decision identity; cancel/supersede are legal from every
  unconsumed state), `Decision` (immutable; approve_once/deny/answer; answer requires a digest),
  `AllowOnceGrant` deriving `identity.AuthorityGrant` from a recorded approve-once bound to the request's
  operation, action digest and policy generation, and `identity.IssueGrant` refusing stale generations.
- Task machine: `await_input` (running -> waiting_input), `input_resolved` (waiting_input -> running),
  `input_unresolved` (waiting_input -> running/failed/cancelling), `cancel` (draft/queued direct;
  running/waiting_input/verifying -> cancelling), `cancelling` exits only on `stop_confirmed`.
- Attempt machine: `request_cancel` from active states -> `cancellation_requested` (directly `terminated`
  from `allocated`), `runtime_stopped`/`environment_stopped` converge `cancellation_requested` on both
  observations (refusing a repeated observation of the same stop at the same version), `reconciled` ->
  terminated, `request_cancel` from `reconciling` -> `cancellation_requested`.
- Store: `InsertAttention`/`ApplyAttention`/`DecideAttention` (replay rules, refusal audits, immutable
  decision trigger), `GetAttentionForUpdate`, grant store (`InsertGrant`/`RevokeGrant`/`GetGrantForUpdate`),
  `ReserveBudget`/`ReportUsage`/`ReleaseBudget` (release exactly once), attempt leases, `LockCursor`/
  `AdvanceCursor`, `RecordRefusal`, `GetPolicy`. Frozen DDL CHECK lists already allow outbox rows for
  `attention_request`; the decision and attention DDL carry the one-decision-per-version and
  digest-XOR constraints.
- Worker: claims provider dispatch and `attempt/request_cancel` rows; the cancel path calls `port.Cancel`,
  acks the row and leaves cancellation observation-driven; `steer_turn` and `interrupt` are frozen runtime
  methods; every request frame carries a per-send `delivery_id`; the reconciler applies `operation_outcome`
  only to uncertain operations; `abandonOperation` (a6b9728) resolves give-ups as rejected (never sent) or
  uncertain (possibly delivered).
- Application: `Submit`/`Assert` show the command-boundary pattern (envelope, fingerprint, ledger, refusal
  reuse, all-transition-one-commit, outbox payload in the transition); `Ingest` (PR A) classifies entries,
  binds, applies, and safe-defines cursor movement. `DecideAttention`/`CancelTask` do not exist; the command
  ledger deliberately has no command list ("new command kinds must not need a schema migration", `00003`).

## Sources

- Baseline ownership rules 1 and 4 and the first vertical slice (attention request -> decision -> resume):
  `docs/architecture/README.md` 59-65, 103-112.
- Review invariants: a state transition and its outbox entry commit atomically; delivery is at least once
  with stable ids and consumer dedup; leases are fenced ("a stale lease holder cannot mutate an Attempt or
  acknowledge a Decision"); cancellation is intent until both stops are observed or reconciled; governed
  authority with one-time retirement; audit digests only (`docs/architecture/review-2026-10-15.md` 47-71).
- ADR-0003: unknown send durability enters reconciliation; `AuthorityGrant` allow-once is consumed
  atomically at the execution transaction (enforcement #25; PR B only issues); cancel frames target an
  in-flight request identity; ceilings and policy generations fail closed.
- Gate #18: attention/decision schemas frozen with their test coverage
  (`TestDenialIsDeliveredLikeAnyDecision`, `TestFirstDecisionWins`,
  `TestApprovalConfersAllowOnceGrantUnderItsPolicy`); `TestCancellationOnlyEndsTerminated` and
  `TestReconciliationNeverReturnsToDispatch` must keep holding.
- SPEC (historical, shapes only): §11 one blocking request per attempt; denial blocks the action, not the
  task; §12 persist the request before display; §13 first valid decision wins, deliver through the runtime,
  "an answer is not proof that the runtime resumed", redelivery with the same decision id, supersede on a
  dead native process; §22 cancel persists intent before dispatch, invalidates pending authorizations and
  unused grants and never undoes effects; §25 "stop dispatching further work; terminate according to
  policy"; §32 a crash between decision and delivery re-delivers the same decision id on the committed
  outbox row.

## Alignment with `architecture-explore/` (re-checked 2026-10-18)

| # | Starting question | Evidence | Resolution |
|---|---|---|---|
| 1 | Wait state and events | Prototype `waiting_attention` / `wait` / `response_consumed` (SPEC §11, machines.ts) | Frozen vocabulary is `waiting_input` with `await_input`/`input_resolved`/`input_unresolved` — baseline wins |
| 2 | Deny ends the request as terminal `denied` | Prototype attention machine terminal `denied` (machines.ts 124) | Baseline changed this: a deny is an immutable Decision that is still delivered and observed consumed, so the runtime learns the denial (`attention.go` 81-85; `TestDenialIsDeliveredLikeAnyDecision`) |
| 3 | Delivery as a `DeliverDecision` outbox command type | Prototype outbox `type` column; SPEC §32 crash story | The frozen outbox rows are keyed by `record_kind` + `event`, and `steer_turn` is the frozen runtime method (`port.go` 25): the row is `attention_request`/`decide` produced by the decide transition, the per-send `delivery_id` stays per send |
| 4 | Where the grant's action class comes from | `AllowOnceGrant(decision, id, action, expires)` needs the class; `ActionClass` is an open validated vocabulary; the frozen request record has no class column | The approval `attention.requested` payload names the governed action; the correlation row records it (migration `00004`), and the decide command derives the grant from that row. Provenance chain: provider observation -> correlation -> decision -> grant; the responder never supplies the class |
| 5 | Expired or superseded cannot authorize; one accepted decision per request version | SPEC §10/§13; prototype unique (tenant, request, request_version) | Already frozen in the `decision` DDL and `Decide` CAS; the prototype's `stale_attempt` check needs nothing new because the attempt binding in the frozen request is the event-bound attempt |
| 6 | Queued cancel terminates without stops | ADR-0011(b)2: queued and ready-for-review cancel directly | Task `queued` has no runtime dispatch in flight (dispatch moves it to `running`), and `request_cancel` from `allocated` terminates directly in the frozen Attempt machine. No attention can be open on an `allocated` attempt (`await_input` is executing-only) |
| 7 | Mark superseded and "reconcile the proposed action" on a dead native process | SPEC §13:598-602 | The operation owns its own lifecycle (PR A: uncertainty, abandon); PR B supersedes open attention wherever the attempt's work turns non-live, and invents no second reconciliation |
| 8 | When does the reservation release | #15 acceptance: "Unused reservation is released exactly once at terminal completion"; SPEC §25:1151 | On Attempt terminal completion (completed/failed/terminated), whatever caused it — never coupled to the Task stop confirmation; late usage afterwards records overshoot (#15) |
| 9 | Which stop reason drives a cancel command | Prototype `StopReason` union (machines.ts:54-62): only `user_cancel` maps to the cancel event; `idle`/`token_limit`/`unknown` produce none by design | `CancelTask` carries the single command-driven reason `user_stop`; the observation reasons belong to turn-end handling, not the command boundary |
| 10 | What keys a consumed decision observation | SPEC §14:622-623 (the boundary supplies/verifies product identity), §13:598 ("redeliver to its mapped request"), prototype `0002_native_correlation.sql` | `decision.consumed` carries the native ref; the correlation row resolves it under the bound tenant, provider and attempt |

## Decision A: three new entry kinds (extends PR A's Decision A)

Exact kinds join the accept list ahead of the refuse prefixes; unknown `attention.*` and `decision.*` kinds
stay refused, so "an observation cannot grant permission by name" keeps holding. All three build their
identities from the bound event — the payload never names tenant, attempt, provider or operation.

| Entry kind | Payload | Effect |
|---|---|---|
| `attention.requested` | `native_ref`, `kind` (question / action_approval / authentication_handoff / blocked_work), for approvals `action` class + `action_digest`, `expires_at` | Product `AttentionRequest` created from the bound event: kind from the payload, digest copied for approvals, responders = the task owner principal, policy generation read from the tenant policy, `expires_at` must lie ahead of the event time. Transactionally claims the provider-native key before changing either lifecycle, then writes the request and correlation with Attempt `await_input` (from `executing`) and Task `await_input` (from `running`). A request whose state machine refuses the await transition or whose kind/payload combination fails validation is refused with evidence. A second unconsumed request on the Attempt (pending, decided, dispatching or delivered) is refused with evidence — one blocking request per attempt. |
| `decision.consumed` | `native_ref` of the request | Resolves the attention through the correlation row (the row must be bound to this event's tenant, provider and attempt, else refused). Attention `consume`; Task `input_resolved` (from `waiting_input`); Attempt `resume` (from `waiting_input`). A `dispatching` request may consume directly because the authenticated observation can race ahead of the send reply and proves delivery to the exact native request. Decided-but-never-dispatched, superseded, cancelled or consumed requests refuse with evidence. |
| `runtime.stopped` | required `had_active_run` | The runtime stop observation (Plane-Bot interrupt shape): Attempt `runtime_stopped` while `cancellation_requested`; only an already `terminated` Attempt can confirm a Task still left `cancelling`. Completed or failed work does not prove the environment stopped. |

`environment.observed` (existing kind) gains stop convergence: after binding and upserting the observation,
an attempt in `cancellation_requested` observing `absent` or `hibernated` applies Attempt
`environment_stopped`. Only an accepted latest observation can prove that stop; a stale or repeated resource
sequence is ingestion progress only. Stop success applies Task `stop_confirmed` and releases the reservation
(Decision D).

New payload types (`AttentionRequested`, `DecisionConsumed`, `RuntimeStopped`) get their
`adapter/entry-*.schema.json` files and the schema-conformance, enum and coverage guards — the same pattern
as PR A's five.

## Decision B: migration 00004, correlation row and new store helpers

`00004_attention_native.sql` (additive; `schema.go.CurrentVersion = 4`):

- `attention_native (tenant_id, provider_id, native_ref, attention_id, action_class)`; primary key
  `(tenant_id, provider_id, native_ref)` — one native ref maps to exactly one attention under one provider —
  plus `UNIQUE (tenant_id, attention_id)` — an attention has at most one native mapping; `action_class`
  stays nullable (questions carry none), and a NULL on an approval is refused at decide time, after the row
  exists.
- Store helpers: `LockAttentionNative` (transaction-scoped serialization of one provider-native key before
  lifecycle mutation), `InsertAttentionNative` (natural-key replay, mirroring `InsertBinding`),
  `GetAttentionNative` (by tenant + provider + native ref — a plain read: the correlation row is
  write-once provenance and never mutated), `ListOpenAttentionByAttempt` (states pending, decided,
  dispatching, delivered); `ListGrantsByAttempt` (open grants: revoked_at and retired_at null) for
  revocation; `GetOperationByAttempt` for the cancel body's operation id. `GetPolicy` already exists for
  the current policy generation.

## Decision C: DecideAttention and its delivery

`DecideAttention` embeds `Command` (actor, nonce, expected version) plus `Attention ids.AttentionID`, kind
(DecisionKind), answer digest for answers, at time. Fingerprint over `{attention, kind, answer_digest,
expected_version}`. One transaction:

1. Load the attention in the caller's tenant (`ErrNotFound` otherwise, matching the cross-tenant invariant),
   plus its correlation row (approvals need the recorded action class; its absence on an approval is a
   validation failure) and the tenant policy's current generation.
2. Build the `Decision` (principal = calling actor; request version = `Command.ExpectedVersion`; kind from
   the payload; digest only for answers) — first valid decision wins; wrong responder, kind/request-kind
   mismatch, stale policy, wrong request version and expiry refuse through the frozen machine and record
   their refusal audit; an identical recorded decision replays it.
3. `store.DecideAttention` — decision row, attention version to `decided`, history, all one commit.
4. For `approve_once`: `request.AllowOnceGrant(decision, grantID, correlationRow.ActionClass,
   attention.ExpiresAt)` — the grant expires exactly at the request's expiry, which the decide ceremony
   already required to lie ahead; `identity.IssueGrant` refuses a policy generation that moved on; then
   `store.InsertGrant`. Standing grants and deny-policy persistence are #25; nothing else is issued here.
5. Enqueue the delivery row in the same commit: record kind `attention_request`, record id the attention id,
   event `decide`, row id derived from the decision id (a replayed decide never duplicates the row), demand
   `person` (a decision delivery preempts background rows in the existing scheduler), body
   `{decision_id, attention, attempt, native_ref, kind, answer_digest?, action?, expires_at}`.

Worker delivery (`decisionRow`, claimed through an extended `dispatchableRows` predicate:
`or (record_kind = 'attention_request' and event = 'decide')`):

- Load the attention; a `decided` state moves it to `dispatching` (`AttentionDispatch`, CAS) — anything
  else (delivered, consumed, cancelled, superseded, expired) acks the row idempotently, so replay and
  duplicate work produce at-most-once sends.
- Take the attempt lease exactly like a dispatch does; a lost lease race releases the row.
- Send `contracts.Request{Operation: the attention's operation, Delivery: fresh delivery id, Tenant,
  Attempt, Role runtime, Method steer_turn, Arguments: body}`; a reply:
  - accepted -> `AttentionDeliver` (dispatching -> delivered) and ack the row, lease release, one commit;
  - rejected -> the delivery cannot reach the native request: `AttentionSupersede` + exhaust the row with a
    static reason — superseded becomes the only stop state the request reaches from a rejected steer; work
    stays as the attempt machinery keeps it (waiting_input still counts as live, matching prototype and
    SPEC — the wait's own eventual policy resolution is out of scope);
  - pending -> leave the row processing (lease lapse reclaims), like `SendPending`;
  - transport failure -> `retryPrepared`-style backoff; unknown durability never marks delivered; a spent
    retry budget exhausts the row and leaves the request `dispatching` — inspectable, but NOT replayable
    from the plane: the decide ledger replays the stored decide without re-running work, so no second row
    can appear. Resumption of a stuck `dispatching` request is #27 recovery (or an explicit real re-decide
    through the surface; the machine's `delivered -> cancel` path keeps it unreachable-minus-nothing).
    Never resume work by a silent assumption ("an answer is not proof that the runtime resumed").

Every reply must match both the stable operation and the current delivery identity. A delayed reply from an
earlier delivery is malformed and cannot acknowledge or resolve the current claim.

No expire driver ships in this PR; the request expiry only binds the decide ceremony (the frozen expiry
ceilings). Consumption follows only from the ingested `decision.consumed` entry.

## Decision D: CancelTask and stop convergence

`CancelTask` embeds `Command` (actor, nonce, expected Task version), `Task ids.TaskID`, the fixed
command-driven reason `user_stop` (resolution 2: the only cancel-mapping member of the prototype's
`StopReason` union), and `at`. Fingerprint over `{task, reason, expected_version}`. One transaction:

- Load the Task in the caller's tenant; unknown reads as absent (cross-tenant invariant).
- Require the calling principal to own the Task. Another tenant principal reads as absent; another principal
  in the same tenant receives an unauthorized-actor refusal before any lifecycle mutation.
- **Task `queued`** (governed pre-receipt, or back to queued from a review): `TaskCancel -> cancelled`.
  An allocated Attempt applies `request_cancel -> terminated` and releases its reservation in the same commit.
  A terminal Attempt from earlier reviewed work stays terminal; cancellation revokes any unused grants rather
  than rewriting completed or failed history.
- **Task `running` or `waiting_input`** (live dispatch or live native wait): `TaskCancel -> cancelling`;
  Attempt `request_cancel -> cancellation_requested`; enqueue the cancel outbox row
  (`attempt`/`request_cancel`, body `{operation: attempt's current dispatched operation via
  GetOperationByAttempt, role runtime, method "interrupt", deadline now+config horizon,
  cancel_reason}` — refusing when no operation exists, since that is a ledger-level producer error);
  attention `cancel` on every unconsumed open request of the Attempt ("cancelling invalidates pending
  authorizations and native waits"); grant `RevokeGrant` on every open grant of the Attempt ("unused
  grants are revoked").
- The dispatch checklist reads the Attempt after claiming any `start_turn` row. If cancellation is already
  committed, it exhausts that start instead of sending or retrying it; an earlier transport failure leaves
  the operation uncertain, but cannot cause work to start after the cancellation interrupt.
- **Task already `cancelling`**: a same-nonce replay returns the stored result through the idempotency
  ledger; a different nonce runs the work and refuses on the version CAS (the Task already left `queued`/
  `running`/`waiting_input`), recording the refusal audit.

Convergence (the follow rules; the frozen machines decide order, the boundary only applies and audits):

- New follow rule in `lifecycle.TaskFollowsAttempt`: Attempt `terminated` AND Task `cancelling` ->
  `stop_confirmed`. Completed or failed Attempts do not prove environment shutdown. Existing rules stay:
  failed + running -> `execution_failed`;
  uncertain + live -> `reconciling`; nothing else changes, so `TestReconciliationNeverReturnsToDispatch`
  and `TestCancellationOnlyEndsTerminated` keep holding.
- Release is not cancellation-coupled: #15's acceptance criterion — "Unused reservation is released exactly
  once at terminal completion" — puts the release on ATTEMPT terminal completion (completed, failed,
  terminated), whatever caused it. A shared `releaseIfTerminal(ctx, uow, tenant, attempt, tr)` helper runs
  after every outcome that lands the Attempt in a terminal state inside the caller's commit (stop
  convergence, reconcile resolution, failed turn, give-up abandon), keyed by the reservation id derived
  from the attempt id — the same derivation `checkBudget` already uses — through `GetReservation` +
  `ReleaseBudget`, which replays as already-released. Late usage arriving after a release records overshoot
  (#15). A queued-cancel releases in its own commit (the attempt terminated directly).
- A `neverSent` give-up (`abandonOperation`) on a `cancellation_requested` attempt applies the runtime stop,
  and an Attempt with no environment binding applies the environment stop as well — it terminates without
  inventing a provider ("a dispatch that was never delivered reconciles the runtime stop"); a possibly-
  delivered give-up stays uncertain and keeps waiting for real observations. The same terminal-release
  helper closes the reservation in that commit.
- `environmentObserved`, `runtimeStopped` and `turnFinished interrupted` share the same stop helper; the
  machines alone decide ordering (`RejectStopAlreadyObserved` surfaces as a refusal audit for a stale
  duplicate after the state moved, and as a replay for a same-version duplicate). Non-stop observations
  (`unknown`, `starting`, `running`) during cancellation stay progress-only — only `absent` and
  `hibernated` are definite stops.

Race policy for every attention/grant mutation outside a single CAS (cancel command touching many requests,
supersede helper, revoke loop): apply per record with the record's current version after a fresh read; on
`ErrStaleVersion` re-read and re-apply — the machine keeps `cancel`/`supersede` legal from every unconsumed
state, including `delivered` — and when the state meanwhile became `consumed` the wait resolved
legitimately, so writing nothing is correct. A racing worker bump never blocks or rolls back the whole
cancel command.

## Decision E: non-live work supersedes its wait

One helper `supersedeOpenAttention(ctx, uow, tenant, attempt, actor, at)`: apply `AttentionSupersede` on
every unconsumed open request of the Attempt in the caller's transaction, ignoring (already-superseded,
cancelled, consumed) states through the machine's own replay and refusal handling. Called:

- from `resolveSend` when a reply lands `SendUnknown` (the plane can no longer know the wait is real);
- from `abandonOperation` when the resolved target is `uncertain` (the give-up mirror);
- from the turn-finished failure path when `AttemptFail` applies (a failed run cannot serve its wait);
- never from the reconcile path itself (by then the wait already superseded, or the machine refuses).

A `decision.consumed` against a superseded request is refusal evidence, never resurrection.

## Acceptance tests

In `internal/application` unless delivery requires the worker fixture; names match
`-run 'Event|Attention|Decision|Cancel|Reconcile'` in both packages.

- **Attention requests**: `TestAttentionRequestedCreatesRequestAndWaits` (pending request + correlation row,
  Attempt/Task `await_input`); `TestAttentionRequestedSecondBlockingRefused`;
  `TestAttentionRequestedNativeReferenceRaceHasOneReachableWait` (one correlation, one wait, no orphan);
  kind payloads: an approval
  requires class + digest; a non-approval carrying one is refused; `TestAttentionRequestedExpiredRefused`
  (`expires_at` behind the event time).
- **Decisions**: `TestDecisionDenyRecordsAndDelivers` (decision row, delivery row, steer accepted, request
  delivered; then ingest `decision.consumed` -> request consumed, Task `input_resolved`, Attempt `resume`);
  `TestDecisionAnswerCarriesDigest`; `TestDecisionApproveOnceIssuesAllowOnceGrant` (fields, generation, same
  row on replay); `TestDecisionStaleVersionConflicts`; `TestDecisionWrongResponderForbidden`;
  `TestDecisionReplayReturnsRecordedDecision`; `TestDecisionStalePolicyForbidden`.
- **Delivery**: `TestDeliveryRedeliversAfterCrashBetweenDecideAndSend` (row survives restart and re-claims;
  SPEC §32); `TestDeliveryRejectedSteerSupersedesRequest`; decided-before-dispatch consumption is refused,
  while dispatching consumption proves delivery even when it races ahead of the reply; a reply for another
  delivery is rejected; duplicate arrow-worker delivery does not double-send (one steer per decision in
  `Sent()`).
- **Cancellation**: `TestCancelQueuedTaskCancelsAndReleases`; `TestCancelDispatchedTaskWaitsForStops`
  (cancelling + `cancellation_requested` + cancel row + attention cancelled + grants revoked;
  `TestWorkerCancelIndependentOfStop` keeps asserting the protocol cancel at the worker layer);
  owner-only authorization and ready-for-review direct cancellation; `TestEventEnvironmentStopConvergesCancellation`;
  `TestEventRuntimeStopCompletesCancellation` (both stops -> terminated -> Task `stop_confirmed` -> reservation
  released once, with nonce replay returning the original command result); stale environment observations
  cannot prove a stop; `TestNormalCompletionDoesNotConfirmCancellation`; `TestEventFailedTurnAfterCancelRefuses`
  (late observation refused and recorded); `TestAbandonReconcilesNeverSentCancel` (asserts the released
  reservation alongside the termination); `TestWorkerDoesNotRetryStartAfterCancellation` (a backed-off
  start is exhausted after the interrupt and never reaches the provider again).
- **Reservation release**: `TestReservationReleasesAtAttemptTerminal` (a failed turn and a
  never-sent give-up both release exactly once; a duplicate release replays as already-released; late
  usage after release records overshoot per #15 — the completed path's production arrives with #26).
- **Uncertainty hooks**: `TestReconcileUnknownOutcomeSupersedesAttention`.
- **Replay/dedupe acceptance**: repeated frames repeat no transition; the correlation natural key, the
  decision-id row and the CAS keep concurrent duplicates convergent (end-to-end duplicate decide command
  through the ledger).

## Verification

```sh
just test-contracts            # three new schemas must compile and be covered, vendor test stays clean
just test-integration ./internal/application/ ./internal/worker/
just ci                        # before opening the PR
```

PR: `feat(application): deliver decisions and converge cancellation`, filled with the adorsys-gis template,
mutation-verified per the repo habit, review-bot findings checked against the mechanism before fixing or
rebutting.

## Boundaries

- No #25 enforcement (dispatch-time grant revalidation, hook interception), no attention expiry driver, no
  standing-grant issuance, no attention projection/UI read model (#26 stacks those), no `input_unresolved`
  producer (parked with the wait policy in #25 — the machine slot stays unused by design here).
- No production transport for decisions; the simulator scripts the delivery replies in a `RowsFrame`-style
  flow only where needed, and the worker tests use the existing loopback script surface.
- Frozen contracts (#17/#18), schemas 00001-00003 and the established PR A behavior stay untouched; the PR
  adds migration 00004, three entry schemas, and composition logic only. No new ADR.

## Resolutions (settled from the sources, 2026-10-19)

None of the four alignment questions stayed open — each resolves in `architecture-explore/` or the frozen
baseline, and the answers amended the decisions above:

1. **`decision.consumed` carries the native ref** — SPEC §14:622-623 makes the authenticated boundary the
   supplier of product identity ("it does not trust these fields merely because the worker sent them"), §13:598
   redelivers "to its mapped request", and prototype `0002_native_correlation.sql` made the native id the
   correlation key in both directions. The correlation row resolves it under the bound tenant, provider and
   attempt; no product id travels in that payload. (Decision A as written.)
2. **Cancel reason: one command-driven value.** Prototype `StopReason` is a closed union
   (machines.ts:54-62) in which only `user_cancel` maps to the cancel event — `idle`, `token_limit` and
   `unknown` deliberately produce none ("stays running; needs follow-up or user decision"). The command
   `CancelTask` carries the single fixed reason `user_stop`; `admin_stop` and the runtime-observation
   reasons are not command vocabulary. The adapter body's free-text `cancel_reason` stays unchanged
   (native interrupts carry a reason string, DEEP-DIVES-04:47-49).
3. **`input_unresolved` stays unproduced — by evidence, not neglect.** Its producer is request-policy or
   expiry logic: task.go:89-91 ("An unresolved one resumes, fails or cancels according to the request's
   policy"), SPEC §11:490-491, machines.ts:26. M1 PR B has neither a request-policy surface nor an expiry
   driver (boundaries: #25, #27), and denial in the frozen baseline resolves via `input_resolved`. Consequence
   owned: a request superseded on non-live work leaves its Task in `waiting_input` until the user cancels or
   the wait returns through a fresh attempt — the PR body will state this.
4. **Release rides attempt-terminal completion, not Task-stop confirmation.** #15's criterion — "Unused
   reservation is released exactly once at terminal completion" — settles the timing on the attempt's
   terminal state, whatever caused it, with late usage recording overshoot afterwards (Decision D as
   amended). This supersedes the earlier task-confirmation stance and slightly widens PR B: the
   terminal-release helper also fires on ordinary attempt failure and on give-up resolution. Reviewer note:
   without it, every normally failed attempt leaks its reservation, and #28's invariant sweep would have
   flagged the leak later regardless.
