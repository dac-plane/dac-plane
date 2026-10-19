package lifecycle

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

func newBinding(t *testing.T) Binding {
	t.Helper()
	return Binding{
		Tenant:    newID[ids.Tenant](t),
		Attempt:   newID[ids.Attempt](t),
		Provider:  newID[ids.Provider](t),
		Operation: newID[ids.Operation](t),
		Kind:      ResourceRuntimeSession,
		NativeRef: "session-1",
		BoundAt:   testNow,
	}
}

func TestCheckBinding(t *testing.T) {
	bound := newBinding(t)
	if err := CheckBinding(nil, bound); err != nil {
		t.Fatalf("first binding: %v", err)
	}

	replay := bound
	replay.Operation = newID[ids.Operation](t)
	if err := CheckBinding([]Binding{bound}, replay); err != nil {
		t.Fatalf("rebinding to the same attempt: %v", err)
	}

	stolen := bound
	stolen.Attempt = newID[ids.Attempt](t)
	if err := CheckBinding([]Binding{bound}, stolen); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("binding to another attempt: %v, want ErrBindingConflict", err)
	}

	for name, other := range map[string]func(b *Binding){
		"other provider": func(b *Binding) { b.Provider = newID[ids.Provider](t) },
		"other tenant":   func(b *Binding) { b.Tenant = newID[ids.Tenant](t) },
		"other kind":     func(b *Binding) { b.Kind = ResourceEnvironmentResource },
		"other ref":      func(b *Binding) { b.NativeRef = "session-2" },
	} {
		candidate := stolen
		other(&candidate)
		if err := CheckBinding([]Binding{bound}, candidate); err != nil {
			t.Errorf("%s: %v, want no conflict", name, err)
		}
	}
}

func TestBindingValidate(t *testing.T) {
	valid := newBinding(t)
	turn := valid
	turn.Kind, turn.NativeRef, turn.SessionRef = ResourceRuntimeTurn, "turn-1", "session-1"
	if err := turn.Validate(); err != nil {
		t.Fatalf("turn binding: %v", err)
	}
	for name, mutate := range map[string]func(b *Binding){
		"missing operation":    func(b *Binding) { b.Operation = ids.OperationID{} },
		"unknown kind":         func(b *Binding) { b.Kind = "task" },
		"empty native ref":     func(b *Binding) { b.NativeRef = "" },
		"oversized native ref": func(b *Binding) { b.NativeRef = strings.Repeat("x", maxNativeRefLength+1) },
		"session with session": func(b *Binding) { b.SessionRef = "session-0" },
		"turn without session": func(b *Binding) { b.Kind = ResourceRuntimeTurn },
	} {
		b := valid
		mutate(&b)
		if err := b.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
}

func newOperation(t *testing.T) ProviderOperation {
	t.Helper()
	operation, err := NewProviderOperation(newID[ids.Operation](t), newID[ids.Tenant](t), newID[ids.Attempt](t), newID[ids.Provider](t), testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	return operation
}

func applyOperation(t *testing.T, operation ProviderOperation, event OperationEvent, target OperationState) ProviderOperation {
	t.Helper()
	next, change, err := operation.Apply(event, operation.Version, target, testNow)
	if err != nil {
		t.Fatalf("%s from %s: %v", event, operation.State, err)
	}
	if change.Version != next.Version || next.Version != operation.Version+1 {
		t.Fatalf("%s: version %d -> %d", event, operation.Version, next.Version)
	}
	return next
}

func TestSendStatusDrivesOperation(t *testing.T) {
	for status, want := range map[SendStatus]OperationState{
		SendAccepted: OperationAccepted,
		SendRejected: OperationRejected,
		SendUnknown:  OperationUncertain,
	} {
		dispatched := applyOperation(t, newOperation(t), OperationDispatch, "")
		if got := applyOperation(t, dispatched, status.Event(), "").State; got != want {
			t.Errorf("send %s = %s, want %s", status, got, want)
		}
	}
	if event := SendPending.Event(); event != "" {
		t.Fatalf("pending maps to %q, want no event", event)
	}
}

func TestUncertainOperationIsReconciledNeverResent(t *testing.T) {
	for _, state := range reachable(operationMachine, OperationUncertain) {
		if state == OperationRecorded || state == OperationDispatched {
			t.Errorf("uncertain can reach %s", state)
		}
	}
	uncertain := applyOperation(t, applyOperation(t, newOperation(t), OperationDispatch, ""), OperationSendUnknown, "")
	_, _, err := uncertain.Apply(OperationDispatch, uncertain.Version, "", testNow)
	testRejectCode(t, err, RejectInvalidTransition)
	if got := applyOperation(t, uncertain, OperationReconcile, OperationSucceeded).State; got != OperationSucceeded {
		t.Fatalf("reconcile = %s", got)
	}
}

func TestTurnEndValidate(t *testing.T) {
	end := TurnEnd{
		Tenant: newID[ids.Tenant](t), Attempt: newID[ids.Attempt](t), Provider: newID[ids.Provider](t),
		SessionRef: "session-1", TurnRef: "turn-1", Outcome: TurnAwaitingInput, EndedAt: testNow,
	}
	if err := end.Validate(); err != nil {
		t.Fatalf("awaiting input: %v", err)
	}
	failed := end
	failed.Outcome = TurnFailed
	failed.Failure = &TurnFailure{Code: TurnFailureRateLimited, Summary: "rate limited", DetailsDigest: testDigest}
	if err := failed.Validate(); err != nil {
		t.Fatalf("failed turn: %v", err)
	}
	for name, mutate := range map[string]func(e *TurnEnd){
		"failed without failure": func(e *TurnEnd) { e.Failure = nil },
		"failure on completed":   func(e *TurnEnd) { e.Outcome = TurnCompleted },
		"unknown failure code":   func(e *TurnEnd) { e.Failure = &TurnFailure{Code: "oom", Summary: "x"} },
		"empty summary":          func(e *TurnEnd) { e.Failure = &TurnFailure{Code: TurnFailureTimeout} },
		"oversized summary": func(e *TurnEnd) {
			e.Failure = &TurnFailure{Code: TurnFailureTimeout, Summary: strings.Repeat("x", maxFailureSummaryLength+1)}
		},
		"malformed details digest": func(e *TurnEnd) {
			e.Failure = &TurnFailure{Code: TurnFailureTimeout, Summary: "x", DetailsDigest: "raw"}
		},
		"unknown outcome":  func(e *TurnEnd) { e.Outcome = "crashed"; e.Failure = nil },
		"missing turn ref": func(e *TurnEnd) { e.TurnRef = "" },
	} {
		e := failed
		failure := *failed.Failure
		e.Failure = &failure
		mutate(&e)
		if err := e.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
}

func TestEnvironmentObservationOrdering(t *testing.T) {
	first := EnvironmentObservation{
		Tenant: newID[ids.Tenant](t), Attempt: newID[ids.Attempt](t), Provider: newID[ids.Provider](t),
		ResourceRef: "box-1", State: RunRunning, Sequence: 5, ObservedAt: testNow,
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	newer, replay, other := first, first, first
	newer.Sequence, newer.State = 6, RunAbsent
	replay.State = RunAbsent
	other.ResourceRef, other.Sequence = "box-2", 9
	if !newer.Supersedes(first) || replay.Supersedes(first) || first.Supersedes(newer) || other.Supersedes(first) {
		t.Fatal("Supersedes() must accept only a newer sequence of the same resource")
	}
	for state, stopped := range map[RunState]bool{RunAbsent: true, RunHibernated: true, RunRunning: false, RunStarting: false, RunUnknown: false} {
		observation := first
		observation.State = state
		if observation.Stopped() != stopped {
			t.Errorf("%s Stopped() = %v, want %v", state, !stopped, stopped)
		}
	}
	zero := first
	zero.Sequence = 0
	if err := zero.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero sequence Validate() = %v, want ErrInvalid", err)
	}
}

func TestHarnessPromotionIsStepwiseAndPrincipalOnly(t *testing.T) {
	principal := Actor{Kind: ActorPrincipal, Principal: newID[ids.Principal](t)}
	harness := HarnessQualification{Tenant: newID[ids.Tenant](t), Provider: newID[ids.Provider](t), Mode: HarnessOff, Version: 1, UpdatedAt: testNow}

	_, _, err := harness.Apply(HarnessPromote, harness.Version, testSystem, testNow)
	testRejectCode(t, err, RejectUnauthorizedActor)

	shadow, _, err := harness.Apply(HarnessPromote, harness.Version, principal, testNow)
	if err != nil || shadow.Mode != HarnessShadow || shadow.Mode.Authoritative() {
		t.Fatalf("promote from off = %s, %v; shadow must not be authoritative", shadow.Mode, err)
	}
	live, _, err := shadow.Apply(HarnessPromote, shadow.Version, principal, testNow)
	if err != nil || live.Mode != HarnessLive || !live.Mode.Authoritative() {
		t.Fatalf("promote from shadow = %s, %v", live.Mode, err)
	}
	if off, _, err := live.Apply(HarnessDisable, live.Version, testSystem, testNow); err != nil || off.Mode != HarnessOff {
		t.Fatalf("system disable = %s, %v", off.Mode, err)
	}
}

func newMigration(t *testing.T) MigrationPass {
	t.Helper()
	pass, err := NewMigrationPass(newID[ids.Migration](t), newID[ids.Tenant](t), newID[ids.Provider](t), newID[ids.Provider](t), testNow)
	if err != nil {
		t.Fatalf("NewMigrationPass() error = %v", err)
	}
	return pass
}

func TestMigrationExecutesOnlyAfterFreshPreview(t *testing.T) {
	principal := Actor{Kind: ActorPrincipal, Principal: newID[ids.Principal](t)}
	step := func(pass MigrationPass, event MigrationEvent, actor Actor) (MigrationPass, error) {
		next, _, err := pass.Apply(event, pass.Version, actor, testNow)
		return next, err
	}

	planned := newMigration(t)
	_, err := step(planned, MigrationExecute, principal)
	testRejectCode(t, err, RejectInvalidTransition)

	previewed, err := step(planned, MigrationPreview, principal)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	held, _ := step(previewed, MigrationHold, principal)
	released, err := step(held, MigrationRelease, principal)
	if err != nil || released.State != MigrationPlanned {
		t.Fatalf("release = %s, %v; want planned", released.State, err)
	}
	_, err = step(released, MigrationExecute, principal)
	testRejectCode(t, err, RejectInvalidTransition)

	_, err = step(previewed, MigrationExecute, testSystem)
	testRejectCode(t, err, RejectUnauthorizedActor)
	executing, err := step(previewed, MigrationExecute, principal)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_, err = step(executing, MigrationComplete, principal)
	testRejectCode(t, err, RejectUnauthorizedActor)
	if done, err := step(executing, MigrationComplete, testSystem); err != nil || done.State != MigrationCompleted {
		t.Fatalf("complete = %s, %v", done.State, err)
	}
}

func TestMigrationPassMustChangeHarness(t *testing.T) {
	provider := newID[ids.Provider](t)
	if _, err := NewMigrationPass(newID[ids.Migration](t), newID[ids.Tenant](t), provider, provider, testNow); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewMigrationPass(same harness) = %v, want ErrInvalid", err)
	}
}

func newClaim(t *testing.T) MaintenanceClaim {
	t.Helper()
	claim, err := NewMaintenanceClaim(newID[ids.Claim](t), newID[ids.Tenant](t), newID[ids.Provider](t), "box-1", testNow)
	if err != nil {
		t.Fatalf("NewMaintenanceClaim() error = %v", err)
	}
	return claim
}

func TestMaintenanceClaimFencesStaleHolder(t *testing.T) {
	const ttl = time.Minute
	a, change, err := newClaim(t).Claim("worker-a", ttl, testNow)
	if err != nil || a.Epoch != 1 || a.State != ClaimClaimed {
		t.Fatalf("Claim() = %+v, %v", a, err)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if audit := change.Audit(newID[ids.Audit](t), testSystem, testNow); audit.Validate() != nil {
		t.Fatalf("claim audit invalid: %v", audit.Validate())
	}

	_, _, err = a.Claim("worker-b", ttl, testNow.Add(ttl/2))
	testRejectCode(t, err, RejectClaimHeld)

	afterExpiry := testNow.Add(ttl)
	b, _, err := a.Claim("worker-b", ttl, afterExpiry)
	if err != nil || b.Epoch != 2 || b.Claimant != "worker-b" {
		t.Fatalf("takeover = %+v, %v", b, err)
	}

	for name, call := range map[string]func() error{
		"stale complete": func() error { _, _, err := b.Complete("worker-a", 1, afterExpiry); return err },
		"stale renew":    func() error { _, _, err := b.Renew("worker-a", 1, ttl, afterExpiry); return err },
		"stale release":  func() error { _, _, err := b.Release("worker-a", 1, afterExpiry); return err },
		"wrong epoch":    func() error { _, _, err := b.Complete("worker-b", 1, afterExpiry); return err },
		"expired holder": func() error { _, _, err := b.Complete("worker-b", 2, afterExpiry.Add(ttl)); return err },
	} {
		if code := RejectionOf(call()); code != RejectStaleClaim {
			t.Errorf("%s: rejection = %q, want stale_claim", name, code)
		}
	}

	renewed, _, err := b.Renew("worker-b", 2, ttl, afterExpiry.Add(ttl/2))
	if err != nil || !renewed.ExpiresAt.Equal(afterExpiry.Add(ttl/2+ttl)) {
		t.Fatalf("Renew() = %v, %v", renewed.ExpiresAt, err)
	}
	done, _, err := renewed.Complete("worker-b", 2, afterExpiry.Add(ttl))
	if err != nil || done.State != ClaimCompleted || done.Claimant != "" {
		t.Fatalf("Complete() = %+v, %v", done, err)
	}
	_, _, err = done.Claim("worker-c", ttl, afterExpiry.Add(2*ttl))
	testRejectCode(t, err, RejectTerminalState)
}

func TestMaintenanceClaimRelease(t *testing.T) {
	claimed, _, _ := newClaim(t).Claim("worker-a", time.Minute, testNow)
	released, _, err := claimed.Release("worker-a", claimed.Epoch, testNow)
	if err != nil || released.State != ClaimScheduled || released.Claimant != "" {
		t.Fatalf("Release() = %+v, %v", released, err)
	}
	again, _, err := released.Claim("worker-b", time.Minute, testNow)
	if err != nil || again.Epoch != 2 {
		t.Fatalf("reclaim after release = %+v, %v; want epoch 2", again, err)
	}
	_, _, err = released.Complete("worker-a", 1, testNow)
	testRejectCode(t, err, RejectInvalidTransition)
}

func TestNewRecordKindsAreAuditable(t *testing.T) {
	operation := newOperation(t)
	_, change, err := operation.Apply(OperationDispatch, operation.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := change.Audit(newID[ids.Audit](t), testSystem, testNow).Validate(); err != nil {
		t.Fatalf("operation audit: %v", err)
	}
	for _, kind := range []RecordKind{RecordOperation, RecordHarness, RecordMigration, RecordClaim} {
		if !slices.Contains(RecordKinds(), kind) {
			t.Errorf("RecordKinds() missing %s", kind)
		}
	}
}

func TestAttemptFollowsOperation(t *testing.T) {
	for _, tc := range []struct {
		operation OperationState
		attempt   AttemptState
		event     AttemptEvent
		target    AttemptState
		ok        bool
	}{
		{OperationUncertain, AttemptExecuting, AttemptOutcomeUnknown, AttemptReconciling, true},
		{OperationUncertain, AttemptProvisioning, AttemptOutcomeUnknown, AttemptReconciling, true},
		{OperationUncertain, AttemptAllocated, "", "", false},
		{OperationUncertain, AttemptCancellationRequested, "", "", false},
		{OperationUncertain, AttemptReconciling, "", "", false},
		{OperationSucceeded, AttemptReconciling, AttemptReconciled, AttemptExecuting, true},
		{OperationAccepted, AttemptReconciling, AttemptReconciled, AttemptExecuting, true},
		{OperationFailed, AttemptReconciling, AttemptReconciled, AttemptFailed, true},
		{OperationRejected, AttemptReconciling, AttemptReconciled, AttemptFailed, true},
		{OperationSucceeded, AttemptExecuting, "", "", false},
		{OperationRejected, AttemptProvisioning, AttemptFail, AttemptFailed, true},
		{OperationFailed, AttemptExecuting, AttemptFail, AttemptFailed, true},
		{OperationRejected, AttemptAllocated, "", "", false},
		{OperationRejected, AttemptFailed, "", "", false},
	} {
		event, target, ok := AttemptFollowsOperation(tc.operation, tc.attempt)
		if event != tc.event || target != tc.target || ok != tc.ok {
			t.Errorf("AttemptFollowsOperation(%s, %s) = %s, %s, %v", tc.operation, tc.attempt, event, target, ok)
		}
		if ok {
			if !slices.Contains(attemptMachine.targets(tc.attempt, event), target) {
				t.Errorf("%s --%s--> %s is not a legal transition", tc.attempt, event, target)
			}
		}
	}
}

func TestTaskFollowsAttempt(t *testing.T) {
	if event, ok := TaskFollowsAttempt(AttemptFailed, TaskRunning); !ok || event != TaskExecutionFailed {
		t.Fatalf("failed attempt on running task = %s, %v", event, ok)
	}
	for _, task := range []TaskState{TaskQueued, TaskWaitingInput, TaskFailed} {
		if _, ok := TaskFollowsAttempt(AttemptFailed, task); ok {
			t.Errorf("failed attempt moves a %s task", task)
		}
	}
	// Cancellation is intent until both stops are observed or reconciled,
	// which is represented by the terminated Attempt state alone.
	if event, ok := TaskFollowsAttempt(AttemptTerminated, TaskCancelling); !ok || event != TaskStopConfirmed {
		t.Errorf("terminated attempt on a cancelling task = %s, %v", event, ok)
	}
	for _, terminal := range []AttemptState{AttemptCompleted, AttemptFailed} {
		if _, ok := TaskFollowsAttempt(terminal, TaskCancelling); ok {
			t.Errorf("terminal %s confirms cancellation without both stop proofs", terminal)
		}
	}
	if _, ok := TaskFollowsAttempt(AttemptExecuting, TaskCancelling); ok {
		t.Error("a live attempt confirms its task's stop")
	}
	if _, ok := TaskFollowsAttempt(AttemptExecuting, TaskRunning); ok {
		t.Fatal("an executing attempt moves its task")
	}
}
