//go:build integration

package worker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// errTransportForTest is the scripted transport loss the loopback replays.
var errTransportForTest = errors.New("transport unavailable for test")

// fixture is one tenant with a provider and a worker wired to a loopback
// port. Helpers commit their own transactions so tests read like scripts:
// arrange fixtures, Tick or Reconcile or ReapClaims, assert durable state
// and loopback traffic.
type fixture struct {
	tenant    ids.TenantID
	principal ids.PrincipalID
	provider  ids.ProviderID
	pool      *pgxpool.Pool
	loopback  *worker.LoopbackPort
	actor     lifecycle.Actor
}

func setupPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func setupFixture(t *testing.T) (context.Context, *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool := setupPool(t, ctx)
	f := &fixture{pool: pool, loopback: worker.NewLoopbackPort(contracts.RoleRuntime)}
	f.tenant = mustID[ids.Tenant](t)
	f.principal = mustID[ids.Principal](t)
	f.provider = mustID[ids.Provider](t)
	f.actor = lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal}
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if err := store.InsertTenant(ctx, uow, f.tenant); err != nil {
		t.Fatalf("InsertTenant() error = %v", err)
	}
	if err := store.InsertPrincipal(ctx, uow, identity.Principal{Tenant: f.tenant, ID: f.principal, Kind: identity.PrincipalHuman}); err != nil {
		t.Fatalf("InsertPrincipal() error = %v", err)
	}
	if err := store.InsertPolicy(ctx, uow, identity.Policy{
		Tenant: f.tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
		Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
	}); err != nil {
		t.Fatalf("InsertPolicy() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	return ctx, f
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func testWorker(t *testing.T, f *fixture, cfg worker.Config) *worker.Worker {
	return testWorkerWithPort(t, f, cfg, f.loopback)
}

func testWorkerWithPort(t *testing.T, f *fixture, cfg worker.Config, port contracts.Port) *worker.Worker {
	t.Helper()
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		map[ids.ProviderID]contracts.Port{f.provider: port}, cfg, "test-worker")
	if err != nil {
		t.Fatalf("worker.New() error = %v", err)
	}
	return w
}

// dispatchableOp records an operation and dispatches it, leaving the ready
// outbox row the worker consumes. The harness starts live; tests that need
// another mode upsert it afterwards.
func (f *fixture) dispatchableOp(t *testing.T, ctx context.Context, at time.Time, demand contracts.Demand, amount uint64) (ids.OperationID, ids.AttemptID) {
	t.Helper()
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	task, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, at)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	if _, replayed, err := store.InsertTask(ctx, uow, task); err != nil || replayed {
		t.Fatalf("InsertTask() = %v, %v", replayed, err)
	}
	attempt, err := lifecycle.NewAttempt(mustID[ids.Attempt](t), f.tenant, task.ID, 1, at)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	if _, replayed, err := store.InsertAttempt(ctx, uow, attempt); err != nil || replayed {
		t.Fatalf("InsertAttempt() = %v, %v", replayed, err)
	}
	opID := mustID[ids.Operation](t)
	operation, err := lifecycle.NewProviderOperation(opID, f.tenant, attempt.ID, f.provider, at)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, replayed, err := store.InsertOperation(ctx, uow, operation); err != nil || replayed {
		t.Fatalf("InsertOperation() = %v, %v", replayed, err)
	}
	outboxID, err := ids.New[ids.Any]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"operation": opID.String(), "role": "runtime", "method": "start_turn",
		"deadline": at.Add(time.Hour).UTC().Format(time.RFC3339),
		"unit":     "tokens", "amount": amount,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	tr := store.Transition{
		ExpectedVersion: 1, At: at, Actor: f.actor, AuditID: mustID[ids.Audit](t),
		Outbox: &store.OutboxPayload{ID: outboxID, Attempt: attempt.ID, Demand: demand, Body: body},
	}
	if _, replayed, err := store.ApplyOperation(ctx, uow, f.tenant, opID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr); err != nil || replayed {
		t.Fatalf("ApplyOperation() = %v, %v", replayed, err)
	}
	if _, err := store.UpsertHarness(ctx, uow, lifecycle.HarnessQualification{
		Tenant: f.tenant, Provider: f.provider, Mode: lifecycle.HarnessLive, Version: 1, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("UpsertHarness() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	return opID, attempt.ID
}

func (f *fixture) operationState(t *testing.T, ctx context.Context, op ids.OperationID) lifecycle.ProviderOperation {
	t.Helper()
	operation, err := store.GetOperation(ctx, f.pool, f.tenant, op)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	return operation
}

func (f *fixture) attemptState(t *testing.T, ctx context.Context, op ids.OperationID) lifecycle.Attempt {
	t.Helper()
	attempt, err := store.GetAttempt(ctx, f.pool, f.tenant, f.operationState(t, ctx, op).Attempt)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	return attempt
}

func (f *fixture) taskState(t *testing.T, ctx context.Context, attempt lifecycle.Attempt) lifecycle.Task {
	t.Helper()
	task, err := store.GetTask(ctx, f.pool, f.tenant, attempt.Task)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	return task
}

// moveDispatched applies the enqueue follow Submit performs when it enqueues
// a dispatch: the Task runs and the Attempt provisions. Tests that assert
// what giving up on a dispatch leaves behind start from those states.
func (f *fixture) moveDispatched(t *testing.T, ctx context.Context, op ids.OperationID, at time.Time) {
	t.Helper()
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	operation, err := store.GetOperation(ctx, uow, f.tenant, op)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	attempt, err := store.GetAttempt(ctx, uow, f.tenant, operation.Attempt)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	tr := store.Transition{ExpectedVersion: attempt.Version, At: at, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	if _, _, err := store.ApplyAttempt(ctx, uow, f.tenant, attempt.ID, lifecycle.AttemptProvision, "", tr); err != nil {
		t.Fatalf("ApplyAttempt() error = %v", err)
	}
	task, err := store.GetTask(ctx, uow, f.tenant, attempt.Task)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	if task.State == lifecycle.TaskDraft {
		trSubmit := store.Transition{ExpectedVersion: task.Version, At: at, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
		task, _, err = store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskSubmit, "", trSubmit)
		if err != nil {
			t.Fatalf("ApplyTask() error = %v", err)
		}
	}
	trTask := store.Transition{ExpectedVersion: task.Version, At: at, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskDispatch, "", trTask); err != nil {
		t.Fatalf("ApplyTask() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
}

func (f *fixture) readyCount(t *testing.T, ctx context.Context, at time.Time) int {
	t.Helper()
	rows, err := store.OutboxReadyDispatch(ctx, f.pool, f.tenant, at, 100)
	if err != nil {
		t.Fatalf("OutboxReadyDispatch() error = %v", err)
	}
	return len(rows)
}

func TestWorkerDispatchAcceptedEndToEnd(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, attemptID := f.dispatchableOp(t, ctx, now, contracts.DemandPerson, 1)
	w := testWorker(t, f, worker.DefaultConfig())

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	sent := f.loopback.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent() = %d requests, want 1", len(sent))
	}
	request := sent[0]
	if request.Operation != opID || request.Delivery.IsZero() || request.Tenant != f.tenant ||
		request.Attempt != attemptID || request.Role != contracts.RoleRuntime ||
		request.Method != contracts.MethodStartTurn || request.Demand != contracts.DemandPerson {
		t.Fatalf("Send() request = %+v, want the dispatched operation", request)
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationAccepted {
		t.Fatalf("operation state = %s, want accepted", operation.State)
	}
	if got := f.readyCount(t, ctx, now.Add(time.Minute)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the row acked", got)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests after a second tick, want no redispatch", len(f.loopback.Sent()))
	}
}

func TestWorkerStaleRacerLoses(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, _ := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	w := testWorker(t, f, worker.DefaultConfig())
	pollAt := now.Add(time.Minute)

	// A rival holds the row in a committed claim: the tick must not send.
	rival, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	rivalRows, err := store.OutboxReadyDispatch(ctx, rival, f.tenant, pollAt, 10)
	if err != nil || len(rivalRows) != 1 {
		_ = rival.Rollback(ctx)
		t.Fatalf("OutboxReadyDispatch() = %v, %v", rivalRows, err)
	}
	rivalClaim, err := store.ClaimOutboxRowByID(ctx, rival, f.tenant, rivalRows[0].ID, "rival", time.Hour, pollAt)
	if err != nil {
		_ = rival.Rollback(ctx)
		t.Fatalf("rival ClaimOutboxRowByID() error = %v", err)
	}
	if err := rival.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 0 {
		t.Fatal("Tick() sent on a row held by a rival")
	}
	// The rival yields: the next tick sends exactly once. The release
	// stamps the test's already-past now so the row is due for the
	// real-clock tick.
	release, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := store.ReleaseOutboxRow(ctx, release, f.tenant, rivalClaim.Row.ID, rivalClaim.Epoch, now); err != nil {
		_ = release.Rollback(ctx)
		t.Fatalf("ReleaseOutboxRow() error = %v", err)
	}
	if err := release.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want exactly one send", len(f.loopback.Sent()))
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationAccepted {
		t.Fatalf("operation state = %s, want accepted", operation.State)
	}
}

func TestWorkerRestartRecovery(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, _ := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	w := testWorker(t, f, worker.DefaultConfig())
	pollAt := now.Add(time.Minute)

	// A tick crashes between claim and send: the claim is committed, the
	// row stays processing, and the lease is already lapsed (claimed at
	// the test's past now with zero TTL).
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	rows, err := store.OutboxReadyDispatch(ctx, uow, f.tenant, pollAt, 10)
	if err != nil || len(rows) != 1 {
		_ = uow.Rollback(ctx)
		t.Fatalf("OutboxReadyDispatch() = %v, %v", rows, err)
	}
	if _, err := store.ClaimOutboxRowByID(ctx, uow, f.tenant, rows[0].ID, "dead-worker", 0, now); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ClaimOutboxRowByID() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// A legacy processing claim has no proof that its owner stopped before
	// sending. Restart reconciles that ambiguity instead of guessing no effect.
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 0 {
		t.Fatalf("Sent() = %d requests after restart, want reconciliation only", len(f.loopback.Sent()))
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationUncertain {
		t.Fatalf("operation state = %s, want uncertain", operation.State)
	}
	if got := f.readyCount(t, ctx, pollAt.Add(time.Minute)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the row acked", got)
	}
}

func TestWorkerExhaustionStopsDispatch(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, _ := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	f.moveDispatched(t, ctx, opID, now)
	cfg := worker.DefaultConfig()
	cfg.Policy.MaxAttempts = 1
	w := testWorker(t, f, cfg)
	f.loopback.ScriptSendError(opID, errTransportForTest)

	// A transport failure may follow an effect. Park it immediately rather
	// than spending the retry budget by repeating an uncertain start.
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want the single attempt", len(f.loopback.Sent()))
	}
	ready, err := store.OutboxReadyDispatch(ctx, f.pool, f.tenant, now.Add(24*time.Hour), 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("OutboxReadyDispatch() = %v, %v, want uncertain work removed from dispatch", ready, err)
	}
	// Further ticks cannot retry the operation, even with a one-send policy.
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := f.readyCount(t, ctx, now.Add(24*time.Hour)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the row exhausted", got)
	}
	// Giving up on a failed send cannot leave the operation dispatched: an
	// earlier attempt may have delivered the request, so the operation is
	// uncertain and the Attempt waits in reconciliation under its running
	// Task. No woken state of the dispatch alone may fail such work.
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationUncertain {
		t.Fatalf("operation state = %s, want uncertain (outcome of failed dispatch)", operation.State)
	}
	attempt := f.attemptState(t, ctx, opID)
	if attempt.State != lifecycle.AttemptReconciling {
		t.Fatalf("attempt state = %s, want reconciling", attempt.State)
	}
	if task := f.taskState(t, ctx, attempt); task.State != lifecycle.TaskRunning {
		t.Fatalf("task state = %s, want running under reconciliation", task.State)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests after two more ticks, want no spin", len(f.loopback.Sent()))
	}
}

func TestWorkerDoesNotRetryStartAfterCancellation(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, attemptID := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	f.moveDispatched(t, ctx, opID, now)
	w := testWorker(t, f, worker.DefaultConfig())
	f.loopback.ScriptSendError(opID, errTransportForTest)

	// The first start delivery fails and enters backoff.
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("first Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want the failed start", len(f.loopback.Sent()))
	}

	// Commit cancellation and its interrupt while the original start waits.
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	task := f.taskState(t, ctx, f.attemptState(t, ctx, opID))
	tr := store.Transition{ExpectedVersion: task.Version, At: now.Add(time.Second), Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskCancel, "", tr); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyTask(cancel) error = %v", err)
	}
	attempt, err := store.GetAttempt(ctx, uow, f.tenant, attemptID)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("GetAttempt() error = %v", err)
	}
	cancelBody, err := json.Marshal(map[string]any{
		"operation": opID.String(), "role": "runtime", "method": "interrupt",
		"deadline": now.Add(time.Hour).UTC().Format(time.RFC3339), "cancel_reason": "user stop",
	})
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("json.Marshal() error = %v", err)
	}
	tr = store.Transition{
		ExpectedVersion: attempt.Version, At: now.Add(time.Second), Actor: f.actor, AuditID: mustID[ids.Audit](t),
		Outbox: &store.OutboxPayload{ID: attempt.ID.Untyped(), Attempt: attempt.ID, Body: cancelBody},
	}
	if _, _, err := store.ApplyAttempt(ctx, uow, f.tenant, attempt.ID, lifecycle.AttemptRequestCancel, "", tr); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyAttempt(request_cancel) error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("cancel Tick() error = %v", err)
	}
	if len(f.loopback.Cancels()) != 1 {
		t.Fatalf("Cancels() = %d, want the interrupt acknowledged", len(f.loopback.Cancels()))
	}

	// Make the backed-off start due. Cancellation must exhaust it without a
	// second provider send after the only interrupt.
	if _, err := f.pool.Exec(ctx, `update outbox set next_attempt_at = now() - interval '1 second'
		where tenant_id = $1 and record_kind = 'provider_operation' and record_id = $2`, f.tenant.String(), opID.String()); err != nil {
		t.Fatalf("force start row due: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("post-cancel Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want no start after cancellation", len(f.loopback.Sent()))
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationUncertain {
		t.Fatalf("operation state = %s, want uncertain after the earlier transport failure", operation.State)
	}
	if attempt := f.attemptState(t, ctx, opID); attempt.State != lifecycle.AttemptCancellationRequested {
		t.Fatalf("attempt state = %s, want cancellation_requested pending stop evidence", attempt.State)
	}
}

func TestWorkerPersonBeforeBackground(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, _ = f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	_, _ = f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	_, _ = f.dispatchableOp(t, ctx, now, contracts.DemandPerson, 1)
	cfg := worker.DefaultConfig()
	cfg.BackgroundBudget = 1
	w := testWorker(t, f, cfg)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	sent := f.loopback.Sent()
	if len(sent) != 2 {
		t.Fatalf("Sent() = %d requests, want person plus one background", len(sent))
	}
	if sent[0].Demand != contracts.DemandPerson {
		t.Fatalf("first send demand = %s, want the person row first", sent[0].Demand)
	}
	if sent[1].Demand != contracts.DemandBackground {
		t.Fatalf("second send demand = %s, want bounded background", sent[1].Demand)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 3 {
		t.Fatalf("Sent() = %d requests, want the last background row served", len(f.loopback.Sent()))
	}
}

func TestWorkerCancelIndependentOfStop(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	w := testWorker(t, f, worker.DefaultConfig())

	// Drive one attempt to cancellation_requested through the store.
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	task, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, now)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	if _, _, err := store.InsertTask(ctx, uow, task); err != nil {
		t.Fatalf("InsertTask() error = %v", err)
	}
	attempt, err := lifecycle.NewAttempt(mustID[ids.Attempt](t), f.tenant, task.ID, 1, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	if _, _, err := store.InsertAttempt(ctx, uow, attempt); err != nil {
		t.Fatalf("InsertAttempt() error = %v", err)
	}
	drive := func(event lifecycle.AttemptEvent, target lifecycle.AttemptState, version uint64) {
		t.Helper()
		tr := store.Transition{ExpectedVersion: version, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
		if _, _, err := store.ApplyAttempt(ctx, uow, f.tenant, attempt.ID, event, target, tr); err != nil {
			_ = uow.Rollback(ctx)
			t.Fatalf("ApplyAttempt(%s) error = %v", event, err)
		}
	}
	drive(lifecycle.AttemptProvision, lifecycle.AttemptProvisioning, 1)
	drive(lifecycle.AttemptStart, lifecycle.AttemptStarting, 2)
	drive(lifecycle.AttemptRuntimeStarted, lifecycle.AttemptExecuting, 3)
	opID := mustID[ids.Operation](t)
	operation, err := lifecycle.NewProviderOperation(opID, f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"operation": opID.String(), "role": "runtime", "method": "interrupt",
		"deadline": now.Add(time.Hour).UTC().Format(time.RFC3339), "cancel_reason": "user stop",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	outboxID := mustID[ids.Any](t)
	tr := store.Transition{
		ExpectedVersion: 4, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t),
		Outbox: &store.OutboxPayload{ID: outboxID, Attempt: attempt.ID, Body: body},
	}
	if _, _, err := store.ApplyAttempt(ctx, uow, f.tenant, attempt.ID, lifecycle.AttemptRequestCancel, lifecycle.AttemptCancellationRequested, tr); err != nil {
		t.Fatalf("ApplyAttempt(request_cancel) error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	cancels := f.loopback.Cancels()
	if len(cancels) != 1 || cancels[0].Operation != opID {
		t.Fatalf("Cancels() = %+v, want the one protocol cancel", cancels)
	}
	if got := f.readyCount(t, ctx, now.Add(time.Minute)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the cancel acked", got)
	}
	stored, err := store.GetAttempt(ctx, f.pool, f.tenant, attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	if stored.State != lifecycle.AttemptCancellationRequested {
		t.Fatalf("attempt state = %s, want cancellation_requested without observed stops", stored.State)
	}
	if len(f.loopback.Sent()) != 0 {
		t.Fatal("Tick() sent a provider request on the cancel path")
	}
}

func TestWorkerUnknownDurabilityReconciles(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, attemptID := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	w := testWorker(t, f, worker.DefaultConfig())
	f.loopback.ScriptReply(opID, contracts.OutcomeFailed, lifecycle.SendUnknown,
		&contracts.Failure{Code: contracts.FailureProviderFailed, Message: "lost reply"}, nil)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationUncertain {
		t.Fatalf("operation state = %s, want uncertain", operation.State)
	}
	if got := f.readyCount(t, ctx, now.Add(time.Minute)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the intent acked", got)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want no blind repeat of the uncertain effect", len(f.loopback.Sent()))
	}
	outcome, err := json.Marshal(map[string]string{"state": "succeeded"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	f.loopback.ScriptFrames(opID, contracts.StreamFrame{
		Kind:       contracts.StreamRows,
		ServerTime: now,
		Rows: &contracts.Rows{Entries: []contracts.Entry{{
			Sequence: 1, UpdatedSequence: 1, Kind: "operation_outcome",
			Payload: outcome, PayloadHash: identity.DigestOf(outcome),
		}}},
	})
	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationSucceeded {
		t.Fatalf("operation state = %s, want reconciled success", operation.State)
	}
	if len(f.loopback.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests after reconcile, want no redispatch", len(f.loopback.Sent()))
	}
	cursor, err := store.GetCursor(ctx, f.pool, f.tenant, opID, "dispatch")
	if err != nil {
		t.Fatalf("GetCursor() error = %v", err)
	}
	if cursor.Attempt != attemptID || cursor.After != 1 {
		t.Fatalf("cursor = %+v, want progress persisted", cursor)
	}
}

func TestWorkerStaleMaintenanceTakeover(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	w := testWorker(t, f, worker.DefaultConfig())

	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	claim, err := lifecycle.NewMaintenanceClaim(mustID[ids.Claim](t), f.tenant, f.provider, "worker-a", now)
	if err != nil {
		t.Fatalf("NewMaintenanceClaim() error = %v", err)
	}
	if _, _, err := store.InsertClaim(ctx, uow, claim); err != nil {
		t.Fatalf("InsertClaim() error = %v", err)
	}
	tr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	held, err := store.TakeClaim(ctx, uow, f.tenant, claim.ID, "old-worker", time.Hour, tr)
	if err != nil {
		t.Fatalf("TakeClaim() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	// The holder dies without renewing: lapse its lease directly.
	if _, err := f.pool.Exec(ctx, `update maintenance_claim set expires_at = now() - interval '1 second'
		where tenant_id = $1 and claim_id = $2`, f.tenant.String(), claim.ID.String()); err != nil {
		t.Fatalf("lapse claim: %v", err)
	}

	if err := w.ReapClaims(ctx); err != nil {
		t.Fatalf("ReapClaims() error = %v", err)
	}
	taken, err := store.GetClaim(ctx, f.pool, f.tenant, claim.ID)
	if err != nil {
		t.Fatalf("GetClaim() error = %v", err)
	}
	if taken.Claimant != "test-worker" || taken.Epoch != held.Epoch+1 {
		t.Fatalf("GetClaim() = %+v, want the worker as holder at epoch %d", taken, held.Epoch+1)
	}
}

func TestWorkerShadowWithholdsSend(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, attemptID := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 1)
	w := testWorker(t, f, worker.DefaultConfig())

	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := store.UpsertHarness(ctx, uow, lifecycle.HarnessQualification{
		Tenant: f.tenant, Provider: f.provider, Mode: lifecycle.HarnessShadow, Version: 2, UpdatedAt: now,
	}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("UpsertHarness() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 0 {
		t.Fatal("Tick() sent on a shadow harness")
	}
	if got := f.readyCount(t, ctx, now.Add(time.Minute)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the shadow row acked", got)
	}
	results, err := store.ListShadowResults(ctx, f.pool, f.tenant, attemptID)
	if err != nil {
		t.Fatalf("ListShadowResults() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ListShadowResults() = %d rows, want the withheld dispatch", len(results))
	}
}

func TestWorkerBudgetCeilingStopsDispatch(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, _ := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 10)
	f.moveDispatched(t, ctx, opID, now)
	w := testWorker(t, f, worker.DefaultConfig())

	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	taskRows, err := store.OutboxReadyDispatch(ctx, uow, f.tenant, now.Add(time.Minute), 10)
	if err != nil || len(taskRows) != 1 {
		t.Fatalf("OutboxReadyDispatch() = %v, %v", taskRows, err)
	}
	operation, err := store.GetOperation(ctx, uow, f.tenant, opID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	attempt, err := store.GetAttempt(ctx, uow, f.tenant, operation.Attempt)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	budget, err := lifecycle.NewBudget(mustID[ids.Budget](t), f.tenant, attempt.Task, lifecycle.Unit("tokens"), 5, now)
	if err != nil {
		t.Fatalf("NewBudget() error = %v", err)
	}
	if _, _, err := store.InsertBudget(ctx, uow, budget); err != nil {
		t.Fatalf("InsertBudget() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(f.loopback.Sent()) != 0 {
		t.Fatal("Tick() sent past the budget ceiling")
	}
	if got := f.readyCount(t, ctx, now.Add(24*time.Hour)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the over-ceiling row exhausted", got)
	}
	// Nothing in flight with nothing sent: the worker refuses its own
	// dispatch, so the operation is rejected and carrying the Attempt and
	// its running Task to failed ends the live states rather than leaving a
	// Task running past an exhausted row.
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationRejected {
		t.Fatalf("operation state = %s, want rejected (never sent, ceiling exhausted)", operation.State)
	}
	attempt = f.attemptState(t, ctx, opID)
	if attempt.State != lifecycle.AttemptFailed {
		t.Fatalf("attempt state = %s, want failed", attempt.State)
	}
	if task := f.taskState(t, ctx, attempt); task.State != lifecycle.TaskFailed {
		t.Fatalf("task state = %s, want failed", task.State)
	}
}
