package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/appserver"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/journal"
)

const (
	testOperation = "01a18301-0000-7000-8000-000000000001"
	testDelivery  = "01a18301-0000-7000-8000-000000000002"
	testTenant    = "01a18301-0000-7000-8000-000000000003"
	testAttempt   = "01a18301-0000-7000-8000-000000000004"
)

type nativeCall struct {
	method string
	params json.RawMessage
}
type fakeNative struct {
	mu                  sync.Mutex
	calls               []nativeCall
	replies             []json.RawMessage
	notifications       chan appserver.Message
	requests            chan appserver.Message
	failThreadStart     bool
	failTurnStart       bool
	failResume          bool
	failClean           bool
	failReplyOnce       bool
	completeOnTurnStart bool
	blockTurnStart      bool
	turnStartEntered    chan struct{}
	releaseTurnStart    chan struct{}
}

func newFakeNative() *fakeNative {
	return &fakeNative{notifications: make(chan appserver.Message, 32), requests: make(chan appserver.Message, 32)}
}
func (f *fakeNative) Call(_ context.Context, method string, params, result any) error {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, nativeCall{method, raw})
	fail := f.failThreadStart && method == "thread/start"
	failResume := f.failResume && method == "thread/resume"
	failClean := f.failClean && method == "thread/backgroundTerminals/clean"
	f.mu.Unlock()
	if failClean {
		return errors.New("clean failed")
	}
	if fail {
		return errors.New("lost thread/start reply")
	}
	if failResume {
		return errors.New("no rollout found")
	}
	var response any
	switch method {
	case "thread/start":
		response = map[string]any{"thread": map[string]any{"id": "thread-1"}}
	case "turn/start":
		if f.blockTurnStart {
			close(f.turnStartEntered)
			<-f.releaseTurnStart
		}
		response = map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}}
		if f.failTurnStart {
			f.requests <- appserver.Message{ID: json.RawMessage(`17`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"approval-during-start","command":"touch result.txt"}`)}
			return errors.New("turn/start response lost")
		}
		if f.completeOnTurnStart {
			f.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
		}
	case "thread/resume":
		response = map[string]any{"thread": map[string]any{"id": "thread-1", "turns": []any{map[string]any{"id": "turn-1", "status": "inProgress"}}}}
	case "thread/read":
		response = map[string]any{"thread": map[string]any{"id": "thread-1"}}
	default:
		response = map[string]any{}
	}
	encoded, _ := json.Marshal(response)
	return json.Unmarshal(encoded, result)
}
func (f *fakeNative) Reply(id json.RawMessage, result any) error {
	raw, _ := json.Marshal(map[string]any{"id": json.RawMessage(id), "result": result})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReplyOnce {
		f.failReplyOnce = false
		return errors.New("lost approval reply")
	}
	f.replies = append(f.replies, raw)
	return nil
}
func (f *fakeNative) NotificationMessages() <-chan appserver.Message { return f.notifications }
func (f *fakeNative) RequestMessages() <-chan appserver.Message      { return f.requests }
func (f *fakeNative) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.method == method {
			n++
		}
	}
	return n
}
func (f *fakeNative) last(method string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i].params
		}
	}
	return nil
}

type fixture struct {
	adapter   *Adapter
	native    *fakeNative
	store     *journal.Store
	workspace string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	store, err := journal.Open(filepath.Join(root, "state", "adapter.db"))
	if err != nil {
		t.Fatal(err)
	}
	native := newFakeNative()
	workspace := filepath.Join(root, "workspace")
	if err = os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Native: native, Journal: store, Workspace: workspace, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000022", Now: func() time.Time { return time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(adapter.stop); _ = store.Close() })
	return &fixture{adapter, native, store, workspace}
}
func startRequest(arguments string) wire.Request {
	return wire.Request{Operation: testOperation, Delivery: testDelivery, Tenant: testTenant, Attempt: testAttempt, Role: wire.RoleRuntime, Method: "start_turn", Arguments: json.RawMessage(arguments), Demand: "person", Deadline: time.Now().Add(time.Minute)}
}

const validStart = `{"brief":{"objective":"Change the fixture exactly once.","acceptance_criteria":["result.txt contains qualified"]},"environment":{"provider_id":"01a18301-0000-7000-8000-000000000022","resource_ref":"sandbox:fixture"}}`

func TestStartCarriesBriefAndReplaysMapping(t *testing.T) {
	f := setup(t)
	request := startRequest(validStart)
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("start reply=%+v err=%v", reply, err)
	}
	if f.native.count("thread/start") != 1 || f.native.count("turn/start") != 1 {
		t.Fatalf("native calls=%+v", f.native.calls)
	}
	turn := string(f.native.last("turn/start"))
	for _, want := range []string{testOperation, "Change the fixture exactly once.", "result.txt contains qualified", "untrusted task data", "sandbox:fixture"} {
		if !strings.Contains(turn, want) {
			t.Fatalf("turn input misses %q: %s", want, turn)
		}
	}
	request.Delivery = "01a18301-0000-7000-8000-000000000005"
	replay, err := f.adapter.Send(context.Background(), request)
	if err != nil || replay.Outcome != wire.OutcomeOK || f.native.count("thread/start") != 1 {
		t.Fatalf("replay=%+v err=%v calls=%d", replay, err, f.native.count("thread/start"))
	}
	changed := startRequest(`{"brief":{"objective":"different","acceptance_criteria":["done"]}}`)
	changed.Delivery = "01a18301-0000-7000-8000-000000000006"
	rejected, err := f.adapter.Send(context.Background(), changed)
	if err != nil || rejected.Durability != wire.DurabilityRejected || rejected.Failure.Code != wire.FailureStalePrecondition {
		t.Fatalf("changed=%+v err=%v", rejected, err)
	}
}

func TestStreamWakesForAnAppendedEvent(t *testing.T) {
	f := setup(t)
	if _, _, err := f.store.Begin(journal.Operation{ID: testOperation, Tenant: testTenant, Attempt: testAttempt, ArgumentsHash: journal.Hash([]byte("stream"))}); err != nil {
		t.Fatal(err)
	}
	s := &stream{store: f.store, operation: testOperation}
	result := make(chan error, 1)
	go func() {
		frame, err := s.Next(context.Background())
		if err == nil && (frame.Kind != wire.StreamRows || frame.Rows == nil || len(frame.Rows.Entries) != 1) {
			err = errors.New("stream did not return the appended event")
		}
		result <- err
	}()
	if _, err := f.store.AppendEvent(testOperation, "stream-event", "progress", map[string]string{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not wake after an event was appended")
	}
}

func TestCompletionDuringTurnStartFollowsDurableStartEvents(t *testing.T) {
	f := setup(t)
	f.native.completeOnTurnStart = true
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		events, err := f.store.EventsAfter(testOperation, 0)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "turn.finished" {
				return true
			}
		}
		return false
	})
	events, err := f.store.EventsAfter(testOperation, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for i, event := range events {
		positions[event.Kind] = i
	}
	if !(positions["runtime.started"] < positions["turn.started"] && positions["turn.started"] < positions["turn.finished"]) {
		t.Fatalf("startup/completion order: %+v", events)
	}
}

func TestNativeEventsContinueDrainingWhileTurnStartWaits(t *testing.T) {
	f := setup(t)
	f.native.blockTurnStart = true
	f.native.turnStartEntered = make(chan struct{})
	f.native.releaseTurnStart = make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(f.native.releaseTurnStart)
		}
	}()
	started := make(chan error, 1)
	go func() {
		_, err := f.adapter.Send(context.Background(), startRequest(validStart))
		started <- err
	}()
	select {
	case <-f.native.turnStartEntered:
	case <-time.After(time.Second):
		t.Fatal("turn/start RPC did not begin")
	}
	for i := 0; i < 300; i++ {
		message := appserver.Message{Method: "item/agentMessage/delta", Params: json.RawMessage(fmt.Sprintf(`{"threadId":"thread-1","delta":"%d"}`, i))}
		select {
		case f.native.notifications <- message:
		case <-time.After(time.Second):
			t.Fatalf("native notifications stopped draining at event %d", i)
		}
	}
	close(f.native.releaseTurnStart)
	released = true
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		events, err := f.store.EventsAfter(testOperation, 0)
		return err == nil && len(events) >= 302
	})
}

func TestFailedTurnStartReleasesBufferedApprovalRequest(t *testing.T) {
	f := setup(t)
	f.native.failTurnStart = true
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err == nil {
		t.Fatal("failed turn/start was accepted")
	}
	waitFor(t, func() bool {
		request, err := f.store.NativeRequest("approval-during-start")
		if err != nil || request.Operation != testOperation {
			return false
		}
		events, err := f.store.EventsAfter(testOperation, 0)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "attention.requested" {
				return true
			}
		}
		return false
	})
	events, err := f.store.EventsAfter(testOperation, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for i, event := range events {
		positions[event.Kind] = i
	}
	if !(positions["runtime.started"] < positions["turn.started"] && positions["turn.started"] < positions["attention.requested"]) {
		t.Fatalf("recovered startup event order: %+v", events)
	}
}

func TestStartupReadyEarlyReturnPurgesUnmappedDeferredEvents(t *testing.T) {
	f := setup(t)
	if _, _, err := f.store.Begin(journal.Operation{ID: testOperation, Tenant: testTenant, Attempt: testAttempt, ArgumentsHash: journal.Hash([]byte("mapped"))}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateOperation(testOperation, func(op *journal.Operation) error {
		op.ThreadID, op.TurnID = "thread-1", "turn-1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter.beginStartup()
	f.adapter.deferNative("orphan-thread", journal.Operation{}, os.ErrNotExist, deferredNative{message: appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"orphan-thread"}`)}})
	known, err := f.store.Operation(testOperation)
	if err != nil {
		t.Fatal(err)
	}
	f.adapter.deferNative("thread-1", known, nil, deferredNative{message: appserver.Message{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","delta":"ready"}`)}})
	f.adapter.setThreadReady("thread-1")
	if f.adapter.deferredCount != 0 || len(f.adapter.deferred) != 0 {
		t.Fatalf("orphaned deferred state remains: count=%d queues=%v", f.adapter.deferredCount, f.adapter.deferred)
	}
}

func TestLostThreadStartDoesNotCreateSecondNativeTurn(t *testing.T) {
	f := setup(t)
	f.native.failThreadStart = true
	request := startRequest(validStart)
	if _, err := f.adapter.Send(context.Background(), request); err == nil {
		t.Fatal("lost reply was accepted")
	}
	if _, err := f.adapter.Send(context.Background(), request); err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("redelivery err=%v", err)
	}
	if got := f.native.count("thread/start"); got != 1 {
		t.Fatalf("thread/start calls=%d", got)
	}
}

func entryKinds(t *testing.T, f *fixture) []string {
	t.Helper()
	entries, err := f.store.EventsAfter(testOperation, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(entries))
	for _, entry := range entries {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}

// The product can only settle a start whose reply it lost from this stream.
func TestStartPublishesItsOutcomeOnce(t *testing.T) {
	f := setup(t)
	request := startRequest(validStart)
	for range 2 {
		if reply, err := f.adapter.Send(context.Background(), request); err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("start=%+v err=%v", reply, err)
		}
	}
	if got := strings.Join(entryKinds(t, f), ","); got != "runtime.started,turn.started,operation_outcome" {
		t.Fatalf("entries=%s", got)
	}
}

func TestCancelBeforeStartFencesTheOperation(t *testing.T) {
	f := setup(t)
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entryKinds(t, f), ","); got != "operation_outcome" {
		t.Fatalf("entries=%s", got)
	}
	reply, err := f.adapter.Send(context.Background(), startRequest(validStart))
	if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure == nil || reply.Failure.Code != wire.FailureCancelled {
		t.Fatalf("start=%+v err=%v", reply, err)
	}
	if got := f.native.count("thread/start"); got != 0 {
		t.Fatalf("thread/start calls=%d", got)
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if got := len(entryKinds(t, f)); got != 1 {
		t.Fatalf("entries=%d, want one rejection", got)
	}
}

// A start that left no turn behind is settled by the interrupt that finds it.
func TestCancelAfterLostStartRejectsIt(t *testing.T) {
	f := setup(t)
	f.native.failThreadStart = true
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err == nil {
		t.Fatal("lost reply was accepted")
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entryKinds(t, f), ","); got != "operation_outcome" {
		t.Fatalf("entries=%s", got)
	}
}

// An interrupt during turn/start must not fence a turn about to be mapped.
func TestCancelDuringStartWaitsForTheTurn(t *testing.T) {
	f := setup(t)
	f.native.blockTurnStart = true
	f.native.turnStartEntered = make(chan struct{})
	f.native.releaseTurnStart = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.adapter.Send(context.Background(), startRequest(validStart))
		done <- err
	}()
	<-f.native.turnStartEntered
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	close(f.native.releaseTurnStart)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if got := f.native.count("turn/interrupt"); got != 1 {
		t.Fatalf("turn/interrupt calls=%d, want the repeat to find the mapped turn", got)
	}
}

// A cancel that read the operation before its turn was mapped must not fence
// the start once it has finished.
func TestCancelRacingAFinishingStartLeavesItsTurnMapped(t *testing.T) {
	for range 100 {
		f := setup(t)
		f.native.blockTurnStart = true
		f.native.turnStartEntered = make(chan struct{})
		f.native.releaseTurnStart = make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := f.adapter.Send(context.Background(), startRequest(validStart))
			done <- err
		}()
		<-f.native.turnStartEntered
		stop := make(chan struct{})
		var cancels sync.WaitGroup
		for range 4 {
			cancels.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
						_ = f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery})
					}
				}
			})
		}
		close(f.native.releaseTurnStart)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		close(stop)
		cancels.Wait()
		op, err := f.store.Operation(testOperation)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == journal.StateCancelled {
			t.Fatalf("operation with turn %q was fenced: entries=%s", op.TurnID, strings.Join(entryKinds(t, f), ","))
		}
	}
}

func interruptedTurn() appserver.Message {
	return appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"interrupted"}}`)}
}

// turn/interrupt leaves its command running, so the stop evidence must follow
// the end of the command rather than the end of the turn.
func TestInterruptedTurnEndsItsCommandsBeforeReportingTheStop(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- interruptedTurn()
	waitFor(t, func() bool {
		return strings.HasSuffix(strings.Join(entryKinds(t, f), ","), "turn.finished,runtime.stopped")
	})
	if got := f.native.count("thread/backgroundTerminals/clean"); got != 1 {
		t.Fatalf("clean calls=%d", got)
	}
}

func TestFailedCommandCleanupWithholdsTheStopAndExits(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.mu.Lock()
	f.native.failClean = true
	f.native.mu.Unlock()
	f.native.notifications <- interruptedTurn()
	select {
	case err := <-f.adapter.Fatal():
		if err == nil {
			t.Fatal("nil fatal error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter kept running without ending the command")
	}
	if got := strings.Join(entryKinds(t, f), ","); strings.Contains(got, "runtime.stopped") || strings.Contains(got, "turn.finished") {
		t.Fatalf("entries=%s, want no stop evidence", got)
	}
}

func TestNaturallyCompletedTurnEndsItsCommandsToo(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed"}}`)}
	waitFor(t, func() bool { return strings.Contains(strings.Join(entryKinds(t, f), ","), "turn.finished") })
	if got := f.native.count("thread/backgroundTerminals/clean"); got != 1 {
		t.Fatalf("clean calls=%d", got)
	}
}

// A turn/start with no reply may have reached Codex, so the rejection that would
// prove nothing runs must wait for a process that knows it.
func TestCancelAfterUnknownTurnStartRestartsInsteadOfRejecting(t *testing.T) {
	f := setup(t)
	f.native.failTurnStart = true
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err == nil {
		t.Fatal("lost turn/start was accepted")
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.adapter.Fatal():
	case <-time.After(5 * time.Second):
		t.Fatal("adapter kept running with an unknown turn")
	}
	if got := strings.Join(entryKinds(t, f), ","); strings.Contains(got, "operation_outcome") {
		t.Fatalf("entries=%s, want no rejection", got)
	}
}

func TestCancelAfterFinishedTurnEndsItsCommands(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- interruptedTurn()
	waitFor(t, func() bool { return f.native.count("thread/backgroundTerminals/clean") == 1 })
	waitFor(t, func() bool {
		op, err := f.store.Operation(testOperation)
		return err == nil && op.State == "interrupted"
	})
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if got := f.native.count("thread/backgroundTerminals/clean"); got != 2 {
		t.Fatalf("clean calls=%d", got)
	}
	if got := f.native.count("turn/interrupt"); got != 0 {
		t.Fatalf("turn/interrupt calls=%d", got)
	}
}

func TestRestartResumesAndReadsMappedThread(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(validStart)
	_, _, err = store.Begin(journal.Operation{ID: testOperation, Tenant: testTenant, Attempt: testAttempt, ArgumentsHash: journal.Hash(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateOperation(testOperation, func(op *journal.Operation) error {
		op.ThreadID = "thread-1"
		op.TurnID = "turn-1"
		op.State = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	native := newFakeNative()
	adapter, err := New(Config{Native: native, Journal: store, Workspace: root, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000022"})
	if err != nil {
		t.Fatal(err)
	}
	defer close(adapter.stop)
	defer store.Close()
	if native.count("thread/resume") != 1 || native.count("thread/read") != 1 {
		t.Fatalf("calls=%+v", native.calls)
	}
	events, err := store.EventsAfter(testOperation, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, event := range events {
		found[event.Kind] = true
	}
	for _, kind := range []string{"runtime.started", "turn.started", "progress"} {
		if !found[kind] {
			t.Fatalf("restart events=%v, missing %s", found, kind)
		}
	}
}

func TestRestartKeepsUncertainEmptyThreadWithoutAcceptingReplay(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	arguments := json.RawMessage(validStart)
	if _, _, err = store.Begin(journal.Operation{ID: testOperation, Tenant: testTenant, Attempt: testAttempt, ArgumentsHash: journal.Hash(arguments)}); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateOperation(testOperation, func(op *journal.Operation) error {
		op.ThreadID = "empty-thread"
		op.State = "thread_started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	native := newFakeNative()
	native.failResume = true
	adapter, err := New(Config{Native: native, Journal: store, Workspace: root, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000022"})
	if err != nil {
		t.Fatal(err)
	}
	defer close(adapter.stop)
	if _, err = adapter.Send(context.Background(), startRequest(validStart)); err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("redelivery err=%v", err)
	}
	if native.count("thread/start") != 0 || native.count("turn/start") != 0 {
		t.Fatalf("uncertain operation created native work: %+v", native.calls)
	}
}

func TestApprovalDecisionAndCancellation(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"approval-1","command":"touch result.txt","startedAtMs":1}`)
	f.native.requests <- appserver.Message{ID: json.RawMessage(`17`), Method: "item/commandExecution/requestApproval", Params: params}
	var entries []wire.Entry
	waitFor(t, func() bool {
		var loadErr error
		entries, loadErr = f.store.EventsAfter(testOperation, 0)
		if loadErr != nil {
			return false
		}
		for _, entry := range entries {
			if entry.Kind == "attention.requested" {
				return true
			}
		}
		return false
	})
	var approval map[string]any
	for _, entry := range entries {
		if entry.Kind == "attention.requested" {
			if err := json.Unmarshal(entry.Payload, &approval); err != nil {
				t.Fatal(err)
			}
		}
	}
	if scope, ok := approval["scope"].(map[string]any); !ok || scope["class"] != "global" {
		t.Fatalf("approval scope=%v", approval["scope"])
	}
	decision := `{"decision_id":"01a18301-0000-7000-8000-000000000010","attention":"01a18301-0000-7000-8000-000000000011","attempt":"` + testAttempt + `","operation":"01a18301-0000-7000-8000-000000000099","native_ref":"approval-1","kind":"approve_once","expires_at":"2026-10-28T16:00:00Z"}`
	request := startRequest(decision)
	request.Method = "steer_turn"
	request.Delivery = "01a18301-0000-7000-8000-000000000012"
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("steer=%+v err=%v", reply, err)
	}
	f.native.mu.Lock()
	delivered := string(f.native.replies[0])
	f.native.mu.Unlock()
	if !strings.Contains(delivered, `"decision":"accept"`) {
		t.Fatalf("native decision=%s", delivered)
	}
	if _, err = f.adapter.Send(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	f.native.mu.Lock()
	count := len(f.native.replies)
	f.native.mu.Unlock()
	if count != 1 {
		t.Fatalf("decision delivered %d times", count)
	}
	foreign := request
	foreign.Operation = "01a18301-0000-7000-8000-000000000099"
	rejected, err := f.adapter.Send(context.Background(), foreign)
	if err != nil || rejected.Durability != wire.DurabilityRejected || rejected.Failure.Code != wire.FailureStalePrecondition {
		t.Fatalf("foreign decision=%+v err=%v", rejected, err)
	}
	if err = f.adapter.Cancel(context.Background(), wire.Cancel{Operation: testOperation, Delivery: testDelivery}); err != nil {
		t.Fatal(err)
	}
	if f.native.count("turn/interrupt") != 1 {
		t.Fatal("cancel did not interrupt the mapped turn")
	}
}

func TestExpiredApprovalIsNotDeliveredToCodex(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.requests <- appserver.Message{ID: json.RawMessage(`17`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"approval-expired","command":"touch result.txt"}`)}
	waitFor(t, func() bool { _, err := f.store.NativeRequest("approval-expired"); return err == nil })
	decision := `{"decision_id":"01a18301-0000-7000-8000-000000000030","attempt":"` + testAttempt + `","native_ref":"approval-expired","kind":"approve_once","expires_at":"2026-10-28T14:59:59Z"}`
	request := startRequest(decision)
	request.Method = "steer_turn"
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureStalePrecondition {
		t.Fatalf("expired decision=%+v err=%v", reply, err)
	}
	f.native.mu.Lock()
	defer f.native.mu.Unlock()
	if len(f.native.replies) != 0 {
		t.Fatalf("expired decision delivered to native app-server: %s", f.native.replies)
	}
}

func TestPreparedDecisionIsRetriedBeforeAcceptance(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.requests <- appserver.Message{ID: json.RawMessage(`17`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"approval-1","command":"touch result.txt"}`)}
	waitFor(t, func() bool { _, err := f.store.NativeRequest("approval-1"); return err == nil })
	f.native.mu.Lock()
	f.native.failReplyOnce = true
	f.native.mu.Unlock()
	decision := `{"decision_id":"01a18301-0000-7000-8000-000000000010","attention":"01a18301-0000-7000-8000-000000000011","attempt":"` + testAttempt + `","native_ref":"approval-1","kind":"approve_once","expires_at":"2026-10-28T16:00:00Z"}`
	request := startRequest(decision)
	request.Method = "steer_turn"
	if _, err := f.adapter.Send(context.Background(), request); err == nil {
		t.Fatal("lost native approval reply was accepted")
	}
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("retry=%+v err=%v", reply, err)
	}
	f.native.mu.Lock()
	count := len(f.native.replies)
	f.native.mu.Unlock()
	if count != 1 {
		t.Fatalf("delivered replies=%d", count)
	}
}

func TestRestartRequiresNativeApprovalRequestToBeReissued(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	arguments := json.RawMessage(validStart)
	if _, _, err = store.Begin(journal.Operation{ID: testOperation, Tenant: testTenant, Attempt: testAttempt, ArgumentsHash: journal.Hash(arguments)}); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateOperation(testOperation, func(op *journal.Operation) error {
		op.ThreadID, op.TurnID, op.State = "thread-1", "turn-1", "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.PutNativeRequest(journal.NativeRequest{NativeRef: "approval-1", Operation: testOperation, Method: "item/commandExecution/requestApproval", Session: "previous-process", RequestID: json.RawMessage(`17`)}); err != nil {
		t.Fatal(err)
	}
	native := newFakeNative()
	adapter, err := New(Config{Native: native, Journal: store, Workspace: root, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000022", Now: func() time.Time { return time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	defer close(adapter.stop)
	decision := `{"decision_id":"01a18301-0000-7000-8000-000000000010","attention":"01a18301-0000-7000-8000-000000000011","attempt":"` + testAttempt + `","native_ref":"approval-1","kind":"approve_once","expires_at":"2026-10-28T16:00:00Z"}`
	request := startRequest(decision)
	request.Method = "steer_turn"
	rejected, err := adapter.Send(context.Background(), request)
	if err != nil || rejected.Durability != wire.DurabilityRejected {
		t.Fatalf("stale native request=%+v err=%v", rejected, err)
	}
	native.requests <- appserver.Message{ID: json.RawMessage(`41`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"approval-1","command":"touch result.txt"}`)}
	waitFor(t, func() bool {
		got, loadErr := store.NativeRequest("approval-1")
		return loadErr == nil && got.Session == adapter.session
	})
	accepted, err := adapter.Send(context.Background(), request)
	if err != nil || accepted.Outcome != wire.OutcomeOK {
		t.Fatalf("reissued native request=%+v err=%v", accepted, err)
	}
}

func TestCompletionProposesAndExportsExactArtifact(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	content := []byte("qualified\n")
	if err := os.WriteFile(filepath.Join(f.workspace, "result.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	completed := json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}`)
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: completed}
	waitFor(t, func() bool { _, err := f.store.Artifact(testOperation, "result.txt"); return err == nil })
	artifact, err := f.store.Artifact(testOperation, "result.txt")
	if err != nil {
		t.Fatal(err)
	}
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	manifest := map[string]any{"artifact_id": testDelivery, "tenant_id": testTenant, "task_id": "01a18301-0000-7000-8000-000000000020", "attempt_id": testAttempt, "operation_id": "01a18301-0000-7000-8000-000000000021", "producer_id": "01a18301-0000-7000-8000-000000000022", "kind": "code_change", "digest": artifact.Digest, "size": artifact.Size, "media_type": artifact.MediaType, "source_revision": "codex-turn:turn-1", "writer_epoch": 1}
	arguments, _ := json.Marshal(map[string]any{"manifest": manifest, "native_ref": "result.txt", "upload": server.URL})
	request := startRequest(string(arguments))
	request.Operation = "01a18301-0000-7000-8000-000000000023"
	request.Method = "export_artifacts"
	request.Delivery = "01a18301-0000-7000-8000-000000000024"
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("export=%+v err=%v", reply, err)
	}
	if string(uploaded) != string(content) {
		t.Fatalf("uploaded=%q", uploaded)
	}
	if err = os.WriteFile(filepath.Join(f.workspace, "result.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Operation = "01a18301-0000-7000-8000-000000000025"
	request.Delivery = "01a18301-0000-7000-8000-000000000026"
	rejected, err := f.adapter.Send(context.Background(), request)
	if err != nil || rejected.Durability != wire.DurabilityRejected || rejected.Failure.Code != wire.FailureStalePrecondition {
		t.Fatalf("changed artifact=%+v err=%v", rejected, err)
	}
}

func TestUnknownNativeTurnStatusCannotBecomeCompleted(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"new-status"}}`)}
	waitFor(t, func() bool {
		events, err := f.store.EventsAfter(testOperation, 0)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "turn.finished" {
				return strings.Contains(string(event.Payload), `"outcome":"failed"`)
			}
		}
		return false
	})
}

func TestCompletedTurnWithoutExportableChangesReportsFailure(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
	waitForProtocolResultFailure(t, f)
}

func TestCompletedTurnWithOnlyDeletedFilesReportsFailure(t *testing.T) {
	f := setup(t)
	deleted := filepath.Join(f.workspace, "delete-me.txt")
	if err := os.WriteFile(deleted, []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
	waitForProtocolResultFailure(t, f)
}

func TestCompletedTurnWithMoreThan32ChangedFilesReportsFailure(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 33; i++ {
		path := filepath.Join(f.workspace, fmt.Sprintf("change-%02d.txt", i))
		if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.native.notifications <- appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
	waitForProtocolResultFailure(t, f)
}

func waitForProtocolResultFailure(t *testing.T, f *fixture) {
	t.Helper()
	waitFor(t, func() bool {
		events, err := f.store.EventsAfter(testOperation, 0)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "turn.finished" {
				return strings.Contains(string(event.Payload), `"outcome":"failed"`) && strings.Contains(string(event.Payload), "protocol_violation")
			}
		}
		return false
	})
}

func TestMalformedArgumentsAreRejected(t *testing.T) {
	f := setup(t)
	for _, raw := range []string{`{}`, `{"brief":{"objective":"x","acceptance_criteria":[]}}`, `{"brief":{"objective":"x","acceptance_criteria":["y"]},"provider":"camer"}`, `{"brief":{"objective":"` + strings.Repeat("界", 10_001) + `","acceptance_criteria":["y"]}}`, `{"brief":{"objective":"x","acceptance_criteria":["` + strings.Repeat("界", 1_001) + `"]}}`} {
		reply, err := f.adapter.Send(context.Background(), startRequest(raw))
		if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureInvalidArguments {
			t.Fatalf("raw=%s reply=%+v err=%v", raw, reply, err)
		}
	}
}

func TestApprovalDigestUsesCanonicalNativeInput(t *testing.T) {
	first, err := canonicalHash([]byte(`{"threadId":"t","itemId":"i","command":"touch x"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicalHash([]byte("{\n  \"command\": \"touch x\", \"itemId\": \"i\", \"threadId\": \"t\"}"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("canonical digests differ: %s != %s", first, second)
	}
}

func TestArtifactExportRejectsRemotePlaintextCapability(t *testing.T) {
	f := setup(t)
	request := startRequest(`{"native_ref":"result.txt","upload":"http://example.com/upload","manifest":{}}`)
	request.Method = "export_artifacts"
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureInvalidArguments {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
