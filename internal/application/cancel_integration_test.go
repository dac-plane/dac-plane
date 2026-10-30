//go:build integration

package application_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// seedSendPolicy makes the approval's action askable, so an approval's grant
// issues under the tenant's current generation.
func (f *fixture) seedSendPolicy(t *testing.T) {
	t.Helper()
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, uow, store.PolicyUpdate{
			Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 1,
			Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAsk},
			Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAsk},
			At:       f.at,
		})
		return err
	})
}

func (f *fixture) cancelCommand(nonce string, taskID ids.TaskID, version uint64) application.CancelTask {
	return application.CancelTask{
		Command: application.Command{Actor: f.actor, Nonce: nonce, ExpectedVersion: version},
		Task:    taskID, At: f.at,
	}
}

// cancel cancels the submission's task on its current version.
func (f *fixture) cancel(t *testing.T, s application.Submission, nonce string) application.CancelView {
	t.Helper()
	task, err := store.GetTask(f.ctx, f.pool, f.tenant, s.Task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	view, err := application.Cancel(f.ctx, f.pool, f.cancelCommand(nonce, s.Task.ID, task.Version))
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	return view
}

func (f *fixture) environmentStopOperation(t *testing.T, s application.Submission) ids.OperationID {
	t.Helper()
	var text string
	err := f.pool.QueryRow(f.ctx, `select record_id::text from outbox
		where tenant_id=$1 and attempt_id=$2 and record_kind='provider_operation'
		and payload->>'role'='environment' and payload->>'method'='stop'
		order by created_at desc limit 1`, f.tenant.String(), s.Attempt.ID.String()).Scan(&text)
	if err != nil {
		t.Fatalf("environment stop operation: %v", err)
	}
	op, err := ids.Parse[ids.Operation](text)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// reservation reads the attempt-derived reservation the budget gate expects.
// observeEnvironmentStop reports the assigned environment absent on its stop
// operation, as a provider does once the resource is gone.
func (f *fixture) observeEnvironmentStop(t *testing.T, s application.Submission, sequence uint64) {
	t.Helper()
	if got := f.ingestEnvironmentNow(t, f.environmentEvent(s, f.environmentStopOperation(t, s), simulator.RowsFrame(false,
		row(1, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunAbsent, Sequence: sequence, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("environment stop = %+v, want applied", got)
	}
}

func (f *fixture) reservation(t *testing.T, s application.Submission) lifecycle.Reservation {
	t.Helper()
	id, err := ids.Parse[ids.Reservation](s.Attempt.ID.String())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got, err := store.GetReservation(f.ctx, f.pool, f.tenant, id)
	if err != nil {
		t.Fatalf("GetReservation() error = %v", err)
	}
	return got
}

func TestCancelQueuedTaskCancelsAndReleases(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-1")
	view := f.cancel(t, s, "c-1")
	if view.Nonce != "c-1" || view.State != string(lifecycle.TaskCancelled) ||
		view.AttemptState != string(lifecycle.AttemptTerminated) || view.Reason != "user_stop" {
		t.Fatalf("view = %+v, want cancelled/terminated for user_stop", view)
	}
	// Nothing was started, so no interrupt dispatches and the unused hold
	// releases in the cancel's own commit.
	if n := f.count(t, "outbox", "event = 'request_cancel'"); n != 0 {
		t.Fatalf("cancel dispatches = %d, want none for a queued task", n)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
	budget, err := store.GetBudget(f.ctx, f.pool, f.tenant, s.Budget.ID)
	if err != nil || budget.Reserved != 0 {
		t.Fatalf("budget = %+v, %v; want nothing reserved", budget, err)
	}
	// The replay returns the stored result.
	replay, err := application.Cancel(f.ctx, f.pool, f.cancelCommand("c-1", s.Task.ID, s.Task.Version))
	if err != nil || replay.State != string(lifecycle.TaskCancelled) {
		t.Fatalf("replay = %+v, %v; want the cancelled view", replay, err)
	}
}

func TestCancelRequiresTaskOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-1")
	other := mustID[ids.Principal](t)
	f.seed(t, func(uow *store.UnitOfWork) error {
		return store.InsertPrincipal(f.ctx, uow, identity.Principal{
			Tenant: f.tenant, ID: other, Kind: identity.PrincipalHuman,
		})
	})
	auth, err := identity.NewAuthorizationContext(identity.Principal{
		Tenant: f.tenant, ID: other, Kind: identity.PrincipalHuman,
	})
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	cmd := f.cancelCommand("c-other", s.Task.ID, s.Task.Version)
	cmd.Actor = auth
	if _, err := application.Cancel(f.ctx, f.pool, cmd); err != nil {
		requireRefusal(t, err, lifecycle.RejectUnauthorizedActor)
	} else {
		t.Fatal("Cancel() by another principal succeeded")
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskQueued || attempt != lifecycle.AttemptAllocated {
		t.Fatalf("states = %s/%s, want unchanged queued/allocated", task, attempt)
	}
}

func TestCancelReadyForReviewKeepsCompletedAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, _ := f.executing(t, "n-1")
	f.seed(t, func(uow *store.UnitOfWork) error {
		step := progress.Step{UOW: uow, Tenant: f.tenant, Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal}, At: f.at}
		if _, _, _, err := step.Attempt(f.ctx, s.Attempt.ID, lifecycle.AttemptFinalize, ""); err != nil {
			return err
		}
		if _, _, _, err := step.Attempt(f.ctx, s.Attempt.ID, lifecycle.AttemptComplete, ""); err != nil {
			return err
		}
		for _, event := range []lifecycle.TaskEvent{lifecycle.TaskProposeResult, lifecycle.TaskVerificationPassed} {
			task, err := store.GetTask(f.ctx, uow, f.tenant, s.Task.ID)
			if err != nil {
				return err
			}
			auditID, err := ids.New[ids.Audit]()
			if err != nil {
				return err
			}
			if _, _, err := store.ApplyTask(f.ctx, uow, f.tenant, task.ID, event, "", store.Transition{
				ExpectedVersion: task.Version, At: f.at, Actor: step.Actor, AuditID: auditID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	task, err := store.GetTask(f.ctx, f.pool, f.tenant, s.Task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	view, err := application.Cancel(f.ctx, f.pool, f.cancelCommand("c-review", task.ID, task.Version))
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if view.State != string(lifecycle.TaskCancelled) || view.AttemptState != string(lifecycle.AttemptCompleted) {
		t.Fatalf("view = %+v, want cancelled/completed", view)
	}
}

func TestCancelDispatchedTaskWaitsForStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	f.seedSendPolicy(t)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, approval("a-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	approved := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionApproveOnce))
	grantID, err := ids.Parse[ids.Grant](approved.GrantID)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	view := f.cancel(t, s, "c-1")
	if view.State != string(lifecycle.TaskCancelling) || view.AttemptState != string(lifecycle.AttemptCancellationRequested) {
		t.Fatalf("view = %+v, want cancelling/cancellation_requested", view)
	}
	// The intent rides one interrupt dispatch on the attempt.
	rows, err := store.OutboxReadyDispatch(f.ctx, f.pool, f.tenant, f.at.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("OutboxReadyDispatch() error = %v", err)
	}
	var cancelRow *store.OutboxRow
	for i := range rows {
		if rows[i].Record == lifecycle.RecordAttempt && rows[i].Event == "request_cancel" {
			cancelRow = &rows[i]
		}
	}
	switch {
	case cancelRow == nil:
		t.Fatalf("rows = %+v, want one request_cancel row", rows)
	case cancelRow.Attempt != s.Attempt.ID || cancelRow.Demand != contracts.DemandPerson:
		t.Fatalf("cancel row = %+v, want a person row for the attempt", cancelRow)
	}
	var body map[string]any
	if err := json.Unmarshal(cancelRow.Payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body["method"] != string(contracts.MethodInterrupt) || body["cancel_reason"] != "user_stop" {
		t.Fatalf("body = %v, want an interrupt for user_stop", body)
	}
	// The wait is cancelled and the unused grant revoked before the work ends.
	if got := f.attention(t, s); got.State != lifecycle.AttentionCancelled {
		t.Fatalf("request = %s, want cancelled", got.State)
	}
	grant, err := store.GetGrant(f.ctx, f.pool, f.tenant, grantID)
	if err != nil {
		t.Fatalf("GetGrant() error = %v", err)
	}
	if grant.RevokedAt.IsZero() {
		t.Fatalf("grant = %+v, want revoked", grant)
	}
	// The interrupt dispatches on the next tick.
	f.tick(t, rt)
	// The adapter records a cancel asynchronously once the frame arrives.
	deadline := time.Now().Add(5 * time.Second)
	for len(rt.Ledger().Cancelled()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("protocol cancel was not delivered")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(rt.Ledger().Cancelled()); n != 1 {
		t.Fatalf("protocol cancels = %d, want 1", n)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
}

func TestEventEnvironmentStopConvergesCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.cancel(t, s, "c-1")
	stopOperation := f.environmentStopOperation(t, s)
	// The environment's stop alone is one of two: the attempt keeps waiting.
	if got := f.ingestEnvironmentNow(t, f.environmentEvent(s, stopOperation, simulator.RowsFrame(false,
		row(1, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("stop observation = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	// With the runtime stop too, the attempt terminates, the task's stop is
	// confirmed and the hold releases once.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
	if n := f.count(t, "durable_event", "record_kind = 'budget' and event = 'release'"); n != 1 {
		t.Fatalf("releases = %d, want exactly one", n)
	}
}

func TestEventRuntimeStopCompletesCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	task, err := store.GetTask(f.ctx, f.pool, f.tenant, s.Task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	cmd := f.cancelCommand("c-1", s.Task.ID, task.Version)
	initial, err := application.Cancel(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if initial.State != string(lifecycle.TaskCancelling) || initial.AttemptState != string(lifecycle.AttemptCancellationRequested) {
		t.Fatalf("initial view = %+v, want cancelling/cancellation_requested", initial)
	}
	// The runtime stops first this time; the order does not matter.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	stopOperation := f.environmentStopOperation(t, s)
	if got := f.ingestEnvironmentNow(t, f.environmentEvent(s, stopOperation, simulator.RowsFrame(false,
		row(1, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("stop observation = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
	replay, err := application.Cancel(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatalf("replayed Cancel() error = %v", err)
	}
	if replay.State != initial.State || replay.AttemptState != initial.AttemptState {
		t.Fatalf("replay = %+v, want original result %+v", replay, initial)
	}
}

func TestEventStaleEnvironmentStopCannotConvergeCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.ingestEnvironmentNow(t, f.environmentEvent(s, s.Attempt.EnvironmentOperation, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunRunning, Sequence: 10, ObservedAt: f.at,
		}))))
	f.cancel(t, s, "c-1")
	stopOperation := f.environmentStopOperation(t, s)
	stale := f.ingestEnvironmentNow(t, f.environmentEvent(s, stopOperation, simulator.RowsFrame(false,
		row(1, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunAbsent, Sequence: 9, ObservedAt: f.at,
		}))))
	if stale.Progress != 1 || stale.Applied != 0 {
		t.Fatalf("stale stop = %+v, want progress only", stale)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want cancellation still pending", task, attempt)
	}
	if got := f.ingestEnvironmentNow(t, f.environmentEvent(s, stopOperation, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunAbsent, Sequence: 11, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("current stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
	}
}

func TestTerminalAttemptAloneDoesNotProveCancellationStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, _ := f.executing(t, "n-1")
	actor := lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal}
	// Construct an invariant-only split that CancelTask cannot produce: its
	// Task and Attempt cancellation transitions commit atomically. Even in
	// this artificial state, normal completion must not be mistaken for proof
	// that both the runtime and environment stopped.
	f.seed(t, func(uow *store.UnitOfWork) error {
		task, err := store.GetTask(f.ctx, uow, f.tenant, s.Task.ID)
		if err != nil {
			return err
		}
		auditID, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		_, _, err = store.ApplyTask(f.ctx, uow, f.tenant, s.Task.ID, lifecycle.TaskCancel, "",
			store.Transition{ExpectedVersion: task.Version, At: f.at, Actor: actor, AuditID: auditID})
		return err
	})
	// ...and the attempt then completes normally. Completion releases the
	// budget hold, but it cannot stand in for runtime and environment stops.
	f.seed(t, func(uow *store.UnitOfWork) error {
		step := progress.Step{UOW: uow, Tenant: f.tenant, Actor: actor, At: f.at}
		if _, _, _, err := step.Attempt(f.ctx, s.Attempt.ID, lifecycle.AttemptFinalize, ""); err != nil {
			return err
		}
		if _, _, _, err := step.Attempt(f.ctx, s.Attempt.ID, lifecycle.AttemptComplete, ""); err != nil {
			return err
		}
		return nil
	})
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCompleted {
		t.Fatalf("states = %s/%s, want cancelling/completed", task, attempt)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
	if n := f.count(t, "durable_event", "record_kind = 'budget' and event = 'release'"); n != 1 {
		t.Fatalf("releases = %d, want exactly one", n)
	}
}

func TestCancelUsesPreviouslyObservedEnvironmentStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	if got := f.ingestEnvironmentNow(t, f.environmentEvent(s, s.Attempt.EnvironmentOperation, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunHibernated, Sequence: 2, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("pre-cancel environment stop = %+v, want applied observation", got)
	}

	view := f.cancel(t, s, "c-1")
	if view.State != string(lifecycle.TaskCancelling) || view.AttemptState != string(lifecycle.AttemptCancellationRequested) {
		t.Fatalf("cancel view = %+v, want cancellation pending only the runtime stop", view)
	}
	attempt, err := store.GetAttempt(f.ctx, f.pool, f.tenant, s.Attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	if !attempt.EnvironmentStopped || attempt.RuntimeStopped {
		t.Fatalf("stop evidence = runtime:%t environment:%t, want only environment", attempt.RuntimeStopped, attempt.EnvironmentStopped)
	}

	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attemptState := f.states(t, s); task != lifecycle.TaskCancelled || attemptState != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attemptState)
	}
}

// Every way a turn can end proves the runtime stopped once a stop is awaited;
// the runtime never has to report the interruption it was asked for.
func TestEventTurnEndAfterCancelIsRuntimeStop(t *testing.T) {
	t.Parallel()
	for name, finished := range map[string]func(f *fixture) contracts.TurnFinished{
		"completed": func(f *fixture) contracts.TurnFinished {
			return contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: f.at}
		},
		"failed": func(f *fixture) contracts.TurnFinished { return failedTurn("s-1", "t-1", f.at) },
		"interrupted": func(f *fixture) contracts.TurnFinished {
			return contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnInterrupted, EndedAt: f.at}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.executing(t, "n-1")
			f.cancel(t, s, "c-1")
			if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
				row(2, contracts.EntryTurnFinished, finished(f))))); got.Applied != 1 {
				t.Fatalf("turn end = %+v, want applied", got)
			}
			// The two-party stop stands: the environment still has to report.
			if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
				t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
			}
			f.observeEnvironmentStop(t, s, 2)
			if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
				t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
			}
			if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
				t.Fatalf("reservation = %s, want released", got.State)
			}
		})
	}
}

// A turn that ended before the cancellation reported its end while no stop was
// awaited, so the cancellation itself must count it. The interrupt still goes
// out, because the record may lag a turn the runtime has started since.
func TestCancelUsesPreviouslyEndedTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: f.at})))); got.Applied != 1 {
		t.Fatalf("turn end = %+v, want applied", got)
	}
	f.cancel(t, s, "c-1")
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	f.observeEnvironmentStop(t, s, 2)
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
	}
}

// A turn awaiting input has not ended the runtime's work, so it proves no stop,
// whether it arrives after the cancellation or was recorded before it.
func TestAwaitingInputTurnIsNoRuntimeStop(t *testing.T) {
	t.Parallel()
	for name, cancelFirst := range map[string]bool{"after_cancel": true, "before_cancel": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.executing(t, "n-1")
			awaiting := f.event(rt, s, 1, simulator.RowsFrame(false,
				row(2, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnAwaitingInput, EndedAt: f.at})))
			if cancelFirst {
				f.cancel(t, s, "c-1")
			}
			f.ingestNow(t, awaiting)
			if !cancelFirst {
				f.cancel(t, s, "c-1")
			}
			attempt, err := store.GetAttempt(f.ctx, f.pool, f.tenant, s.Attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.RuntimeStopped {
				t.Fatal("a turn awaiting input was accepted as the runtime stop")
			}
		})
	}
}

// A result the runtime finished before its stop was observed stays unadmitted.
func TestResultDuringCancellationStaysInactive(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.cancel(t, g.s, "c-1")
	finished := row(3, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: g.at})
	if got := g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(2, contracts.EntryResultProposed, g.proposal), finished))); got.Applied != 1 || got.Refused != 1 {
		t.Fatalf("late result = %+v, want the proposal refused and the turn end applied", got)
	}
	g.observeEnvironmentStop(t, g.s, 2)
	if task, attempt := g.states(t, g.s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attempt)
	}
	if n := g.count(t, "result_revision", "task_id = $2", g.s.Task.ID.String()); n != 0 {
		t.Fatalf("result revisions = %d, want none", n)
	}
}

// A late running observation is progress only: nothing reverses a termination.
func TestLateRunningObservationCannotReverseTermination(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.cancel(t, s, "c-1")
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	f.observeEnvironmentStop(t, s, 2)
	f.ingestEnvironmentNow(t, f.environmentEvent(s, s.Attempt.EnvironmentOperation, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "environment:" + s.Attempt.ID.String(), State: lifecycle.RunRunning, Sequence: 3, ObservedAt: f.at,
		}))))
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelled || attempt != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want still cancelled/terminated", task, attempt)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
}

func TestReservationReleasesAtAttemptTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	// A failed turn is terminal: the hold releases exactly once.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at))))); got.Applied != 1 {
		t.Fatalf("failed turn = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("states = %s/%s, want failed/failed", task, attempt)
	}
	if got := f.reservation(t, s); got.State != lifecycle.ReservationReleased {
		t.Fatalf("reservation = %s, want released", got.State)
	}
	budget, err := store.GetBudget(f.ctx, f.pool, f.tenant, s.Budget.ID)
	if err != nil || budget.Reserved != 0 {
		t.Fatalf("budget = %+v, %v; want nothing reserved", budget, err)
	}
	if n := f.count(t, "durable_event", "record_kind = 'budget' and event = 'release'"); n != 1 {
		t.Fatalf("releases = %d, want exactly one", n)
	}
	// A duplicate release replays as already-released: the late observation
	// is refused evidence and writes no second release.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryTurnFinished, failedTurn("s-1", "t-2", f.at))))); got.Refused != 1 {
		t.Fatalf("duplicate failure = %+v, want refused", got)
	}
	if n := f.count(t, "durable_event", "record_kind = 'budget' and event = 'release'"); n != 1 {
		t.Fatalf("releases = %d, want still exactly one", n)
	}
	// Late usage after the release records overshoot instead of reserving.
	reservationID, err := ids.Parse[ids.Reservation](s.Attempt.ID.String())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	f.seed(t, func(uow *store.UnitOfWork) error {
		auditID, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		_, _, _, err = store.ReportUsage(f.ctx, uow, f.tenant, s.Budget.ID, reservationID, 1200,
			store.Transition{ExpectedVersion: budget.Version, At: f.at,
				Actor:   lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal},
				AuditID: auditID})
		return err
	})
	budget, err = store.GetBudget(f.ctx, f.pool, f.tenant, s.Budget.ID)
	if err != nil || budget.Consumed != 1200 || budget.Overshoot != 200 {
		t.Fatalf("budget = %+v, %v; want 1200 consumed and 200 overshoot", budget, err)
	}
}

func TestReconcileUnknownOutcomeSupersedesAttention(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	f.dispatchPendingEnvironments(t)
	rt := f.runtime(t, s.Operation.ID)
	// The runtime reports its session while the start_turn delivery is still
	// in flight, so the attempt executes and awaits an answer.
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}))))
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	if _, attempt := f.states(t, s); attempt != lifecycle.AttemptWaitingInput {
		t.Fatalf("attempt = %s, want waiting_input", attempt)
	}
	// The start_turn reply is lost: the plane can no longer know the wait is
	// real, so entering reconciliation supersedes the request.
	lost, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider},
		simulator.RuntimeScenario{Operations: map[ids.OperationID]simulator.RuntimeOp{
			s.Operation.ID: {Reply: simulator.Unknown(&contracts.Failure{
				Code: contracts.FailureProviderFailed, Message: "lost reply",
			}), Effect: simulator.TurnStarted},
		}})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	f.tick(t, lost)
	if got := f.attention(t, s); got.State != lifecycle.AttentionSuperseded {
		t.Fatalf("request = %s, want superseded", got.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskWaitingInput || attempt != lifecycle.AttemptReconciling {
		t.Fatalf("states = %s/%s, want waiting_input/reconciling", task, attempt)
	}
}

// A failed Attempt releases its budget, so the prepared environment gets its
// own stop rather than outliving the work it was prepared for.
func TestFailedRuntimeStopsPreparedEnvironment(t *testing.T) {
	t.Parallel()
	turnFailed := func(t *testing.T, f *fixture) application.Submission {
		t.Helper()
		s, rt := f.executing(t, "failed-runtime")
		f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)))))
		return s
	}
	startRejected := func(t *testing.T, f *fixture) application.Submission {
		t.Helper()
		s := f.submit(t, f.submitCommand("failed-runtime"))
		runtime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{Operations: map[ids.OperationID]simulator.RuntimeOp{
			s.Operation.ID: {Reply: simulator.Rejected(&contracts.Failure{Code: contracts.FailureProviderFailed, Message: "scripted rejection"})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		f.tick(t, runtime)
		return s
	}
	for name, fail := range map[string]func(*testing.T, *fixture) application.Submission{"start rejected": startRejected, "turn failed": turnFailed} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, lifecycle.HarnessLive)
			s := fail(t, f)
			after := f.reload(t, s.Nonce)
			if after.Attempt.State != lifecycle.AttemptFailed || after.Reservation.State != lifecycle.ReservationReleased {
				t.Fatalf("attempt=%s reservation=%s; want failed and released", after.Attempt.State, after.Reservation.State)
			}
			stop := f.environmentStopOperation(t, s)
			f.dispatchPendingEnvironments(t)
			if op, err := store.GetOperation(f.ctx, f.pool, f.tenant, stop); err != nil || op.State != lifecycle.OperationAccepted {
				t.Fatalf("stop operation=%s err=%v; want accepted", op.State, err)
			}
			if stopped, err := store.EnvironmentStopped(f.ctx, f.pool, f.tenant, s.Attempt.ID); err != nil || !stopped {
				t.Fatalf("environment stopped=%v err=%v; want the stop observed on the failed attempt", stopped, err)
			}
		})
	}
}

// An environment stop that expires unsent proves nothing: the provision it
// would stop reached the provider, so the environment may be live even though
// no resource observation has arrived yet.
func TestCancelUnsentEnvironmentStopDoesNotProveStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	environment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{
		Operations: map[ids.OperationID]simulator.EnvironmentOp{
			s.Attempt.EnvironmentOperation: {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned, SendError: io.ErrUnexpectedEOF},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tick := func(cfg worker.Config) {
		t.Helper()
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "test-environment-worker",
			wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.environmentProvider: environment}))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Tick(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	tick(worker.DefaultConfig())
	if environment.Ledger().Effects(s.Attempt.EnvironmentOperation) != 1 {
		t.Fatal("provision did not reach the environment")
	}
	f.cancel(t, s, "c-1")
	stop := f.environmentStopOperation(t, s)
	// Spend the stop's only send without claiming it, so it exhausts unsent.
	if _, err := f.pool.Exec(f.ctx, `update outbox set attempts = 1 where tenant_id = $1 and record_id = $2`, f.tenant.String(), stop.String()); err != nil {
		t.Fatal(err)
	}
	cfg := worker.DefaultConfig()
	cfg.Policy.MaxAttempts = 1
	tick(cfg)
	if op, err := store.GetOperation(f.ctx, f.pool, f.tenant, stop); err != nil || op.State != lifecycle.OperationRejected {
		t.Fatalf("stop operation = %s, %v; want rejected as never sent", op.State, err)
	}
	after := f.reload(t, s.Nonce)
	if after.Attempt.EnvironmentStopped || after.Attempt.State != lifecycle.AttemptCancellationRequested {
		t.Fatalf("attempt = %s environment_stopped=%v; want still awaiting the environment stop", after.Attempt.State, after.Attempt.EnvironmentStopped)
	}
	if after.Reservation.State != lifecycle.ReservationHeld {
		t.Fatalf("reservation = %s, want held while the environment may be live", after.Reservation.State)
	}
}
