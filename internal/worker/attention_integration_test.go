//go:build integration

package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

type decidedAttention struct {
	request   lifecycle.AttentionRequest
	operation lifecycle.ProviderOperation
	row       store.OutboxRow
}

type mismatchedDeliveryPort struct {
	*worker.LoopbackPort
	delivery ids.DeliveryID
}

func (p *mismatchedDeliveryPort) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.LoopbackPort.Send(ctx, request)
	if err == nil {
		reply.Delivery = p.delivery
	}
	return reply, err
}

// arrangeDecidedAttention records one question and answers it through the
// application boundary, leaving the durable decision delivery for the worker.
func (f *fixture) arrangeDecidedAttention(t *testing.T, ctx context.Context, at time.Time) decidedAttention {
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
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, at)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, replayed, err := store.InsertOperation(ctx, uow, operation); err != nil || replayed {
		t.Fatalf("InsertOperation() = %v, %v", replayed, err)
	}
	request, err := lifecycle.NewAttentionRequest(lifecycle.AttentionRequest{
		ID: mustID[ids.Attention](t), Tenant: f.tenant, Task: task.ID, Attempt: attempt.ID,
		Kind: lifecycle.AttentionQuestion, PolicyGeneration: 1,
		Responders: []ids.PrincipalID{f.principal}, ExpiresAt: at.Add(time.Hour),
	}, at)
	if err != nil {
		t.Fatalf("NewAttentionRequest() error = %v", err)
	}
	if _, replayed, err := store.InsertAttention(ctx, uow, request); err != nil || replayed {
		t.Fatalf("InsertAttention() = %v, %v", replayed, err)
	}
	if _, replayed, err := store.InsertAttentionNative(ctx, uow, store.AttentionNative{
		Tenant: f.tenant, Provider: f.provider, NativeRef: "question-1",
		Attention: request.ID, CreatedAt: at,
	}); err != nil || replayed {
		t.Fatalf("InsertAttentionNative() = %v, %v", replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true

	auth, err := identity.NewAuthorizationContext(identity.Principal{
		Tenant: f.tenant, ID: f.principal, Kind: identity.PrincipalHuman,
	})
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	if _, err := application.Decide(ctx, f.pool, application.DecideAttention{
		Command:   application.Command{Actor: auth, Nonce: "decision-1", ExpectedVersion: request.Version},
		Attention: request.ID, Kind: lifecycle.DecisionDeny, At: at.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	request, err = store.GetAttention(ctx, f.pool, f.tenant, request.ID)
	if err != nil {
		t.Fatalf("GetAttention() error = %v", err)
	}
	rows, err := store.OutboxReadyDispatch(ctx, f.pool, f.tenant, at.Add(2*time.Minute), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("OutboxReadyDispatch() = %+v, %v; want the decision delivery", rows, err)
	}
	return decidedAttention{request: request, operation: operation, row: rows[0]}
}

func TestDeliveryRedeliversAfterCrashBetweenDecideAndSend(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decided := f.arrangeDecidedAttention(t, ctx, now)

	// The dead worker committed every pre-send fact, then exited before the
	// port call. Both leases are already lapsed so a restarted worker can take
	// over without guessing whether the decision itself exists.
	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	claim, err := store.ClaimOutboxRowByID(ctx, uow, f.tenant, decided.row.ID, "dead-worker", 0, now)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ClaimOutboxRowByID() error = %v", err)
	}
	tr := store.Transition{
		ExpectedVersion: decided.request.Version, At: now, Actor: f.actor,
		AuditID: mustID[ids.Audit](t),
	}
	if _, replayed, err := store.ApplyAttention(ctx, uow, f.tenant, decided.request.ID, lifecycle.AttentionDispatch, tr); err != nil || replayed {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyAttention(dispatch) = %v, %v", replayed, err)
	}
	if _, err := store.ClaimAttemptLease(ctx, uow, f.tenant, decided.request.Attempt, claim.Owner, 0, now); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ClaimAttemptLease() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	w := testWorker(t, f, worker.DefaultConfig())
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	sent := f.loopback.Sent()
	if len(sent) != 1 || sent[0].Operation != decided.operation.ID || sent[0].Method != contracts.MethodSteerTurn {
		t.Fatalf("Sent() = %+v, want one reclaimed steer", sent)
	}
	request, err := store.GetAttention(ctx, f.pool, f.tenant, decided.request.ID)
	if err != nil || request.State != lifecycle.AttentionDelivered {
		t.Fatalf("GetAttention() = %+v, %v; want delivered", request, err)
	}
	if got := f.readyCount(t, ctx, now.Add(time.Hour)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the delivery acked", got)
	}
}

func TestDeliveryRejectedSteerSupersedesRequest(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decided := f.arrangeDecidedAttention(t, ctx, now)
	f.loopback.ScriptReply(decided.operation.ID, contracts.OutcomeFailed, lifecycle.SendRejected,
		&contracts.Failure{Code: contracts.FailureProviderFailed, Message: "native request is gone"}, nil)

	w := testWorker(t, f, worker.DefaultConfig())
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	request, err := store.GetAttention(ctx, f.pool, f.tenant, decided.request.ID)
	if err != nil || request.State != lifecycle.AttentionSuperseded {
		t.Fatalf("GetAttention() = %+v, %v; want superseded", request, err)
	}
	if got := len(f.loopback.Sent()); got != 1 {
		t.Fatalf("Sent() = %d requests, want one rejected steer", got)
	}
	if got := f.readyCount(t, ctx, now.Add(time.Hour)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the delivery exhausted", got)
	}
}

func TestDeliveryRejectsReplyForAnotherDelivery(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decided := f.arrangeDecidedAttention(t, ctx, now)
	port := &mismatchedDeliveryPort{LoopbackPort: f.loopback, delivery: mustID[ids.Delivery](t)}
	w := testWorkerWithPort(t, f, worker.DefaultConfig(), port)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	request, err := store.GetAttention(ctx, f.pool, f.tenant, decided.request.ID)
	if err != nil || request.State != lifecycle.AttentionDispatching {
		t.Fatalf("GetAttention() = %+v, %v; want dispatching after malformed reply", request, err)
	}
	row, err := store.GetOutboxRow(ctx, f.pool, f.tenant, decided.row.ID)
	if err != nil || row.ExhaustedAt.IsZero() || row.LastError != "malformed_reply" {
		t.Fatalf("GetOutboxRow() = %+v, %v; want malformed reply exhaustion", row, err)
	}
}

func TestDeliveryDuplicateDoesNotDoubleSend(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decided := f.arrangeDecidedAttention(t, ctx, now)
	w := testWorker(t, f, worker.DefaultConfig())

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	// Replay the same durable arrow after its acknowledgement. The delivered
	// request already has its story, so the worker must ack without another
	// provider call.
	if _, err := f.pool.Exec(ctx, `update outbox set status = 'pending', exhausted_at = null,
		claimed_at = null, claimed_by = null, lease_expires_at = null,
		next_attempt_at = now() - interval '1 second'
		where tenant_id = $1 and outbox_id = $2`, f.tenant.String(), decided.row.ID.String()); err != nil {
		t.Fatalf("replay delivery row: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("replay Tick() error = %v", err)
	}
	if got := len(f.loopback.Sent()); got != 1 {
		t.Fatalf("Sent() = %d requests, want one steer for the decision", got)
	}
	request, err := store.GetAttention(ctx, f.pool, f.tenant, decided.request.ID)
	if err != nil || request.State != lifecycle.AttentionDelivered {
		t.Fatalf("GetAttention() = %+v, %v; want delivered", request, err)
	}
}

func TestDecisionRetryBudgetDoesNotSendAgain(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decided := f.arrangeDecidedAttention(t, ctx, now)
	f.loopback.ScriptSendError(decided.operation.ID, errTransportForTest)
	cfg := worker.DefaultConfig()
	cfg.Policy.MaxAttempts = 1
	w := testWorker(t, f, cfg)

	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	ready, err := store.OutboxReadyDispatch(ctx, f.pool, f.tenant, now.Add(24*time.Hour), 10)
	if err != nil || len(ready) != 1 || ready[0].Attempts != 1 {
		t.Fatalf("OutboxReadyDispatch() = %+v, %v; want one failed send", ready, err)
	}
	if _, err := f.pool.Exec(ctx, `update outbox set next_attempt_at = now() - interval '1 second'
		where tenant_id = $1 and outbox_id = $2`, f.tenant.String(), decided.row.ID.String()); err != nil {
		t.Fatalf("force delivery due: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if got := len(f.loopback.Sent()); got != 1 {
		t.Fatalf("Sent() = %d requests, want no send past the retry budget", got)
	}
	if got := f.readyCount(t, ctx, now.Add(24*time.Hour)); got != 0 {
		t.Fatalf("OutboxReadyDispatch() = %d rows, want the delivery exhausted", got)
	}
	request, err := store.GetAttention(ctx, f.pool, f.tenant, decided.request.ID)
	if err != nil || request.State != lifecycle.AttentionDispatching {
		t.Fatalf("GetAttention() = %+v, %v; want inspectable dispatching state", request, err)
	}
}

func TestAbandonReconcilesNeverSentCancel(t *testing.T) {
	ctx, f := setupFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	opID, attemptID := f.dispatchableOp(t, ctx, now, contracts.DemandBackground, 10)
	f.moveDispatched(t, ctx, opID, now)

	uow, err := store.Begin(ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	attempt, err := store.GetAttempt(ctx, uow, f.tenant, attemptID)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("GetAttempt() error = %v", err)
	}
	for _, event := range []lifecycle.AttemptEvent{lifecycle.AttemptStart, lifecycle.AttemptRuntimeStarted, lifecycle.AttemptRequestCancel} {
		tr := store.Transition{ExpectedVersion: attempt.Version, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
		attempt, _, err = store.ApplyAttempt(ctx, uow, f.tenant, attempt.ID, event, "", tr)
		if err != nil {
			_ = uow.Rollback(ctx)
			t.Fatalf("ApplyAttempt(%s) error = %v", event, err)
		}
	}
	task, err := store.GetTask(ctx, uow, f.tenant, attempt.Task)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("GetTask() error = %v", err)
	}
	tr := store.Transition{ExpectedVersion: task.Version, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskCancel, "", tr); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyTask(cancel) error = %v", err)
	}
	budget, err := lifecycle.NewBudget(mustID[ids.Budget](t), f.tenant, task.ID, "tokens", 100, now)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("NewBudget() error = %v", err)
	}
	if _, replayed, err := store.InsertBudget(ctx, uow, budget); err != nil || replayed {
		_ = uow.Rollback(ctx)
		t.Fatalf("InsertBudget() = %v, %v", replayed, err)
	}
	reservationID, err := ids.Parse[ids.Reservation](attempt.ID.String())
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("Parse() error = %v", err)
	}
	tr = store.Transition{ExpectedVersion: budget.Version, At: now, Actor: f.actor, AuditID: mustID[ids.Audit](t)}
	if _, _, replayed, err := store.ReserveBudget(ctx, uow, store.ReservationRequest{
		Tenant: f.tenant, Budget: budget.ID, Reservation: reservationID, Attempt: attempt.ID, Amount: 10,
	}, tr); err != nil || replayed {
		_ = uow.Rollback(ctx)
		t.Fatalf("ReserveBudget() = %v, %v", replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// Mark the first send budget spent without ever claiming the row. The
	// worker exhausts before a provider call, so epoch one proves never-sent.
	if _, err := f.pool.Exec(ctx, `update outbox set attempts = 1
		where tenant_id = $1 and record_kind = 'provider_operation' and record_id = $2`,
		f.tenant.String(), opID.String()); err != nil {
		t.Fatalf("spend retry budget: %v", err)
	}
	cfg := worker.DefaultConfig()
	cfg.Policy.MaxAttempts = 1
	w := testWorker(t, f, cfg)
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := len(f.loopback.Sent()); got != 0 {
		t.Fatalf("Sent() = %d requests, want the cancel reconciled without a send", got)
	}
	if operation := f.operationState(t, ctx, opID); operation.State != lifecycle.OperationRejected {
		t.Fatalf("operation state = %s, want rejected", operation.State)
	}
	attempt = f.attemptState(t, ctx, opID)
	if attempt.State != lifecycle.AttemptTerminated {
		t.Fatalf("attempt state = %s, want terminated", attempt.State)
	}
	if task = f.taskState(t, ctx, attempt); task.State != lifecycle.TaskCancelled {
		t.Fatalf("task state = %s, want cancelled", task.State)
	}
	reservation, err := store.GetReservation(ctx, f.pool, f.tenant, reservationID)
	if err != nil || reservation.State != lifecycle.ReservationReleased {
		t.Fatalf("GetReservation() = %+v, %v; want released", reservation, err)
	}
}
