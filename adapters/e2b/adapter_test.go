package e2badapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

type fakeE2B struct {
	mu                   sync.Mutex
	created              int
	deleted              int
	sandboxes            map[string]sandbox
	timeouts             []int
	dropFirstCreateReply bool
	rejectCreateStatus   int
	hideList             bool
	malformedList        bool
	failDeletes          int
}

func (f *fakeE2B) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != "e2b-test-secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v2/sandboxes":
		if f.rejectCreateStatus != 0 {
			status := f.rejectCreateStatus
			f.rejectCreateStatus = 0
			http.Error(w, http.StatusText(status), status)
			return
		}
		var input struct {
			Template  string `json:"templateID"`
			Allow     *bool  `json:"allow_internet_access"`
			Timeout   int    `json:"timeout"`
			AutoPause *bool  `json:"autoPause"`
			Network   struct {
				DenyOut []string `json:"denyOut"`
			} `json:"network"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if input.Template != "base" || input.Allow == nil || *input.Allow || len(input.Network.DenyOut) != 1 || input.Network.DenyOut[0] != "0.0.0.0/0" || input.Metadata[metadataOperation] == "" || input.Timeout <= 0 || input.AutoPause == nil || *input.AutoPause {
			http.Error(w, "invalid create request", 400)
			return
		}
		f.created++
		f.timeouts = append(f.timeouts, input.Timeout)
		id := fmt.Sprintf("sbx-%d", f.created)
		sbx := sandbox{SandboxID: id, Template: input.Template, State: "running", Metadata: input.Metadata}
		f.sandboxes[id] = sbx
		if f.dropFirstCreateReply {
			f.dropFirstCreateReply = false
			hj := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"sandboxId": sbx.SandboxID, "templateID": sbx.Template, "state": sbx.State})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		if f.malformedList {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[`))
			return
		}
		var found []sandbox
		for _, sbx := range f.sandboxes {
			if f.hideList {
				continue
			}
			if sbx.Metadata[metadataOperation] == stringsAfterEqual(r.URL.Query().Get("metadata")) {
				found = append(found, sbx)
			}
		}
		_ = json.NewEncoder(w).Encode(found)
	case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/sbx-1":
		if sbx, ok := f.sandboxes["sbx-1"]; ok {
			_ = json.NewEncoder(w).Encode(sbx)
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodDelete:
		if f.failDeletes > 0 {
			f.failDeletes--
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		id := r.URL.Path[len("/sandboxes/"):]
		if _, ok := f.sandboxes[id]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.sandboxes, id)
		f.deleted++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func stringsAfterEqual(value string) string {
	for i := range value {
		if value[i] == '=' {
			return value[i+1:]
		}
	}
	return ""
}

func newTestAdapter(t *testing.T, fake *fakeE2B) (*Adapter, *journal.Store) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store, err := journal.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a, err := New(Config{BaseURL: server.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: "e2b-test", Journal: store, Now: func() time.Time { return time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return a, store
}

func provisionRequest(op, delivery string) wire.Request {
	raw, _ := json.Marshal(migrationRequest{Migration: "migration-1", Operation: op, Source: "attempt-source", Target: "attempt-target", Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
	return wire.Request{Operation: op, Delivery: delivery, Tenant: "tenant-1", Attempt: "attempt-target", Role: wire.RoleEnvironment, Method: "provision", Arguments: raw}
}

func TestProvisionReplayReturnsRecordedSandbox(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a, _ := newTestAdapter(t, fake)
	req := provisionRequest("operation-1", "delivery-1")
	first, err := a.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Delivery = "delivery-2"
	second, err := a.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != wire.OutcomeOK || second.Outcome != wire.OutcomeOK || fake.created != 1 {
		t.Fatalf("first=%+v second=%+v creates=%d", first, second, fake.created)
	}
	var receipt migrationReceipt
	if err := json.Unmarshal(second.Result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.EnvRef != "sandbox:sbx-1" {
		t.Fatalf("environment ref %q", receipt.EnvRef)
	}
}

func TestTaskEnvironmentProvisionAndStopPublishObservations(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a, _ := newTestAdapter(t, fake)
	provision, _ := json.Marshal(struct{}{})
	request := wire.Request{Operation: "task-environment-create", Delivery: "delivery-create", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: provision}
	created, err := a.Send(context.Background(), request)
	if err != nil || created.Outcome != wire.OutcomeOK {
		t.Fatalf("provision = %+v, %v", created, err)
	}
	var result taskProvisionResult
	if err := json.Unmarshal(created.Result, &result); err != nil || result.ResourceRef != "sandbox:sbx-1" {
		t.Fatalf("provision result %+v, %v", result, err)
	}
	stream, err := a.Subscribe(context.Background(), wire.Cursor{Tenant: request.Tenant, Attempt: request.Attempt, Operation: request.Operation, Stream: "events", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := stream.Next(context.Background())
	if err == nil && frame.Kind == wire.StreamConnected {
		frame, err = stream.Next(context.Background())
	}
	if err != nil || frame.Rows == nil || len(frame.Rows.Entries) != 1 {
		t.Fatalf("provision observation frame = %+v, %v", frame, err)
	}
	var ready observation
	if err := json.Unmarshal(frame.Rows.Entries[0].Payload, &ready); err != nil || ready.State != "running" || ready.ResourceRef != result.ResourceRef {
		t.Fatalf("ready observation %+v, %v", ready, err)
	}
	stopArgs, _ := json.Marshal(map[string]string{"provision_operation_id": request.Operation})
	stop := wire.Request{Operation: "task-environment-stop", Delivery: "delivery-stop", Tenant: request.Tenant, Attempt: request.Attempt, Role: wire.RoleEnvironment, Method: "stop", Arguments: stopArgs}
	stopped, err := a.Send(context.Background(), stop)
	if err != nil || stopped.Outcome != wire.OutcomeOK {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	stream, err = a.Subscribe(context.Background(), wire.Cursor{Tenant: stop.Tenant, Attempt: stop.Attempt, Operation: stop.Operation, Stream: "events", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = stream.Next(context.Background())
	if err == nil && frame.Kind == wire.StreamConnected {
		frame, err = stream.Next(context.Background())
	}
	if err != nil || frame.Rows == nil || len(frame.Rows.Entries) != 1 {
		t.Fatalf("stop observation frame = %+v, %v", frame, err)
	}
	var absent observation
	if err := json.Unmarshal(frame.Rows.Entries[0].Payload, &absent); err != nil || absent.State != "absent" || absent.Sequence != ready.Sequence+1 {
		t.Fatalf("stop observation %+v, %v", absent, err)
	}
}

func taskStopRequest(provision, stop string) wire.Request {
	args, _ := json.Marshal(map[string]string{"provision_operation_id": provision})
	return wire.Request{Operation: stop, Delivery: "delivery-" + stop, Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "stop", Arguments: args}
}

func provisionTaskEnvironment(t *testing.T, a *Adapter, operation string) {
	t.Helper()
	raw, _ := json.Marshal(struct{}{})
	reply, err := a.Send(context.Background(), wire.Request{Operation: operation, Delivery: "delivery-" + operation, Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: raw})
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("provision = %+v, %v", reply, err)
	}
}

func stopObservations(t *testing.T, store *journal.Store, operation string) []observation {
	t.Helper()
	entries, err := store.EventsAfter(operation, 0)
	if err != nil {
		t.Fatal(err)
	}
	var observed []observation
	for _, entry := range entries {
		var o observation
		if err := json.Unmarshal(entry.Payload, &o); err != nil {
			t.Fatal(err)
		}
		observed = append(observed, o)
	}
	return observed
}

// The control plane never sends a stop twice, so an unconfirmed DELETE must
// complete from the adapter and publish the observation that settles it.
func TestFailedTaskStopRetriesUntilObserved(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, failDeletes: 2}
	a, store := newTestAdapter(t, fake)
	provisionTaskEnvironment(t, a, "create")
	if reply, err := a.Send(context.Background(), taskStopRequest("create", "stop")); err == nil {
		t.Fatalf("failed stop = %+v, want an unknown outcome", reply)
	}
	if got := stopObservations(t, store, "stop"); len(got) != 0 {
		t.Fatalf("observations = %+v, want none before the sandbox is gone", got)
	}
	a.retryPendingStops(context.Background())
	if got := stopObservations(t, store, "stop"); len(got) != 0 {
		t.Fatalf("observations = %+v, want none while DELETE fails", got)
	}
	a.retryPendingStops(context.Background())
	got := stopObservations(t, store, "stop")
	if len(got) != 1 || got[0].State != "absent" || got[0].ResourceRef != "sandbox:sbx-1" {
		t.Fatalf("observations = %+v, want one absent", got)
	}
	if fake.deleted != 1 || len(fake.sandboxes) != 0 {
		t.Fatalf("deleted = %d, sandboxes = %v", fake.deleted, fake.sandboxes)
	}
	a.retryPendingStops(context.Background())
	if pending, err := store.PendingStops(); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if got := stopObservations(t, store, "stop"); len(got) != 1 {
		t.Fatalf("observations = %+v, want no duplicate", got)
	}
}

// A stop interrupted by a restart is finished by the next process.
func TestPendingTaskStopSurvivesRestart(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, failDeletes: 1}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	path := t.TempDir() + "/state.db"
	store, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{BaseURL: server.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: "e2b-test", Journal: store}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	provisionTaskEnvironment(t, a, "create")
	if reply, err := a.Send(context.Background(), taskStopRequest("create", "stop")); err == nil {
		t.Fatalf("failed stop = %+v, want an unknown outcome", reply)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	config.Journal = reopened
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	restarted.retryPendingStops(context.Background())
	if got := stopObservations(t, reopened, "stop"); len(got) != 1 || got[0].State != "absent" {
		t.Fatalf("observations = %+v, want one absent", got)
	}
}

// An unknown create stays unresolved until the sandbox lifetime proves it gone.
func TestStopOfUnknownCreateWaitsOutTheSandboxLifetime(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, hideList: true, rejectCreateStatus: http.StatusBadGateway}
	a, store := newTestAdapter(t, fake)
	raw, _ := json.Marshal(struct{}{})
	if reply, err := a.Send(context.Background(), wire.Request{Operation: "create", Delivery: "d", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: raw}); err == nil {
		t.Fatalf("provision = %+v, want an unknown outcome", reply)
	}
	op, err := store.Operation("create")
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := a.Send(context.Background(), taskStopRequest("create", "stop")); err == nil {
		t.Fatalf("stop = %+v, want an unknown outcome", reply)
	}
	a.cfg.Now = func() time.Time { return op.CreatedAt.Add(a.cfg.SandboxTimeout + createOutcomeMargin + time.Second) }
	a.retryPendingStops(context.Background())
	if got := stopObservations(t, store, "stop"); len(got) != 1 || got[0].State != "absent" || got[0].ResourceRef != "operation:create" {
		t.Fatalf("observations = %+v, want the operation observed absent", got)
	}
}

// A repeated stop of a provision that was fenced before it started stays clean.
func TestStopAfterCancelledProvisionStaysAbsent(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, rejectCreateStatus: http.StatusBadRequest}
	a, store := newTestAdapter(t, fake)
	raw, _ := json.Marshal(struct{}{})
	if _, err := a.Send(context.Background(), wire.Request{Operation: "create", Delivery: "d", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: raw}); err == nil {
		t.Fatal("rejected create was accepted")
	}
	if err := a.Cancel(context.Background(), wire.Cancel{Operation: "create", Delivery: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if reply, err := a.Send(context.Background(), taskStopRequest("create", "stop")); err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("stop = %+v, %v", reply, err)
	}
	if got := stopObservations(t, store, "stop"); len(got) != 1 || got[0].ResourceRef != "operation:create" {
		t.Fatalf("observations = %+v", got)
	}
}

// A crash after the sandbox mapping is saved but before its readiness is
// recorded must not leave the runtime waiting forever: redelivery repairs it,
// exactly once.
func TestTaskProvisionRedeliveryRepairsMissingReadiness(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{"sbx-1": {SandboxID: "sbx-1"}}}
	a, store := newTestAdapter(t, fake)
	provision, _ := json.Marshal(struct{}{})
	request := wire.Request{Operation: "task-environment-create", Delivery: "delivery-1", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: provision}
	if _, _, err := store.Begin(journal.Operation{ID: request.Operation, Hash: digest(request.Arguments), Tenant: request.Tenant, Attempt: request.Attempt, Method: request.Method}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(request.Operation, func(o *journal.Operation) error { o.ResourceRef, o.State = "sandbox:sbx-1", "provisioned"; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range []string{"delivery-2", "delivery-3"} {
		request.Delivery = delivery
		reply, err := a.Send(context.Background(), request)
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("redelivered provision = %+v, %v", reply, err)
		}
		var result taskProvisionResult
		if err := json.Unmarshal(reply.Result, &result); err != nil || result.ResourceRef != "sandbox:sbx-1" {
			t.Fatalf("provision result %+v, %v", result, err)
		}
	}
	events, err := store.EventsAfter(request.Operation, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("readiness events = %+v, %v; want exactly one", events, err)
	}
	var ready observation
	if err := json.Unmarshal(events[0].Payload, &ready); err != nil || ready.State != "running" || ready.ResourceRef != "sandbox:sbx-1" {
		t.Fatalf("ready observation %+v, %v", ready, err)
	}
	if fake.created != 0 {
		t.Fatalf("sandbox creates = %d, want the recorded sandbox reused", fake.created)
	}
}

func TestTaskEnvironmentStopBeforeProvisionFencesCreate(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a, _ := newTestAdapter(t, fake)
	stopArgs, _ := json.Marshal(map[string]string{"provision_operation_id": "provision-before-stop"})
	stop := wire.Request{Operation: "stop-before-create", Delivery: "delivery-stop", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "stop", Arguments: stopArgs}
	reply, err := a.Send(context.Background(), stop)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("stop = %+v, %v", reply, err)
	}
	provision := wire.Request{Operation: "provision-before-stop", Delivery: "delivery-create", Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: json.RawMessage(`{}`)}
	reply, err = a.Send(context.Background(), provision)
	if err != nil || reply.Durability != wire.DurabilityRejected || reply.Failure.Code != wire.FailureCancelled || fake.created != 0 {
		t.Fatalf("provision after stop = %+v, %v; creates=%d", reply, err, fake.created)
	}
}

func TestLostCreateReplyReconcilesBeforeRedelivery(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, dropFirstCreateReply: true}
	a, _ := newTestAdapter(t, fake)
	reply, err := a.Send(context.Background(), provisionRequest("operation-2", "delivery-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeOK || fake.created != 1 {
		t.Fatalf("reply=%+v creates=%d", reply, fake.created)
	}
	if _, err := a.Send(context.Background(), provisionRequest("operation-2", "delivery-2")); err != nil {
		t.Fatal(err)
	}
	if fake.created != 1 {
		t.Fatalf("duplicate sandbox create count %d", fake.created)
	}
}

func TestDefinitiveCreateRejectionCanRetry(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, rejectCreateStatus: http.StatusUnauthorized}
	adapter, store := newTestAdapter(t, fake)
	request := provisionRequest("retry-rejected-create", "delivery-1")
	if _, err := adapter.Send(context.Background(), request); err == nil {
		t.Fatal("expected explicit provider rejection")
	}
	operation, err := store.Operation(request.Operation)
	if err != nil || operation.State != "prepared" {
		t.Fatalf("operation after explicit rejection=%+v err=%v", operation, err)
	}
	request.Delivery = "delivery-2"
	reply, err := adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK || fake.created != 1 {
		t.Fatalf("retry reply outcome=%q err=%v creates=%d", reply.Outcome, err, fake.created)
	}
}

func TestUnknownCreateReconcilesAfterJournalRestartWithoutRetryingCreate(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, dropFirstCreateReply: true, hideList: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	statePath := t.TempDir() + "/state.db"
	store, err := journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{BaseURL: server.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: "e2b-test", Journal: store}
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	request := provisionRequest("operation-restart", "delivery-1")
	if _, err := adapter.Send(context.Background(), request); err == nil {
		t.Fatal("lost create reply with no visible resource should remain uncertain")
	}
	if fake.created != 1 {
		t.Fatalf("creates after uncertain first attempt: %d", fake.created)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	fake.hideList = false
	store, err = journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	adapter, err = New(configWithJournal(config, store))
	if err != nil {
		t.Fatal(err)
	}
	request.Delivery = "delivery-2"
	reply, err := adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("reconcile: reply=%+v err=%v", reply, err)
	}
	if fake.created != 1 {
		t.Fatalf("reconciliation issued another create: %d", fake.created)
	}
}

func TestMalformedCreateLookupDoesNotRetryCreate(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, dropFirstCreateReply: true, malformedList: true}
	a, _ := newTestAdapter(t, fake)
	if _, err := a.Send(context.Background(), provisionRequest("operation-malformed-list", "delivery-1")); err == nil {
		t.Fatal("expected malformed reconciliation response to leave the outcome uncertain")
	}
	if fake.created != 1 {
		t.Fatalf("create calls after malformed lookup: %d", fake.created)
	}
	fake.malformedList = false
	reply, err := a.Send(context.Background(), provisionRequest("operation-malformed-list", "delivery-2"))
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("reconcile after lookup recovery: reply=%+v err=%v", reply, err)
	}
	if fake.created != 1 {
		t.Fatalf("reconciliation issued another create: %d", fake.created)
	}
}

func configWithJournal(config Config, store *journal.Store) Config {
	config.Journal = store
	return config
}

func TestStopIsIdempotentAndObserveCancellationDoesNotStopResource(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{"sbx-1": {SandboxID: "sbx-1", State: "running"}}}
	adapter, _ := newTestAdapter(t, fake)
	observe := wire.Request{Operation: "observe-cancel", Delivery: "d1", Tenant: "tenant-1", Attempt: "attempt-target", Role: wire.RoleEnvironment, Method: "observe", Arguments: []byte(`{"environment_ref":"sandbox:sbx-1"}`)}
	if _, err := adapter.Send(context.Background(), observe); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Cancel(context.Background(), wire.Cancel{Operation: observe.Operation, Delivery: "d2"}); err != nil {
		t.Fatal(err)
	}
	if fake.deleted != 0 {
		t.Fatal("cancelling an observation stopped the environment")
	}
	args, _ := json.Marshal(migrationRequest{Migration: "migration-1", Operation: "stop-op", Source: "attempt-source", Stage: "source_cleaned", Epoch: 1, EnvRef: "sandbox:sbx-1"})
	stop := wire.Request{Operation: "stop-op", Delivery: "d3", Tenant: "tenant-1", Attempt: "attempt-source", Role: wire.RoleEnvironment, Method: "stop", Arguments: args}
	first, err := adapter.Send(context.Background(), stop)
	if err != nil || first.Outcome != wire.OutcomeOK {
		t.Fatalf("stop reply=%+v err=%v", first, err)
	}
	stop.Delivery = "d4"
	second, err := adapter.Send(context.Background(), stop)
	if err != nil || second.Outcome != wire.OutcomeOK {
		t.Fatalf("stop replay=%+v err=%v", second, err)
	}
	if fake.deleted != 1 {
		t.Fatalf("delete calls=%d, want 1", fake.deleted)
	}
}

func TestCancelProvisionDeletesOnlyRecordedSandboxAndReplays(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	adapter, _ := newTestAdapter(t, fake)
	request := provisionRequest("cancel-provision", "delivery-1")
	if reply, err := adapter.Send(context.Background(), request); err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("provision reply=%+v err=%v", reply, err)
	}
	cancel := wire.Cancel{Operation: request.Operation, Delivery: "cancel-1"}
	if err := adapter.Cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	cancel.Delivery = "cancel-2"
	if err := adapter.Cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if fake.deleted != 1 || len(fake.sandboxes) != 0 {
		t.Fatalf("delete calls=%d remaining sandboxes=%d", fake.deleted, len(fake.sandboxes))
	}
}

func TestObserveReportsDeletedSandboxAsAbsent(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{"sbx-1": {SandboxID: "sbx-1", State: "running"}}}
	adapter, _ := newTestAdapter(t, fake)
	args, _ := json.Marshal(migrationRequest{Migration: "migration-1", Operation: "stop-after-observe", Source: "attempt-source", Stage: "source_cleaned", Epoch: 1, EnvRef: "sandbox:sbx-1"})
	stopped, err := adapter.Send(context.Background(), wire.Request{Operation: "stop-after-observe", Delivery: "d1", Tenant: "tenant-1", Attempt: "attempt-source", Role: wire.RoleEnvironment, Method: "stop", Arguments: args})
	if err != nil || stopped.Outcome != wire.OutcomeOK {
		t.Fatalf("stop reply=%+v err=%v", stopped, err)
	}
	observed, err := adapter.Send(context.Background(), wire.Request{Operation: "observe-after-stop", Delivery: "d2", Tenant: "tenant-1", Attempt: "attempt-source", Role: wire.RoleEnvironment, Method: "observe", Arguments: []byte(`{"environment_ref":"sandbox:sbx-1"}`)})
	if err != nil || observed.Outcome != wire.OutcomeOK {
		t.Fatalf("observe reply=%+v err=%v", observed, err)
	}
	var state observation
	if err := json.Unmarshal(observed.Result, &state); err != nil || state.State != "absent" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestCancelBeforeProvisionPreventsSandboxCreation(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	adapter, _ := newTestAdapter(t, fake)
	request := provisionRequest("cancel-before-start", "delivery-1")
	if err := adapter.Cancel(context.Background(), wire.Cancel{Operation: request.Operation, Delivery: "cancel-1"}); err != nil {
		t.Fatal(err)
	}
	reply, err := adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeFailed || reply.Failure == nil || reply.Failure.Code != wire.FailureCancelled {
		t.Fatalf("provision reply=%+v err=%v", reply, err)
	}
	if fake.created != 0 {
		t.Fatalf("pre-cancelled operation created %d sandboxes", fake.created)
	}
}

func taskProvisionRequest(op, delivery string) wire.Request {
	return wire.Request{Operation: op, Delivery: delivery, Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: []byte(`{}`)}
}

func TestEveryCreateCarriesTheSandboxLifetime(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	adapter, _ := newTestAdapter(t, fake)
	for _, request := range []wire.Request{taskProvisionRequest("task-lifetime", "delivery-1"), provisionRequest("migration-lifetime", "delivery-2")} {
		if reply, err := adapter.Send(context.Background(), request); err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("provision %s = %+v, %v", request.Operation, reply, err)
		}
	}
	if want := int(defaultSandboxTimeout / time.Second); len(fake.timeouts) != 2 || fake.timeouts[0] != want || fake.timeouts[1] != want {
		t.Fatalf("create timeouts = %v, want two of %d", fake.timeouts, want)
	}
}

func TestSandboxTimeoutConfiguration(t *testing.T) {
	store, err := journal.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := Config{BaseURL: "https://api.e2b.app", APIKey: "key", TemplateID: "base", ProviderID: "e2b", Journal: store}
	for _, timeout := range []time.Duration{-time.Minute, time.Millisecond} {
		config.SandboxTimeout = timeout
		if _, err := New(config); err == nil {
			t.Fatalf("New accepted sandbox timeout %s", timeout)
		}
	}
	config.SandboxTimeout = 6 * time.Hour
	adapter, err := New(config)
	if err != nil || adapter.cfg.SandboxTimeout != 6*time.Hour {
		t.Fatalf("configured timeout = %+v, %v", adapter, err)
	}
}

func TestRejectedTaskCreateCanRetry(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, rejectCreateStatus: http.StatusBadRequest}
	adapter, store := newTestAdapter(t, fake)
	request := taskProvisionRequest("retry-rejected-task-create", "delivery-1")
	if _, err := adapter.Send(context.Background(), request); err == nil {
		t.Fatal("expected explicit provider rejection")
	}
	operation, err := store.Operation(request.Operation)
	if err != nil || operation.State != "prepared" {
		t.Fatalf("operation after explicit rejection=%+v err=%v", operation, err)
	}
	request.Delivery = "delivery-2"
	reply, err := adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK || fake.created != 1 {
		t.Fatalf("retry reply outcome=%q err=%v creates=%d", reply.Outcome, err, fake.created)
	}
}

func TestCancelAfterRejectedTaskProvisionPreventsRetry(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, rejectCreateStatus: http.StatusBadRequest}
	adapter, _ := newTestAdapter(t, fake)
	request := taskProvisionRequest("cancel-rejected-task-create", "delivery-1")
	if _, err := adapter.Send(context.Background(), request); err == nil {
		t.Fatal("first create should be rejected")
	}
	if err := adapter.Cancel(context.Background(), wire.Cancel{Operation: request.Operation, Delivery: "cancel-1"}); err != nil {
		t.Fatal(err)
	}
	reply, err := adapter.Send(context.Background(), taskProvisionRequest(request.Operation, "delivery-2"))
	if err != nil || reply.Outcome != wire.OutcomeFailed || reply.Failure == nil || reply.Failure.Code != wire.FailureCancelled {
		t.Fatalf("provision retry=%+v err=%v, want cancelled", reply, err)
	}
	if fake.created != 0 {
		t.Fatalf("creates=%d, want none", fake.created)
	}
}

func TestCancelAfterRejectedProvisionPreventsRetry(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, rejectCreateStatus: http.StatusUnauthorized}
	adapter, _ := newTestAdapter(t, fake)
	request := provisionRequest("cancel-rejected-create", "delivery-1")
	if _, err := adapter.Send(context.Background(), request); err == nil {
		t.Fatal("first create should be rejected")
	}
	if err := adapter.Cancel(context.Background(), wire.Cancel{Operation: request.Operation, Delivery: "cancel-1"}); err != nil {
		t.Fatal(err)
	}
	retry := provisionRequest(request.Operation, "delivery-2")
	reply, err := adapter.Send(context.Background(), retry)
	if err != nil || reply.Outcome != wire.OutcomeFailed || reply.Failure == nil || reply.Failure.Code != wire.FailureCancelled {
		t.Fatalf("provision retry=%+v err=%v, want cancelled", reply, err)
	}
	if fake.created != 0 || len(fake.sandboxes) != 0 {
		t.Fatalf("creates=%d remaining sandboxes=%d, want no create", fake.created, len(fake.sandboxes))
	}
}

func TestProvisionRejectsChangedPayload(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a, _ := newTestAdapter(t, fake)
	if _, err := a.Send(context.Background(), provisionRequest("operation-3", "delivery-1")); err != nil {
		t.Fatal(err)
	}
	changed := provisionRequest("operation-3", "delivery-2")
	changed.Arguments = []byte(`{"migration_id":"migration-2","operation_id":"operation-3","source_attempt_id":"attempt-source","target_attempt_id":"attempt-target","stage":"target_environment_ready","claim_epoch":1,"recovery":"restart_from_artifacts"}`)
	reply, err := a.Send(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeFailed || fake.created != 1 {
		t.Fatalf("reply=%+v creates=%d", reply, fake.created)
	}
}

func TestObserveSequencePersistsAndStreams(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{"sbx-1": {SandboxID: "sbx-1", State: "running"}}}
	a, store := newTestAdapter(t, fake)
	makeRequest := func(op string) wire.Request {
		return wire.Request{Operation: op, Delivery: "delivery-" + op, Tenant: "tenant-1", Attempt: "attempt-target", Role: wire.RoleEnvironment, Method: "observe", Arguments: []byte(`{"environment_ref":"sandbox:sbx-1"}`)}
	}
	for i, op := range []string{"observe-1", "observe-2"} {
		reply, err := a.Send(context.Background(), makeRequest(op))
		if err != nil {
			t.Fatal(err)
		}
		var got observation
		if err := json.Unmarshal(reply.Result, &got); err != nil {
			t.Fatalf("reply=%+v: %v", reply, err)
		}
		if got.Sequence != uint64(i+1) || got.State != "running" {
			t.Fatalf("observation %+v", got)
		}
	}
	duplicate, err := a.Send(context.Background(), makeRequest("observe-2"))
	if err != nil {
		t.Fatal(err)
	}
	var replayed observation
	if err := json.Unmarshal(duplicate.Result, &replayed); err != nil || replayed.Sequence != 2 {
		t.Fatalf("duplicate observation=%+v err=%v", replayed, err)
	}
	stream, err := a.Subscribe(context.Background(), wire.Cursor{Operation: "observe-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if frame, err := stream.Next(context.Background()); err != nil || frame.Kind != wire.StreamConnected {
		t.Fatalf("connected frame=%+v err=%v", frame, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame, err := stream.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != wire.StreamRows || frame.Rows == nil || len(frame.Rows.Entries) != 1 || frame.Rows.Entries[0].Kind != "environment.observed" {
		t.Fatalf("event frame %+v", frame)
	}
	op, err := store.Operation("observe-2")
	if err != nil || op.Sequence != 2 || op.State != "observed" {
		t.Fatalf("persisted observation state %+v err=%v", op, err)
	}
}

func TestNewRejectsSecretInURL(t *testing.T) {
	store, err := journal.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = New(Config{BaseURL: "https://user:secret@example.test", APIKey: "key", TemplateID: "base", ProviderID: "e2b", Journal: store})
	if err == nil {
		t.Fatal("expected credential-bearing URL rejection")
	}
}

func TestProviderErrorBodyDoesNotLeakCredential(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-API-Key", "e2b-test-secret")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"e2b-test-secret"}`))
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	store, err := journal.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	adapter, err := New(Config{BaseURL: server.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: "e2b-test", Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Send(context.Background(), provisionRequest("redaction-operation", "delivery-1"))
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if strings.Contains(err.Error(), "e2b-test-secret") {
		t.Fatalf("provider error leaked credential: %v", err)
	}
}
