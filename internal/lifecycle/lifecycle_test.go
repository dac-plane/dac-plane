package lifecycle

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var (
	testNow        = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	testDigest     = identity.Digest("sha256:" + strings.Repeat("a", 64))
	testSystem     = Actor{Kind: ActorSystem}
	testGeneration = uint64(3)
	testRejectCode = func(t *testing.T, err error, want RejectionCode) {
		t.Helper()
		if got := RejectionOf(err); got != want {
			t.Fatalf("error = %v, want rejection %s", err, want)
		}
	}
)

func newID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func newTask(t *testing.T) Task {
	t.Helper()
	task, err := NewTask(newID[ids.Task](t), newID[ids.Tenant](t), newID[ids.Principal](t), testNow)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	return task
}

func newAttempt(t *testing.T) Attempt {
	t.Helper()
	attempt, err := NewAttempt(newID[ids.Attempt](t), newID[ids.Tenant](t), newID[ids.Task](t), 1, testNow)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	return attempt
}

// drive applies events in order, each at the version the previous one produced.
func driveTask(t *testing.T, task Task, steps ...any) Task {
	t.Helper()
	for i := 0; i < len(steps); i += 2 {
		event, target := steps[i].(TaskEvent), steps[i+1].(TaskState)
		next, change, err := task.Apply(event, task.Version, target, testNow)
		if err != nil {
			t.Fatalf("%s from %s: %v", event, task.State, err)
		}
		if next.Version != task.Version+1 || change.Version != next.Version || change.To != string(next.State) {
			t.Fatalf("%s: version %d -> %d, change %+v", event, task.Version, next.Version, change)
		}
		task = next
	}
	return task
}

func driveAttempt(t *testing.T, attempt Attempt, events ...AttemptEvent) Attempt {
	t.Helper()
	for _, event := range events {
		next, _, err := attempt.Apply(event, attempt.Version, "", testNow)
		if err != nil {
			t.Fatalf("%s from %s: %v", event, attempt.State, err)
		}
		attempt = next
	}
	return attempt
}

func TestTaskHappyPathAndReviewOutcomes(t *testing.T) {
	ready := driveTask(t, newTask(t),
		TaskSubmit, TaskState(""),
		TaskDispatch, TaskState(""),
		TaskAwaitInput, TaskState(""),
		TaskInputResolved, TaskState(""),
		TaskProposeResult, TaskState(""),
		TaskVerificationPassed, TaskState(""),
	)
	if ready.State != TaskReadyForReview || ready.Version != 7 {
		t.Fatalf("task = %s v%d, want ready_for_review v7", ready.State, ready.Version)
	}
	if got := driveTask(t, ready, TaskAccept, TaskState("")); got.State != TaskSucceeded {
		t.Fatalf("accept = %s", got.State)
	}
	if got := driveTask(t, ready, TaskReject, TaskState("")); got.State != TaskRejected {
		t.Fatalf("reject = %s", got.State)
	}
	if got := driveTask(t, ready, TaskRequestChanges, TaskState("")); got.State != TaskQueued {
		t.Fatalf("request_changes = %s", got.State)
	}
}

func TestTaskSucceedsOnlyThroughReviewAcceptance(t *testing.T) {
	for from, transitions := range taskMachine.table {
		for event, targets := range transitions {
			if slices.Contains(targets, TaskSucceeded) && (from != TaskReadyForReview || event != TaskAccept) {
				t.Errorf("%s --%s--> succeeded: only review acceptance may succeed", from, event)
			}
		}
	}
}

func TestTaskApplyRejectsStaleVersionWithoutChange(t *testing.T) {
	task := driveTask(t, newTask(t), TaskSubmit, TaskState(""))
	next, change, err := task.Apply(TaskDispatch, task.Version-1, "", testNow)
	testRejectCode(t, err, RejectVersionConflict)
	if next != task {
		t.Fatal("rejected Apply() changed the task")
	}
	if change.To != "" || change.Version != task.Version {
		t.Fatalf("rejected change = %+v", change)
	}
}

func TestLateResultOnCancellingTaskIsRejected(t *testing.T) {
	task := driveTask(t, newTask(t), TaskSubmit, TaskState(""), TaskDispatch, TaskState(""), TaskCancel, TaskState(""))
	_, _, err := task.Apply(TaskProposeResult, task.Version, "", testNow)
	testRejectCode(t, err, RejectInvalidTransition)
	if got := driveTask(t, task, TaskStopConfirmed, TaskState("")); got.State != TaskCancelled {
		t.Fatalf("stop_confirmed = %s", got.State)
	}
}

func TestAttemptTerminatesOnlyAfterBothStops(t *testing.T) {
	for name, order := range map[string][2]AttemptEvent{
		"runtime first":     {AttemptRuntimeStopped, AttemptEnvironmentStopped},
		"environment first": {AttemptEnvironmentStopped, AttemptRuntimeStopped},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := driveAttempt(t, newAttempt(t), AttemptProvision, AttemptStart, AttemptRuntimeStarted, AttemptRequestCancel)
			first := driveAttempt(t, attempt, order[0])
			if first.State != AttemptCancellationRequested {
				t.Fatalf("after one stop = %s, want cancellation_requested", first.State)
			}
			if err := first.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}

			_, _, err := first.Apply(order[0], first.Version, "", testNow)
			testRejectCode(t, err, RejectStopAlreadyObserved)
			_, _, err = first.Apply(order[1], first.Version, AttemptCancellationRequested, testNow)
			testRejectCode(t, err, RejectInvalidTarget)

			done := driveAttempt(t, first, order[1])
			if done.State != AttemptTerminated || !done.RuntimeStopped || !done.EnvironmentStopped {
				t.Fatalf("after both stops = %+v, want terminated with both observed", done)
			}
		})
	}
}

func TestAttemptStopObservationOutsideCancellationIsRejected(t *testing.T) {
	attempt := driveAttempt(t, newAttempt(t), AttemptProvision, AttemptStart, AttemptRuntimeStarted)
	_, _, err := attempt.Apply(AttemptRuntimeStopped, attempt.Version, "", testNow)
	testRejectCode(t, err, RejectInvalidTransition)
}

func TestAttemptStopWithStaleVersionIsVersionConflict(t *testing.T) {
	attempt := driveAttempt(t, newAttempt(t), AttemptProvision, AttemptRequestCancel)
	_, _, err := attempt.Apply(AttemptRuntimeStopped, attempt.Version-1, "", testNow)
	testRejectCode(t, err, RejectVersionConflict)
}

func TestAttemptUnknownOutcomeEntersReconciliation(t *testing.T) {
	for _, event := range []AttemptEvent{AttemptOutcomeUnknown, AttemptLeaseExpired} {
		attempt := driveAttempt(t, newAttempt(t), AttemptProvision, AttemptStart, AttemptRuntimeStarted, event)
		if attempt.State != AttemptReconciling {
			t.Fatalf("%s = %s, want reconciling", event, attempt.State)
		}
		_, _, err := attempt.Apply(AttemptReconciled, attempt.Version, "", testNow)
		testRejectCode(t, err, RejectAmbiguousTarget)
		resolved, _, err := attempt.Apply(AttemptReconciled, attempt.Version, AttemptExecuting, testNow)
		if err != nil || resolved.State != AttemptExecuting {
			t.Fatalf("reconciled to executing = %s, %v", resolved.State, err)
		}
	}
}

func TestAttemptCancelBeforeProvisioningTerminatesImmediately(t *testing.T) {
	attempt := driveAttempt(t, newAttempt(t), AttemptRequestCancel)
	if attempt.State != AttemptTerminated {
		t.Fatalf("cancel from allocated = %s, want terminated", attempt.State)
	}
}

func TestRecordValidation(t *testing.T) {
	task := newTask(t)
	attempt := newAttempt(t)
	for name, err := range map[string]error{
		"task zero":             Task{}.Validate(),
		"task unknown state":    func() error { x := task; x.State = "paused"; return x.Validate() }(),
		"task updated early":    func() error { x := task; x.UpdatedAt = x.CreatedAt.Add(-1); return x.Validate() }(),
		"attempt zero number":   func() error { x := attempt; x.Number = 0; return x.Validate() }(),
		"attempt stray stop":    func() error { x := attempt; x.RuntimeStopped = true; return x.Validate() }(),
		"attempt unknown state": func() error { x := attempt; x.State = "lost"; return x.Validate() }(),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAuditOfAppliedAndRejectedChanges(t *testing.T) {
	task := newTask(t)
	principal := Actor{Kind: ActorPrincipal, Principal: task.Owner}
	submitted, change, err := task.Apply(TaskSubmit, task.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	applied := change.Audit(newID[ids.Audit](t), principal, testNow)
	if err := applied.Validate(); err != nil {
		t.Fatalf("applied audit Validate() error = %v", err)
	}
	if applied.Outcome != AuditApplied || applied.From != "draft" || applied.To != "queued" || applied.Version != submitted.Version {
		t.Fatalf("applied audit = %+v", applied)
	}

	provider := Actor{Kind: ActorProvider, Provider: newID[ids.Provider](t)}
	_, _, err = submitted.Apply(TaskProposeResult, submitted.Version, "", testNow)
	var transitionErr *TransitionError
	if !errors.As(err, &transitionErr) {
		t.Fatalf("Apply() error = %v, want TransitionError", err)
	}
	rejected := transitionErr.Audit(newID[ids.Audit](t), provider, testNow)
	if err := rejected.Validate(); err != nil {
		t.Fatalf("rejected audit Validate() error = %v", err)
	}
	if rejected.Outcome != AuditRejected || rejected.Reason != RejectInvalidTransition || rejected.To != "" ||
		rejected.Version != submitted.Version {
		t.Fatalf("rejected audit = %+v", rejected)
	}
}

func TestActorValidate(t *testing.T) {
	principal, provider := newID[ids.Principal](t), newID[ids.Provider](t)
	valid := []Actor{
		{Kind: ActorPrincipal, Principal: principal},
		{Kind: ActorProvider, Provider: provider},
		testSystem,
	}
	for _, actor := range valid {
		if err := actor.Validate(); err != nil {
			t.Errorf("%+v Validate() error = %v", actor, err)
		}
	}
	invalid := []Actor{
		{Kind: ActorPrincipal},
		{Kind: ActorPrincipal, Principal: principal, Provider: provider},
		{Kind: ActorProvider, Principal: principal},
		{Kind: ActorSystem, Principal: principal},
		{Kind: "agent"},
	}
	for _, actor := range invalid {
		if err := actor.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v Validate() error = %v, want ErrInvalid", actor, err)
		}
	}
}
