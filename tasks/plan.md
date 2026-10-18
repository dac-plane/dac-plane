# Implementation plan: issue #23 authenticated submission and dispatch

Tracker: [GitHub issue #23](https://github.com/AssahBismarkabah/dac-plane/issues/23) (epic #11)

## Objective

Implement the transport-neutral command boundary: an authenticated command that
carries tenant, principal, idempotency key and expected version where
applicable, submits one Task through Budget reservation, Attempt creation and
provider-operation dispatch into the transactional outbox, atomically with its
audit evidence. Depends on #15, #19, #20, #21 and #22 (all merged). The issue's
verification is
`go test -tags=integration ./internal/application/... -run 'Submit|Dispatch'`
and the `-race` variant.

Acceptance criteria beyond the four base ones (from the live issue body):

- Reusing a client nonce with the same payload returns the original submission;
  changing the payload is rejected.
- Dispatch records person/background demand and the selected harness mode
  without selecting a production provider.
- Spend-governed submission cannot enqueue dispatch without both budget
  reservation and a valid receipt.
- Submission persists the identities needed to correlate later session, turn
  and environment bindings.

## Sources

- `docs/architecture/review-2026-10-15.md` §46: "Every mutating surface command
  is authenticated, authorized, idempotent and version checked"; §47-48: a
  state transition and its outbox entry commit atomically, delivery is at least
  once and consumers dedupe; §74-75: spend challenges are replay protected and
  produce durable receipts without replacing Budget reservation.
- `docs/architecture/README.md` §99-124: the initial implementation boundary
  (`submit task -> create and dispatch attempt -> start one runtime ...`).
- `docs/adr/0002-initial-application-boundary.md`: one application with a
  surface/auth module and transactional command/outbox persistence; internal
  package boundaries prevent bypassing lifecycle commands.
- `docs/adr/0003-provider-contract-semantics.md`: demand classes are scheduling
  hints only; harness modes `off`/`shadow`/`live`; spend authorization is a
  separate two-phase ceremony whose receipt reserves and consumes nothing;
  #23-#26 must exercise contracts only through application boundaries (§177).
- `docs/architecture/contract-gate-18.md`: the frozen budget/spend/execution
  contract families this boundary composes.
- Existing code the boundary drives, unchanged: `internal/platform/postgres`
  (`Begin`/`UnitOfWork`, `InsertTask`/`ApplyTask`, `InsertAttempt`,
  `InsertBudget`/`ReserveBudget`, `InsertOperation`/`ApplyOperation` with
  `OutboxPayload`, `BeginSpend`/`InsertSpendChallenge`/`AssertSpendChallenge`/
  `GetSpendReceipt`, `GetHarness`, `appendRefusal`), `internal/worker`
  (`Tick`, `DispatchBody` producer contract, reservation identity derived from
  the Attempt), `internal/simulator` (runtime port and spend `Spender`),
  `internal/identity` (`AuthorizationContext`).
- `architecture-explore/` is evidence only (Plane-Bot `clientNonce` command
  vocabulary; SPEC §12 "Task + submitted event + dispatch command"). The
  prototype's `idempotency_key(tenant_id, operation, key, request_hash,
  response jsonb)` table is the shape precedent, not a requirement.

## Design: the command boundary

New package `internal/application`, transport-neutral, no HTTP:

- `application.go` — the shared envelope and the idempotency machinery.

  ```go
  type Command struct {
      Actor           identity.AuthorizationContext
      Nonce           string // client idempotency key, 1..128 chars
      ExpectedVersion uint64 // where applicable; 0 when the command creates
  }
  ```

  Errors: `ErrInvalidCommand` (boundary validation) and `ErrNoHarness` (no
  qualified harness for the selected provider); domain refusals stay
  `*lifecycle.TransitionError` and store errors pass through unchanged.

- Every command runs one transaction:
  1. Validate the envelope and structural inputs (ids, times, unit, demand,
     amounts); domain rules stay in the domain.
  2. Read the idempotency record by `(tenant, principal, command, nonce)`.
     Found with an equal payload digest: load and return the original
     submission. Found with a different digest: `ErrPayloadMismatch`.
  3. Insert a placeholder record for the nonce inside the transaction. A
     conflict means a concurrent duplicate already committed: roll back, re-read
     and replay it. The `(tenant, principal, command, nonce)` key serializes
     racers before any work exists.
  4. Run the work: domain transitions via the store. A domain refusal rolls the
     whole command back (nothing partial ever commits) and persists the refusal
     audit alone through the new store helper `RecordRefusal`; retries of a
     refused command deterministically re-refuse. Other errors roll back.
  5. Fill the record's result facts and commit. Committed records are the
     "original submission": ids, demand, deadline, unit, amount, provider and
     the harness mode selected at submission.
- Read surface for callers (and the acceptance tests, which may not touch the
  store): `GetSubmission(ctx, pool, actor, nonce)` loads a recorded submission
  plus the current state of its rows and its dispatch.

## Design: submit_task

```go
type SubmitTask struct {
    Command
    At       time.Time          // command time
    Provider ids.ProviderID     // the selected harness (simulator in tests)
    Demand   contracts.Demand   // person|background, "" defaults to background
    Deadline time.Time          // bounds the dispatch; requires Deadline > At
    Unit     lifecycle.Unit     // budget unit
    Ceiling  uint64             // budget ceiling
    Amount   uint64             // reservation for this attempt
    Spend    bool               // spend-governed when true
}

func SubmitTask(ctx context.Context, pool *pgxpool.Pool, cmd SubmitTask) (Submission, error)
```

Work, in one transaction, in order:

1. Load the harness qualification for `Provider`; refuse with `ErrNoHarness`
   when it is absent or `off`. The submission selects a qualified harness;
   later demotion is the dispatch loop's business.
2. `InsertTask` draft, then `ApplyTask(TaskSubmit -> queued)` — the task machine,
   audit and durable event commit with the rest.
3. `InsertAttempt` number 1, `allocated`.
4. `InsertBudget`, then `ReserveBudget` with the reservation identity derived
   from the attempt (mirroring `internal/worker/dispatch.go`), before anything
   dispatchable exists. A ceiling refusal fails the command here; no dispatch
   row can precede a held reservation.
5. `InsertOperation` against `Provider`, state `recorded`.
6. Ungoverned: build the dispatch body
   `{"operation","role":"runtime","method":"start_turn","deadline","unit","amount"}`
   and `ApplyOperation(OperationDispatch -> dispatched)` with an
   `OutboxPayload{ID, Attempt, Demand, Body}` — the canonical producer form the
   #21 persistence tests fix. Spend-governed: `InsertSpendChallenge` bound to
   tenant, principal, task, attempt, operation, amount, unit and a nonce
   derived from the client nonce, expiring at `Deadline`, and **no outbox row
   yet** — the submission cannot enqueue before a receipt exists.

`Submission` carries the loaded records (`Task`, `Attempt`, `Operation`,
`Budget`, `Reservation`, `Challenge`, `Receipt`), the bound selection
(`Provider`, `Mode`), `Demand`, `Deadline`, and `Dispatch` (outbox id, status,
demand, attempts, last error, exhausted-at) once enqueued.

## Design: assert_spend (the governed second phase)

```go
type AssertSpend struct {
    Command                       // ExpectedVersion = the challenge's version
    SubmissionNonce string        // the governed submission being completed
    Assertion lifecycle.SpendAssertion // challenge id, terms, proof, asserted_at
}

func AssertSpend(ctx context.Context, pool *pgxpool.Pool, cmd AssertSpend) (Submission, error)
```

The spend ceremony is two-phase for real: the challenge binds ids that only
exist after submission, so the principal's assertion can only arrive in a
second authenticated command (the Plane-Bot begin/finish shape, exercised
through the application boundary as ADR-0003 requires).

Work, in one transaction:

1. Load the submission record for `SubmissionNonce` (same tenant and
   principal); refuse `ErrInvalidCommand` when it is not spend-governed, and
   cross-check that the assertion's terms name that submission's operation.
2. `AssertSpendChallenge` — mismatch, expiry, terminal state and version
   conflicts refuse here with audit evidence and without a receipt.
3. Verify the attempt's reservation is still held and matches the asserted
   amount. Receipt and reservation are both required before enqueue.
4. `ApplyOperation(OperationDispatch -> dispatched)` with the dispatch body and
   an `OutboxPayload` carrying the **submission's** demand: the outbox row and
   the receipt commit together. On the store's idempotent replay (operation
   already dispatched) no outbox row is written and `Dispatch` stays empty.
5. Record this command's own result. A replayed command with the same nonce and
   payload returns the stored result; an assertion replayed against an
   already-asserted challenge converges through the store's replay rules.

## Design: persistence (migration 00003)

`internal/platform/migrations/migrations/00003_command_record.sql`, additive:

```sql
CREATE TABLE command_record (
  tenant_id      uuid NOT NULL REFERENCES tenant (id),
  principal_id   uuid NOT NULL,
  command        text NOT NULL CHECK (command <> ''),
  nonce          text NOT NULL CHECK (nonce <> '' AND char_length(nonce) <= 128),
  request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
  result         jsonb NOT NULL CHECK (jsonb_typeof(result) = 'object'),
  created_at     timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, principal_id, command, nonce),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES principal (tenant_id, principal_id)
);
```

The nonce is scoped to its tenant, principal and command kind, so no principal
can replay or observe another's submission. `result` is inserted as a
placeholder and filled before commit; no committed row is ever updated (no
mutation path exists in code). No `command` CHECK list: new commands must not
force a schema migration.

Store additions, keeping every SQL statement and pg error mapping in
`internal/platform/postgres`:

- `commands.go`: `CommandRecord` plus `InsertCommandRecord` (conflict-safe),
  `SetCommandResult`, `GetCommandRecord`.
- `history.go` or a neighbor: exported `RecordRefusal(ctx, q, audit)` wrapping
  the existing refusal-audit append, for refusals of multi-write commands that
  must roll back.
- `resume.go`/`outbox.go`: `GetOutboxRow(ctx, q, tenant, id)` plus `Status` and
  `ExhaustedAt` on `OutboxRow` and in `selectOutboxRow`/`scanOutboxRow`, so the
  boundary's dispatch view can report state without a second claim path.

No changes to `internal/worker`, `internal/bootstrap`, the lifecycle machines,
the identity contracts or the frozen schema.

## Ordered tasks

1. Rewrite this plan (done, committed separately).
2. Migration `00003_command_record.sql` + a focused schema integration test
   (PK scoping, FK, digest and nonce CHECKs) in
   `internal/platform/migrations/schema_integration_test.go`.
3. Store additions (`GetOutboxRow`/`OutboxRow` fields, `RecordRefusal`,
   `commands.go`) with store-level coverage where the acceptance tests cannot
   observe them (refusal-audit persistence).
4. `internal/application` core: envelope, validation, idempotency flow,
   `GetSubmission`, `SubmitTask`, `AssertSpend`; pure unit tests for envelope
   validation and payload digest canonicalization (UTC times, defaulted demand,
   command time excluded).
5. Acceptance tests in `internal/application` (build-tagged integration), named
   so the issue's `-run 'Submit|Dispatch'` selects all of them; every mutation
   and observation goes through the command boundary, the worker `Tick`, or the
   simulator ledger. Only tenant/principal/harness preconditions are seeded
   with the store fixture pattern (there is no M1 command surface for
   identities or qualifications), and no lifecycle record is written by a test.
6. Full gate, PR `feat(application): implement authenticated submission and
   dispatch`, review-bot findings verified against the code, squash-merge on
   green.

## Acceptance tests

- `TestSubmitTaskDispatchesTurnThroughSelectedHarness`: live simulator harness,
  person demand; assert task queued, attempt allocated, budget reserved,
  reservation held, operation dispatched, dispatch pending with demand and the
  bound mode, then `Tick` sends exactly one runtime `start_turn` request for
  that operation, operation accepted, dispatch done.
- `TestSubmitTaskReplayReturnsOriginalSubmission`: same nonce and payload
  returns the same task/attempt/operation; a different nonce creates a distinct
  submission; replay after dispatch returns the same ids.
- `TestSubmitTaskChangedPayloadRejected`: same nonce, different amount is
  `ErrPayloadMismatch` and the original is untouched.
- `TestSubmitTaskRejectsInvalidCommands`: zero actor, empty/over-long nonce,
  expected version set on a creating command, deadline not after command time,
  zero amount, malformed unit, unknown demand, unknown provider (missing and
  `off` qualification -> `ErrNoHarness`).
- `TestSubmitTaskCeilingRefusalDoesNotDispatch`: amount over ceiling is a
  `ceiling_reached` refusal; nothing is recorded (submission not found) and a
  `Tick` sends nothing.
- `TestSubmitTaskPersistsCorrelationIdentities`: the operation names the
  attempt and the selected provider, and the submission view re-reads them —
  the ids later session, turn and environment bindings must reference.
- `TestDispatchShadowHarnessWithholdsSend`: shadow harness dispatches nothing,
  consumes the row and leaves the operation undispatched.
- `TestDispatchHarnessDemotionExhausts`: submission under a live harness, then
  demotion to `off` before `Tick` exhausts the row with
  `harness_not_live` and sends nothing.
- `TestSubmitSpendGovernedAwaitsReceiptThenDispatches`: governed submission
  records the pending challenge, no dispatch, reservation held; `Tick` sends
  nothing; the simulator `Spender` asserts through `AssertSpend`; receipt and
  dispatch row (with the submission's demand) commit together; `Tick` sends.
- `TestSubmitSpendGovernedRejectsMismatchedAssertion`: drifted terms refuse
  with `assertion_mismatch`, leave the challenge pending, write no receipt and
  no dispatch.
- `TestSubmitSpendGovernedRejectsExpiredAssertion`: an assertion at or past
  expiry refuses with `request_expired`.
- `TestSubmitSpendAssertsRejectStaleVersion`: a wrong expected version refuses
  with `version_conflict` and changes nothing.

## Verification

```sh
go test -tags=integration ./internal/application/... -run 'Submit|Dispatch'
go test -tags=integration -race ./internal/application/... -run 'Submit|Dispatch'
go test -tags=integration ./internal/platform/postgres/... ./internal/platform/migrations/...
just test-contracts
just ci
```

## Boundaries

- No HTTP endpoints, no transport types, no wire schemas; commands are Go
  values, the boundary stays transport-neutral.
- No provider event processing, no reconciliation (#24), no attention/decision
  commands, no cancellation commands.
- No direct repository access from the acceptance tests for any lifecycle
  fact; identity and harness qualification preconditions are fixture seeding.
- No changes to the worker's dispatch semantics, the lifecycle machines, the
  frozen `00001`/`00002` schema, `contracts/jsonschema` or the guard lists.
- No baseline, diagram or accepted-ADR edits; `architecture-explore/` stays
  read-only. No new ADR: ADR-0002 and ADR-0003 already fix this boundary.

## Investigation check (2026-10-18)

The plan above was re-verified against `main` at `3b64ea5` (after #22 merged):

- Issue #23's live body matches the scope, out-of-scope list, acceptance criteria and
  verification commands above.
- Every existing store, worker and simulator function the design names exists with the
  assumed signature (`Begin`, `InsertTask`, `ApplyTask`, `InsertAttempt`, `InsertBudget`,
  `ReserveBudget(ReservationRequest, Transition)`, `InsertOperation`, `ApplyOperation`,
  `OutboxPayload`, `InsertSpendChallenge`, `AssertSpendChallenge`, `GetSpendReceipt`,
  `GetHarness`, `appendRefusal`, `OutboxRow`, `selectOutboxRow`, `scanOutboxRow`,
  `worker.DispatchBody`, `Worker.Tick`, `simulator.Spender`). `GetOutboxRow`,
  `RecordRefusal` and `commands.go` are new; migration number `00003` is free.
- The producer form is forced, not chosen: the worker requires the outbox row's record id to
  equal `DispatchBody.Operation` (`internal/worker/dispatch.go:57`), and the only provider
  operation transition from `recorded` is `dispatch`, so the outbox row must ride
  `ApplyOperation(dispatch)`. That is the form the #21 worker tests use.
- Reservation identity agrees with the worker: `checkBudget` derives the reservation id from
  the Attempt id and, when that reservation already exists, only checks the amount
  (`dispatch.go:371-420`). Submission reserving first therefore makes the worker's gate a
  replay, never a second hold.
- Concurrent duplicates: PostgreSQL blocks an insert on a unique index while another
  transaction holds the same key, and `ON CONFLICT DO NOTHING` then skips it once that commits
  (PostgreSQL 18 `INSERT` docs, index uniqueness checks). The store's `insertReturning`
  already uses this, so `InsertCommandRecord` follows it and needs no in-progress state.
- Idempotency semantics follow the IETF `Idempotency-Key` draft
  (draft-ietf-httpapi-idempotency-key-header): a request fingerprint (checksum of the
  payload) is stored with the key, and reusing a key with a different payload is refused
  (HTTP 422 there, `ErrPayloadMismatch` here). The draft requires the expiry rule to be
  documented; see below.
- Exploration evidence holds: Plane-Bot `clientNonce` (DEEP-DIVES-01, MASTER-RECORD),
  historical SPEC "unique idempotency key per tenant and API operation" and
  "Require Idempotency-Key", and the prototype's `idempotency_key` table in
  `architecture-explore/services/product-tasks`.

## Open alignment points

- Spend-governed flow is two commands (submit opens the challenge and holds the
  reservation without enqueueing; assert completes the receipt and enqueues in
  the same transaction). This is the only reading that satisfies both the
  receipt-before-enqueue rule and the challenge's id binding.
- The selected harness mode is bound into the idempotency record and the
  submission view; it is deliberately not copied into the dispatch body, since
  the worker resolves the mode from the qualification at dispatch time and a
  body copy could go stale.
- A refused multi-write command persists its refusal audit through a separate
  single-statement `RecordRefusal`; the transaction itself rolls back so no
  partial submission ever commits.
- Command records do not expire in M1: a nonce stays bound to its submission for the life of
  the tenant data, which the IETF draft permits as long as it is documented. Expiry and pruning
  belong to retention work after M1.
- An outbox row the worker exhausts (for example after a harness demotion) leaves its
  operation `dispatched` with no delivery, because the producer form marks the operation
  dispatched at enqueue. That is existing #21 semantics, surfaced as an exhausted row; turning
  it into an explicit Attempt outcome is reconciliation work for #24, not this issue.

## Implementation notes

Refinements made while implementing, each kept within the design above:

- Go cannot name a type and a function alike, so the command types keep the plan's names
  (`SubmitTask`, `AssertSpend`) and the entry points are `Submit` and `Assert`.
- The dispatch row's outbox id is the operation id: one operation has exactly one dispatch,
  so a retried enqueue reuses it, and `GetSubmission` finds the dispatch whether `Submit` or
  `Assert` enqueued it without consulting the second command's record.
- Authorization loads the principal inside the command transaction and requires the stored
  principal (tenant, id, kind) to equal the authenticated one; otherwise `ErrUnauthorized`.
- An assertion replayed after its dispatch was enqueued converges: `Assert` enqueues only
  while the operation is still `recorded`.
- `TestDispatchShadowHarnessWithholdsSend` asserts no send and a consumed row; the operation
  remains `dispatched`, the producer-form consequence recorded under the alignment points.
- Beyond the plan's list: `TestSubmitTaskConcurrentDuplicatesConverge` (racing duplicates
  produce one task and one send), and two evidence tests outside the `Submit|Dispatch`
  acceptance filter that read the store directly: refusal audit survives the rollback, and a
  receipt without a held reservation cannot enqueue.

