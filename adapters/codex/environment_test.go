package codexadapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/appserver"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/execclient/exectest"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/journal"
)

const remoteWorkspace = "/workspace"

var testAttachKey = []byte("attach-key")

type remoteFixture struct {
	*fixture
	exec *exectest.Server
}

func setupRemote(t *testing.T) *remoteFixture {
	t.Helper()
	store, err := journal.Open(filepath.Join(t.TempDir(), "adapter.db"))
	if err != nil {
		t.Fatal(err)
	}
	return remoteOn(t, newFakeNative(), store, exectest.New(t, attachToken(testAttachKey, "sandbox:fixture"), nil))
}

func remoteOn(t *testing.T, native *fakeNative, store *journal.Store, exec *exectest.Server) *remoteFixture {
	t.Helper()
	adapter, err := New(Config{Native: native, Journal: store, Workspace: remoteWorkspace, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000022", Now: func() time.Time { return time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC) }, ExecServerURL: exec.URL + "/{resource}", AttachKey: testAttachKey, EnvironmentGrace: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(adapter.stop) })
	return &remoteFixture{&fixture{adapter, native, store, remoteWorkspace}, exec}
}

func environmentEvent(method string) appserver.Message {
	return appserver.Message{Method: method, Params: json.RawMessage(`{"threadId":"thread-1","environmentId":"env"}`)}
}

func completedTurn() appserver.Message {
	return appserver.Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}`)}
}

// The value is pinned because the environment adapter derives the same token,
// and its test pins the same value.
func TestAttachTokenDerivation(t *testing.T) {
	const want = "6d47dc03aab35e65cca42dbc5267090a7ed4fdf847d79892de91dd07f1a28984"
	if got := attachToken(testAttachKey, "sandbox:abc"); got != want {
		t.Fatalf("attach token = %s", got)
	}
}

func TestStartSelectsTheAssignedEnvironment(t *testing.T) {
	f := setupRemote(t)
	if reply, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("start=%+v err=%v", reply, err)
	}
	var add map[string]any
	if err := json.Unmarshal(f.native.last("environment/add"), &add); err != nil {
		t.Fatal(err)
	}
	id := environmentID("sandbox:fixture")
	if add["environmentId"] != id || add["execServerUrl"] != f.exec.URL+"/fixture" || add["authBearerToken"] != attachToken(testAttachKey, "sandbox:fixture") {
		t.Fatalf("environment/add = %+v", add)
	}
	if len(id) > 64 {
		t.Fatalf("environment id too long for Codex: %s", id)
	}
	var thread struct {
		Environments []struct {
			EnvironmentID string `json:"environmentId"`
			Cwd           string `json:"cwd"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(f.native.last("thread/start"), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Environments) != 1 || thread.Environments[0].EnvironmentID != id || thread.Environments[0].Cwd != remoteWorkspace {
		t.Fatalf("thread/start = %s", f.native.last("thread/start"))
	}
	if strings.Contains(string(f.native.last("turn/start")), "resource_ref") {
		t.Fatalf("the reference is still in the prompt: %s", f.native.last("turn/start"))
	}
}

func TestStartWithoutAnEnvironmentIsRefused(t *testing.T) {
	f := setupRemote(t)
	arguments := `{"brief":{"objective":"Change the fixture exactly once.","acceptance_criteria":["result.txt contains qualified"]}}`
	reply, err := f.adapter.Send(context.Background(), startRequest(arguments))
	if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureInvalidArguments {
		t.Fatalf("start=%+v err=%v", reply, err)
	}
	if got := f.native.count("thread/start"); got != 0 {
		t.Fatalf("thread/start calls=%d", got)
	}
}

func TestReferenceThatCouldChangeTheHostIsRefused(t *testing.T) {
	for _, ref := range []string{"sandbox:evil.example/", "sandbox:a@evil.example", "sandbox:a.b", "sandbox:a?b", "sandbox:"} {
		f := setupRemote(t)
		arguments := strings.Replace(validStart, "sandbox:fixture", ref, 1)
		reply, err := f.adapter.Send(context.Background(), startRequest(arguments))
		if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureInvalidArguments {
			t.Fatalf("%s: start=%+v err=%v", ref, reply, err)
		}
		if got := f.native.count("environment/add"); got != 0 {
			t.Fatalf("%s: environment/add calls=%d", ref, got)
		}
	}
}

func TestLocalModeAddsNoEnvironment(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	if got := f.native.count("environment/add"); got != 0 {
		t.Fatalf("environment/add calls=%d", got)
	}
	if strings.Contains(string(f.native.last("thread/start")), "environments") {
		t.Fatalf("thread/start = %s", f.native.last("thread/start"))
	}
}

func TestRestartAddsTheEnvironmentBeforeResuming(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	restarted := newFakeNative()
	remoteOn(t, restarted, f.store, f.exec)
	if got := restarted.count("environment/add"); got != 1 {
		t.Fatalf("environment/add calls=%d", got)
	}
	if restarted.calls[0].method != "environment/add" {
		t.Fatalf("first call after restart was %s", restarted.calls[0].method)
	}
	if !strings.Contains(string(restarted.last("thread/resume")), environmentID("sandbox:fixture")) {
		t.Fatalf("thread/resume = %s", restarted.last("thread/resume"))
	}
}

func TestLostEnvironmentFailsAnInterruptedTurn(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- environmentEvent("thread/environment/disconnected")
	waitFor(t, func() bool { return f.native.count("turn/interrupt") == 1 })
	f.native.notifications <- interruptedTurn()
	waitFor(t, func() bool {
		op, err := f.store.OperationByThread("thread-1")
		return err == nil && op.State == "failed"
	})
}

func TestRestartReportsAnInterruptedTurnAsFailed(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	restarted := newFakeNative()
	restarted.resumedStatus = "interrupted"
	remoteOn(t, restarted, f.store, f.exec)
	op, err := f.store.OperationByThread("thread-1")
	if err != nil || op.State != "failed" {
		t.Fatalf("operation=%+v err=%v", op, err)
	}
}

func TestResultIsReadFromTheEnvironment(t *testing.T) {
	f := setupRemote(t)
	f.exec.Set(remoteWorkspace+"/existing.txt", "unchanged")
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.exec.Set(remoteWorkspace+"/result.txt", "qualified\n")
	f.native.notifications <- completedTurn()
	waitFor(t, func() bool { _, err := f.store.Artifact(testOperation, "result.txt"); return err == nil })
	if _, err := f.store.Artifact(testOperation, "existing.txt"); err == nil {
		t.Fatal("an unchanged file became an artifact")
	}
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
	if string(uploaded) != "qualified\n" {
		t.Fatalf("uploaded=%q", uploaded)
	}
	f.exec.Set(remoteWorkspace+"/result.txt", "changed\n")
	request.Operation = "01a18301-0000-7000-8000-000000000025"
	request.Delivery = "01a18301-0000-7000-8000-000000000026"
	changed, err := f.adapter.Send(context.Background(), request)
	if err != nil || changed.Durability != wire.DurabilityRejected || changed.Failure.Code != wire.FailureStalePrecondition {
		t.Fatalf("changed artifact=%+v err=%v", changed, err)
	}
}

func TestLostEnvironmentInterruptsTheTurn(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- environmentEvent("thread/environment/disconnected")
	waitFor(t, func() bool { return f.native.count("turn/interrupt") == 1 })
}

func TestReturningEnvironmentKeepsTheTurn(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.notifications <- environmentEvent("thread/environment/disconnected")
	f.native.notifications <- environmentEvent("thread/environment/connected")
	waitFor(t, func() bool { return !f.adapter.isEnvironmentLost("thread-1") })
	time.Sleep(60 * time.Millisecond)
	if got := f.native.count("turn/interrupt"); got != 0 {
		t.Fatalf("turn/interrupt calls=%d", got)
	}
}

// The model finished this turn itself, against a dead link, as observed live.
func TestTurnCompletedAfterItsEnvironmentWasLostIsAFailure(t *testing.T) {
	f := setupRemote(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.exec.Set(remoteWorkspace+"/result.txt", "written before the loss")
	f.native.notifications <- environmentEvent("thread/environment/disconnected")
	waitFor(t, func() bool { return f.adapter.isEnvironmentLost("thread-1") })
	f.native.notifications <- completedTurn()
	waitFor(t, func() bool { return strings.Contains(strings.Join(entryKinds(t, f.fixture), ","), "turn.finished") })
	if strings.Contains(strings.Join(entryKinds(t, f.fixture), ","), "result.proposed") {
		t.Fatal("a result was proposed after the environment was lost")
	}
	if got := f.native.count("thread/backgroundTerminals/clean"); got != 0 {
		t.Fatalf("clean calls=%d against a lost environment", got)
	}
	waitFor(t, func() bool {
		op, err := f.store.OperationByThread("thread-1")
		return err == nil && op.State == "failed"
	})
}
