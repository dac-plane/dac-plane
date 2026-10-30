//go:build integration

package application_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

const eventStream = "events"

// live submits a task and dispatches it, so its operation is accepted and
// the provider may report on it.
func (f *fixture) live(t *testing.T, nonce string) (application.Submission, *simulator.Runtime) {
	t.Helper()
	s := f.submit(t, f.submitCommand(nonce))
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	return s, rt
}

func (f *fixture) peer() contracts.Peer {
	return contracts.Peer{Tenant: f.tenant, Provider: f.provider}
}

// event is the provider's claim for frame on the submission's event stream.
func (f *fixture) event(rt *simulator.Runtime, s application.Submission, generation uint64, frame contracts.StreamFrame) contracts.Event {
	return rt.Event(s.Attempt.ID, s.Operation.ID, eventStream, generation, frame)
}

func (f *fixture) environmentEvent(s application.Submission, operation ids.OperationID, frame contracts.StreamFrame) contracts.Event {
	return contracts.Event{Tenant: f.tenant, Attempt: s.Attempt.ID, Provider: f.environmentProvider,
		Operation: operation, Stream: eventStream, Generation: 1, Body: frame}
}

func (f *fixture) ingestEnvironmentNow(t *testing.T, event contracts.Event) application.Ingestion {
	t.Helper()
	result, err := application.Ingest(f.ctx, f.pool, contracts.Peer{Tenant: f.tenant, Provider: f.environmentProvider}, event, f.at)
	if err != nil {
		t.Fatalf("environment Ingest() error = %v", err)
	}
	return result
}

func (f *fixture) ingest(t *testing.T, event contracts.Event) application.Ingestion {
	t.Helper()
	result, err := application.Ingest(f.ctx, f.pool, f.peer(), event, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	return result
}

// states reads the Task and Attempt of a submission.
func (f *fixture) states(t *testing.T, s application.Submission) (lifecycle.TaskState, lifecycle.AttemptState) {
	t.Helper()
	task, err := store.GetTask(f.ctx, f.pool, f.tenant, s.Task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	attempt, err := store.GetAttempt(f.ctx, f.pool, f.tenant, s.Attempt.ID)
	if err != nil {
		t.Fatalf("GetAttempt() error = %v", err)
	}
	return task.State, attempt.State
}

func (f *fixture) count(t *testing.T, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `select count(*) from `+table+` where tenant_id = $1 and `+where,
		append([]any{f.tenant.String()}, args...)...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func row(sequence uint64, kind string, payload any) simulator.Row {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return simulator.Row{Sequence: sequence, Updated: sequence, Kind: kind, Payload: string(data)}
}

func failedTurn(session, turn string, at time.Time) contracts.TurnFinished {
	return contracts.TurnFinished{
		SessionRef: session, TurnRef: turn, Outcome: lifecycle.TurnFailed, EndedAt: at,
		Failure: &lifecycle.TurnFailure{Code: lifecycle.TurnFailureToolFailed, Summary: "tool exited 1", DetailsDigest: identity.DigestOf([]byte("trace"))},
	}
}

func TestEventRuntimeStartBindsSessionAndExecutes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	f.ingest(t, f.event(rt, s, 1, simulator.ConnectedFrame(f.at)))
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnStarted, contracts.TurnStarted{NativeRef: "t-1", SessionRef: "s-1"}),
		row(3, "message.delta", map[string]string{"text": "working"}),
		row(4, "custom.metric", map[string]int{"tokens": 3}),
	)
	got := f.ingest(t, f.event(rt, s, 1, frame))
	if got.Applied != 2 || got.Progress != 2 || got.Refused != 0 || got.Cursor.After != 4 {
		t.Fatalf("ingestion = %+v, want 2 applied, 2 progress, cursor 4", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("states = %s/%s, want running/executing", task, attempt)
	}
	if n := f.count(t, "binding", "attempt_id = $2 and operation_id = $3", s.Attempt.ID.String(), s.Operation.ID.String()); n != 2 {
		t.Fatalf("bindings = %d, want session and turn", n)
	}

	replay := f.ingest(t, f.event(rt, s, 1, frame))
	if replay.Duplicates != 4 || replay.Applied != 0 || replay.Cursor.After != 4 {
		t.Fatalf("duplicate frame = %+v, want 4 duplicates and nothing applied", replay)
	}
	f.ingest(t, f.event(rt, s, 1, simulator.HeartbeatFrame(f.at)))
	// provision at enqueue, then start and runtime_started; nothing from the replay.
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'applied'", s.Attempt.ID.String()); n != 3 {
		t.Fatalf("attempt transitions = %d, want 3", n)
	}
}

func TestEventTurnFinishedRecordsTurnEnds(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	ended := f.at.Add(30 * time.Second)
	got := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: ended}),
	)))
	if got.Applied != 2 {
		t.Fatalf("ingestion = %+v, want both applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("completed turn changed state: %s/%s", task, attempt)
	}

	// The failed turn was never started: it is bound from the event's operation.
	f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(3, contracts.EntryTurnFinished, failedTurn("s-1", "t-2", ended)))))
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("failed turn: states = %s/%s, want failed/failed", task, attempt)
	}
	end, err := store.GetTurnEnd(f.ctx, f.pool, f.tenant, f.provider, "s-1", "t-2")
	if err != nil {
		t.Fatalf("GetTurnEnd() error = %v", err)
	}
	want := failedTurn("s-1", "t-2", ended).Failure
	if end.Failure == nil || *end.Failure != *want || end.Attempt != s.Attempt.ID {
		t.Fatalf("turn end = %+v, want the failure preserved", end)
	}
	if n := f.count(t, "binding", "kind = 'runtime_turn' and native_ref = 't-2' and operation_id = $2", s.Operation.ID.String()); n != 1 {
		t.Fatal("failed turn is not bound to its operation")
	}

	late := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(4, contracts.EntryTurnFinished, failedTurn("s-1", "t-3", ended)))))
	if late.Refused != 1 {
		t.Fatalf("late failure = %+v, want refused", late)
	}
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'rejected' and event = 'fail'", s.Attempt.ID.String()); n != 1 {
		t.Fatalf("late failure evidence = %d, want 1", n)
	}
	if n := f.count(t, "turn_end", "true"); n != 3 {
		t.Fatalf("turn ends = %d, want 3", n)
	}
}

func TestEventRefusesUnknownStateChangingKinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	got := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, "attempt.completed", map[string]string{}),
		row(2, "tool.approved", map[string]string{}),
		row(3, contracts.EntryRuntimeStarted, map[string]string{"native_ref": "s-1", "attempt_id": s.Attempt.ID.String()}),
	)))
	if got.Refused != 3 || got.Applied != 0 || got.Cursor.After != 3 {
		t.Fatalf("ingestion = %+v, want 3 refused and the cursor past them", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptStarting {
		t.Fatalf("refused kinds changed state: %s/%s", task, attempt)
	}
	if n := f.count(t, "audit_event", "outcome = 'rejected' and provider_id = $2", f.provider.String()); n != 3 {
		t.Fatalf("refusal evidence = %d, want 3", n)
	}
	if n := f.count(t, "binding", "kind = 'runtime_session' and native_ref = 's-1'"); n != 0 {
		t.Fatal("a payload smuggling an identity was bound")
	}
}

func TestEventForgedEnvelopesRefusedBeforeMutation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	other, _ := f.live(t, "n-2")
	frame := simulator.RowsFrame(false, row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}))
	intruder := mustID[ids.Provider](t)
	cases := map[string]struct {
		peer  contracts.Peer
		event contracts.Event
		code  contracts.FailureCode
	}{
		"another provider's peer": {contracts.Peer{Tenant: f.tenant, Provider: intruder}, f.event(rt, s, 1, frame), contracts.FailureUnauthenticated},
		"another tenant's peer":   {contracts.Peer{Tenant: mustID[ids.Tenant](t), Provider: f.provider}, f.event(rt, s, 1, frame), contracts.FailureUnauthorized},
		"another attempt's op":    {f.peer(), rt.Event(s.Attempt.ID, other.Operation.ID, eventStream, 1, frame), contracts.FailureUnauthorized},
	}
	for name, tc := range cases {
		_, err := application.Ingest(f.ctx, f.pool, tc.peer, tc.event, f.at)
		var failure *contracts.Failure
		if !errors.As(err, &failure) || failure.Code != tc.code {
			t.Errorf("%s: Ingest() = %v, want %s", name, err, tc.code)
		}
	}
	for _, table := range []string{"binding", "stream_cursor"} {
		if n := f.count(t, table, "true"); n != 0 {
			t.Fatalf("%s rows after forged envelopes = %d, want 0", table, n)
		}
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptProvisioning {
		t.Fatalf("forged envelope changed state: %s/%s", task, attempt)
	}
	// The other tenant's operation is unknown in that tenant: nothing to
	// record. The two envelopes naming this tenant's operations leave evidence.
	if n := f.count(t, "audit_event", "record_kind = 'provider_operation' and outcome = 'rejected' and event = 'ingest'"); n != 2 {
		t.Fatalf("forged envelope evidence = %d, want 2", n)
	}
}

func TestEventBindingOfAnotherAttemptRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	first, rt := f.live(t, "n-1")
	second, _ := f.live(t, "n-2")
	f.ingest(t, f.event(rt, first, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 1, ObservedAt: f.at}),
	)))
	got := f.ingest(t, f.event(rt, second, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnStarted, contracts.TurnStarted{NativeRef: "t-9", SessionRef: "s-1"}),
		row(3, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{ResourceRef: "box-1", State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at}),
	)))
	if got.Refused != 3 || got.Applied != 0 {
		t.Fatalf("ingestion = %+v, want all three refused", got)
	}
	if _, attempt := f.states(t, second); attempt != lifecycle.AttemptProvisioning {
		t.Fatalf("second attempt = %s, want unchanged", attempt)
	}
	observation, err := store.GetObservation(f.ctx, f.pool, f.tenant, f.provider, "box-1")
	if err != nil {
		t.Fatalf("GetObservation() error = %v", err)
	}
	if observation.Attempt != first.Attempt.ID || observation.State != lifecycle.RunRunning {
		t.Fatalf("observation = %+v, want the first attempt's", observation)
	}
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'rejected' and reason = 'wrong_record'", second.Attempt.ID.String()); n != 3 {
		t.Fatalf("binding refusals = %d, want 3", n)
	}
}

func TestEventEnvironmentObservationsKeepTheNewest(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	observed := func(sequence uint64, state lifecycle.RunState) contracts.EnvironmentObserved {
		return contracts.EnvironmentObserved{ResourceRef: "box-1", State: state, Sequence: sequence, ObservedAt: f.at}
	}
	f.ingestEnvironmentNow(t, f.environmentEvent(s, s.Attempt.EnvironmentOperation, simulator.RowsFrame(false,
		row(1, contracts.EntryEnvironmentObserved, observed(2, lifecycle.RunRunning)),
		row(2, contracts.EntryEnvironmentObserved, observed(1, lifecycle.RunStarting)),
	)))
	observation, err := store.GetObservation(f.ctx, f.pool, f.tenant, f.environmentProvider, "box-1")
	if err != nil {
		t.Fatalf("GetObservation() error = %v", err)
	}
	if observation.Sequence != 2 || observation.State != lifecycle.RunRunning {
		t.Fatalf("observation = %+v, want the newer sequence", observation)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptStarting {
		t.Fatalf("observation changed lifecycle state: %s/%s", task, attempt)
	}
}

func TestEventRehydrationReplaysWithoutRepeating(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	rows := []simulator.Row{
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)),
	}
	f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false, rows...)))
	f.ingest(t, f.event(rt, s, 1, simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 3})))
	again := f.ingest(t, f.event(rt, s, 1, simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 3})))
	if again.Duplicates != 1 || again.Tombstones != 0 {
		t.Fatalf("duplicate tombstone = %+v, want a duplicate", again)
	}
	transitions := f.count(t, "audit_event", "outcome = 'applied'")

	tooOld := f.ingest(t, f.event(rt, s, 1, simulator.CursorTooOldFrame()))
	if !tooOld.Rehydrate || tooOld.Cursor.After != 3 {
		t.Fatalf("cursor_too_old = %+v, want rehydrate with the cursor kept", tooOld)
	}
	cleared := f.ingest(t, f.event(rt, s, 1, simulator.ClearedFrame(2)))
	if !cleared.Rehydrate || cleared.Cursor.Generation != 2 || cleared.Cursor.After != 0 {
		t.Fatalf("cleared = %+v, want rehydrate into generation 2", cleared)
	}
	replay := f.ingest(t, f.event(rt, s, 2, simulator.RowsFrame(true, rows...)))
	if replay.Cursor.After != 2 || replay.Cursor.Generation != 2 {
		t.Fatalf("replay = %+v, want the new generation's cursor", replay)
	}
	stale := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(9, "progress", map[string]int{}))))
	if !stale.Stale || stale.Cursor.Generation != 2 || stale.Cursor.After != 2 {
		t.Fatalf("stale generation = %+v, want ignored", stale)
	}
	if n := f.count(t, "audit_event", "outcome = 'applied'"); n != transitions {
		t.Fatalf("rehydration repeated transitions: %d applied audits, want %d", n, transitions)
	}
	if n := f.count(t, "turn_end", "true"); n != 1 {
		t.Fatalf("turn ends = %d after replay, want 1", n)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("states = %s/%s, want failed/failed", task, attempt)
	}
}

// lost submits a task whose dispatch reply is lost, so the operation is
// uncertain.
func (f *fixture) lost(t *testing.T, nonce string) (application.Submission, *simulator.Runtime) {
	t.Helper()
	return f.replied(t, nonce, simulator.Unknown(&contracts.Failure{Code: contracts.FailureProviderFailed, Message: "lost reply"}))
}

// replied submits a task and dispatches it to a runtime answering reply.
func (f *fixture) replied(t *testing.T, nonce string, reply simulator.Reply) (application.Submission, *simulator.Runtime) {
	t.Helper()
	s := f.submit(t, f.submitCommand(nonce))
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{s.Operation.ID: {Reply: reply, Effect: simulator.TurnStarted}},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	f.tick(t, rt)
	return s, rt
}

func TestReconcileLostReplyMovesAttemptToReconciling(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		outcome lifecycle.OperationState
		task    lifecycle.TaskState
		attempt lifecycle.AttemptState
	}{
		"provider accepted": {lifecycle.OperationSucceeded, lifecycle.TaskRunning, lifecycle.AttemptExecuting},
		"provider failed":   {lifecycle.OperationFailed, lifecycle.TaskFailed, lifecycle.AttemptFailed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.lost(t, "n-1")
			if after := f.reload(t, "n-1"); after.Operation.State != lifecycle.OperationUncertain {
				t.Fatalf("operation = %s, want uncertain", after.Operation.State)
			}
			if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptReconciling {
				t.Fatalf("states = %s/%s, want running/reconciling", task, attempt)
			}
			frame := simulator.RowsFrame(false, row(1, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: tc.outcome}))
			if got := f.ingest(t, f.event(rt, s, 1, frame)); got.Applied != 1 {
				t.Fatalf("ingestion = %+v, want the outcome applied", got)
			}
			if after := f.reload(t, "n-1"); after.Operation.State != tc.outcome {
				t.Fatalf("operation = %s, want %s", after.Operation.State, tc.outcome)
			}
			if task, attempt := f.states(t, s); task != tc.task || attempt != tc.attempt {
				t.Fatalf("states = %s/%s, want %s/%s", task, attempt, tc.task, tc.attempt)
			}
			if got := len(rt.Ledger().Sent()); got != 1 {
				t.Fatalf("sent %d requests, want no redispatch", got)
			}
		})
	}
}

func TestReconcileOutcomeForSettledOperationChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	frame := simulator.RowsFrame(false, row(1, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationFailed}))
	if got := f.ingest(t, f.event(rt, s, 1, frame)); got.Applied != 0 || got.Progress != 1 {
		t.Fatalf("ingestion = %+v, want acknowledged without effect", got)
	}
	if after := f.reload(t, "n-1"); after.Operation.State != lifecycle.OperationAccepted {
		t.Fatalf("operation = %s, want accepted", after.Operation.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptStarting {
		t.Fatalf("states = %s/%s, want unchanged", task, attempt)
	}
}

func TestEventConcurrentDuplicateFramesApplyOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	// The cursor exists before the race, so only its row lock orders the racers.
	f.ingest(t, f.event(rt, s, 1, simulator.ConnectedFrame(f.at)))
	event := f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)),
	))
	const racers = 4
	results := make([]application.Ingestion, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = application.Ingest(f.ctx, f.pool, f.peer(), event, f.at)
		}()
	}
	wg.Wait()
	applied := 0
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		applied += results[i].Applied
	}
	if applied != 2 {
		t.Fatalf("applied entries across racers = %d, want each entry once", applied)
	}
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'applied' and event = 'fail'", s.Attempt.ID.String()); n != 1 {
		t.Fatalf("attempt failures = %d, want 1", n)
	}
}

func TestEventCursorNeverMovesBackwards(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	f.ingest(t, f.event(rt, s, 1, simulator.ConnectedFrame(f.at)))
	holder, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = holder.Rollback(f.ctx) }()
	cursor, err := store.LockCursor(f.ctx, holder, contracts.Cursor{
		Tenant: f.tenant, Attempt: s.Attempt.ID, Operation: s.Operation.ID, Stream: eventStream, Generation: 1,
	})
	if err != nil {
		t.Fatalf("LockCursor() error = %v", err)
	}

	// An older frame arrives while a newer ingest holds the stream.
	older := f.event(rt, s, 1, simulator.RowsFrame(false, row(1, "progress", map[string]int{}), row(2, "progress", map[string]int{})))
	done := make(chan application.Ingestion, 1)
	go func() {
		result, err := application.Ingest(f.ctx, f.pool, f.peer(), older, f.at)
		if err != nil {
			t.Errorf("Ingest() error = %v", err)
		}
		done <- result
	}()
	select {
	case <-done:
		t.Fatal("ingest of the same stream did not wait for the cursor lock")
	case <-time.After(200 * time.Millisecond):
	}
	cursor.After = 4
	if _, err := store.UpsertCursor(f.ctx, holder, cursor); err != nil {
		t.Fatalf("UpsertCursor() error = %v", err)
	}
	if err := holder.Commit(f.ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := <-done; got.Duplicates != 2 || got.Cursor.After != 4 {
		t.Fatalf("older frame = %+v, want duplicates behind cursor 4", got)
	}
	stored, err := store.GetCursor(f.ctx, f.pool, f.tenant, s.Operation.ID, eventStream)
	if err != nil || stored.After != 4 {
		t.Fatalf("cursor = %+v, %v; want 4", stored, err)
	}
}

func TestReconcileFailedTurnWhileReconcilingStaysFailed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "n-1")
	got := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)),
	)))
	if got.Applied != 2 || got.Refused != 0 {
		t.Fatalf("ingestion = %+v, want the failure applied", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("failed turn while reconciling: states = %s/%s, want failed/failed", task, attempt)
	}
	f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(3, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationAccepted}))))
	if after := f.reload(t, "n-1"); after.Operation.State != lifecycle.OperationAccepted {
		t.Fatalf("operation = %s, want reconciled to accepted", after.Operation.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("a later outcome revived failed work: %s/%s", task, attempt)
	}
}

func TestDispatchRejectedReplyFailsAttemptAndTask(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.replied(t, "n-1", simulator.Rejected(&contracts.Failure{Code: contracts.FailureProviderFailed, Message: "refused"}))
	if after := f.reload(t, "n-1"); after.Operation.State != lifecycle.OperationRejected {
		t.Fatalf("operation = %s, want rejected", after.Operation.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("rejected dispatch: states = %s/%s, want failed/failed", task, attempt)
	}
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 1 {
		t.Fatalf("sent %d requests, want no redispatch after a rejection", got)
	}
}

func TestEventTurnUnderAnotherSessionRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.live(t, "n-1")
	got := f.ingest(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-2"}),
		row(3, contracts.EntryTurnStarted, contracts.TurnStarted{NativeRef: "t-1", SessionRef: "s-1"}),
		row(4, contracts.EntryTurnFinished, failedTurn("s-2", "t-1", f.at)),
	)))
	if got.Applied != 3 || got.Refused != 1 {
		t.Fatalf("ingestion = %+v, want the turn under another session refused", got)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("contradicting turn changed state: %s/%s", task, attempt)
	}
	if n := f.count(t, "turn_end", "true"); n != 0 {
		t.Fatalf("turn ends = %d, want none for the contradicting report", n)
	}
}

func TestReconcileWorkerCursorNeverRestoresOlderProgress(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "n-1")
	const dispatchStream = "dispatch"
	progressRows := simulator.RowsFrame(false, row(1, "progress", map[string]int{}), row(4, "progress", map[string]int{}))
	f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, dispatchStream, 1, progressRows))
	stale := contracts.Cursor{Tenant: f.tenant, Attempt: s.Attempt.ID, Operation: s.Operation.ID, Stream: dispatchStream, Generation: 1, After: 2}
	advance := func(cursor contracts.Cursor) contracts.Cursor {
		t.Helper()
		var stored contracts.Cursor
		f.seed(t, func(uow *store.UnitOfWork) error {
			var err error
			stored, err = store.AdvanceCursor(f.ctx, uow, cursor)
			return err
		})
		return stored
	}
	if got := advance(stale); got.Generation != 1 || got.After != 4 {
		t.Fatalf("older position = %+v, want the stored cursor 4 kept", got)
	}
	f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, dispatchStream, 1, simulator.ClearedFrame(2)))
	if got := advance(contracts.Cursor{Tenant: f.tenant, Attempt: s.Attempt.ID, Operation: s.Operation.ID, Stream: dispatchStream, Generation: 1, After: 9}); got.Generation != 2 || got.After != 0 {
		t.Fatalf("older generation = %+v, want generation 2 kept", got)
	}
	if got := advance(contracts.Cursor{Tenant: f.tenant, Attempt: s.Attempt.ID, Operation: s.Operation.ID, Stream: dispatchStream, Generation: 2, After: 3}); got.Generation != 2 || got.After != 3 {
		t.Fatalf("newer position = %+v, want it stored", got)
	}
}
