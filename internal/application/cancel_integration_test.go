//go:build integration

package application_test

import (
	"encoding/json"
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

// reservation reads the attempt-derived reservation the budget gate expects.
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
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 1, ObservedAt: f.at,
		}))))
	f.cancel(t, s, "c-1")
	// The environment's stop alone is one of two: the attempt keeps waiting.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at,
		})))); got.Applied != 1 {
		t.Fatalf("stop observation = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	// With the runtime stop too, the attempt terminates, the task's stop is
	// confirmed and the hold releases once.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(4, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
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
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 1, ObservedAt: f.at,
		}))))
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
		row(3, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(4, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at,
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
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 10, ObservedAt: f.at,
		}))))
	f.cancel(t, s, "c-1")
	stale := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunAbsent, Sequence: 9, ObservedAt: f.at,
		}))))
	if stale.Progress != 1 || stale.Applied != 0 {
		t.Fatalf("stale stop = %+v, want progress only", stale)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(4, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want cancellation still pending", task, attempt)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(5, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunAbsent, Sequence: 11, ObservedAt: f.at,
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
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{
			ResourceRef: "box-1", State: lifecycle.RunHibernated, Sequence: 1, ObservedAt: f.at,
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
		row(3, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))); got.Applied != 1 {
		t.Fatalf("runtime stop = %+v, want applied", got)
	}
	if task, attemptState := f.states(t, s); task != lifecycle.TaskCancelled || attemptState != lifecycle.AttemptTerminated {
		t.Fatalf("states = %s/%s, want cancelled/terminated", task, attemptState)
	}
}

func TestEventFailedTurnAfterCancelRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.cancel(t, s, "c-1")
	// A failure reported after the stop was requested is a late observation:
	// it is recorded as evidence and never overrides the cancellation.
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at))))); got.Applied != 0 || got.Refused != 1 {
		t.Fatalf("late failure = %+v, want a refusal", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("states = %s/%s, want still cancelling", task, attempt)
	}
	if n := f.count(t, "audit_event", "event = 'fail' and record_id = $2 and outcome = 'rejected'", s.Attempt.ID.String()); n != 1 {
		t.Fatalf("late-failure evidence = %d, want 1", n)
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
