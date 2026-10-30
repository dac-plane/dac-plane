package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestAdapterURLsSelectOneConfiguration(t *testing.T) {
	if got, err := adapterURLs("wss://one.example", ""); err != nil || len(got) != 1 || got[0] != "wss://one.example" {
		t.Fatalf("single adapterURLs() = %v, %v", got, err)
	}
	got, err := adapterURLs("", " wss://one.example, wss://two.example, wss://one.example ")
	if err != nil || len(got) != 2 || got[0] != "wss://one.example" || got[1] != "wss://two.example" {
		t.Fatalf("multiple adapterURLs() = %v, %v", got, err)
	}
	for _, inputs := range [][2]string{{"", ""}, {"wss://one.example", "wss://two.example"}} {
		if _, err := adapterURLs(inputs[0], inputs[1]); err == nil {
			t.Errorf("adapterURLs(%q, %q) succeeded", inputs[0], inputs[1])
		}
	}
}

func TestControlAddressIsDisabledByDefault(t *testing.T) {
	if got := controlAddress(func(string) string { return "" }); got != "" {
		t.Fatalf("controlAddress() = %q, want disabled", got)
	}
	if got := controlAddress(func(string) string { return " 127.0.0.1:8080 " }); got != "127.0.0.1:8080" {
		t.Fatalf("controlAddress() = %q, want trimmed address", got)
	}
}

func TestControlPausesAndReportsOneEffect(t *testing.T) {
	tenant := mustNewID(t, ids.New[ids.Tenant])
	provider := mustNewID(t, ids.New[ids.Provider])
	operation := mustNewID(t, ids.New[ids.Operation])
	runtime, err := simulator.NewRuntime(simulator.Config{Tenant: tenant, Provider: provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{
			operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newControlHandler(&simulatorSet{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler.routes())
	t.Cleanup(server.Close)

	pause := []byte(`{"role":"runtime","boundary":"after_effect"}`)
	response := controlRequest(t, http.MethodPost, server.URL+"/v1/pauses", pause)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register pause = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	request := contracts.Request{
		Operation: operation, Delivery: mustNewID(t, ids.New[ids.Delivery]), Tenant: tenant,
		Attempt: mustNewID(t, ids.New[ids.Attempt]), Role: contracts.RoleRuntime,
		Method: contracts.MethodStartTurn, Demand: contracts.DemandPerson, Deadline: time.Now().Add(time.Minute),
	}
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Send(context.Background(), request)
		done <- err
	}()

	path := "/v1/pauses/runtime/next/after_effect"
	deadline := time.Now().Add(time.Second)
	for {
		response = controlRequest(t, http.MethodGet, server.URL+path, nil)
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK && bytes.Contains(body, []byte(`"reached":true`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pause was not reached: %d %s", response.StatusCode, body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	response = controlRequest(t, http.MethodGet, server.URL+"/v1/ledger/runtime/"+operation.String(), nil)
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"deliveries":1`)) || !bytes.Contains(body, []byte(`"effects":1`)) || !bytes.Contains(body, []byte(`"kind":"turn_started"`)) {
		t.Fatalf("ledger = %d %s", response.StatusCode, body)
	}
	response = controlRequest(t, http.MethodPost, server.URL+path+"/release", nil)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("release pause = %d", response.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

func TestControlRejectsMalformedRequestsAndMissingStore(t *testing.T) {
	runtime, err := simulator.NewRuntime(simulator.Config{Tenant: mustNewID(t, ids.New[ids.Tenant]), Provider: mustNewID(t, ids.New[ids.Provider])}, simulator.RuntimeScenario{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newControlHandler(&simulatorSet{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler.routes())
	t.Cleanup(server.Close)

	for _, request := range []struct {
		method string
		path   string
		body   []byte
		want   int
	}{
		{http.MethodPost, "/v1/pauses", []byte(`{"role":"runtime"}`), http.StatusBadRequest},
		{http.MethodPost, "/v1/pauses", []byte(`{} {}`), http.StatusBadRequest},
		{http.MethodPost, "/v1/pauses", bytes.Repeat([]byte(" "), controlBodyLimit+1), http.StatusBadRequest},
		{http.MethodGet, "/v1/ledger/unknown/not-an-id", nil, http.StatusBadRequest},
		{http.MethodGet, "/v1/artifacts/" + mustNewID(t, ids.New[ids.Artifact]).String(), nil, http.StatusNotFound},
	} {
		response := controlRequest(t, request.method, server.URL+request.path, request.body)
		_ = response.Body.Close()
		if response.StatusCode != request.want {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.StatusCode, request.want)
		}
	}
}

func controlRequest(t *testing.T, method, target string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func mustNewID[T any](t *testing.T, create func() (T, error)) T {
	t.Helper()
	id, err := create()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestScriptRowsNumberRowsAndFillFixtureDigests(t *testing.T) {
	rows, err := scriptRows([]scriptRow{
		{Kind: contracts.EntryRuntimeStarted, Payload: json.RawMessage("{\n  \"native_ref\": \"s-1\"\n}")},
		{Kind: contracts.EntryResultProposed, Payload: json.RawMessage(`{"session_ref":"s-1","turn_ref":"t-1","source_revision":"r","artifacts":[{"native_ref":"patch","kind":"code_change","media_type":"text/plain"}]}`)},
	}, 3, map[string]string{"patch": "exact bytes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Sequence != 3 || rows[1].Sequence != 4 || rows[1].Updated != 4 {
		t.Fatalf("rows = %+v, want sequences 3 and 4", rows)
	}
	if rows[0].Payload != `{"native_ref":"s-1"}` {
		t.Fatalf("first payload = %q, want compact JSON so its wire hash remains stable", rows[0].Payload)
	}
	var proposed contracts.ResultProposed
	if err := json.Unmarshal([]byte(rows[1].Payload), &proposed); err != nil {
		t.Fatal(err)
	}
	if got := proposed.Artifacts[0]; got.Digest != identity.DigestOf([]byte("exact bytes")) || got.Size != uint64(len("exact bytes")) {
		t.Fatalf("artifact = %+v, want the fixture's digest and size", got)
	}
}

func TestRuntimeScenarioDefaultsServeAnyOperation(t *testing.T) {
	var s scenarioFile
	if err := json.Unmarshal([]byte(`{"default":{"effect":"turn_started","rows":[{"kind":"runtime.started","payload":{"native_ref":"s-1"}}],"after_decision":[{"kind":"decision.consumed","payload":{"native_ref":"q-1"}}]}}`), &s); err != nil {
		t.Fatal(err)
	}
	scenario, err := s.runtimeScenario()
	if err != nil {
		t.Fatal(err)
	}
	if scenario.Default == nil || scenario.DefaultDecision == nil || scenario.Default.AfterDecision == nil {
		t.Fatalf("scenario = %+v, want a default, a default decision and a continuation", scenario)
	}
	if frames := scenario.Default.Stream.Frames; len(frames) != 2 || frames[0].Kind != contracts.StreamConnected || frames[1].Rows.Entries[0].Sequence != 1 {
		t.Fatalf("default stream = %+v", frames)
	}
	if after := scenario.Default.AfterDecision.Frames[0].Rows.Entries[0]; after.Sequence != 2 {
		t.Fatalf("continuation starts at %d, want 2", after.Sequence)
	}
}

func TestScenarioEnvironmentServesItsOwnRegistration(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAC_ADAPTER_TOKEN_FILE", tokenFile)
	var s scenarioFile
	if err := json.Unmarshal([]byte(`{"tenant_id":"01a167fd-51d9-76ba-b563-496e2bd89646","provider_id":"01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10","role":"runtime",
		"environment":{"provider_id":"01a167fd-51d9-7d44-8a3b-6c5d4e3f2a12"}}`), &s); err != nil {
		t.Fatal(err)
	}
	if _, err := s.adapters(nil, false); err == nil || !strings.Contains(err.Error(), "DAC_ENVIRONMENT_TOKEN_FILE") {
		t.Fatalf("adapters() without an environment token error = %v, want the missing setting named", err)
	}
	t.Setenv("DAC_ENVIRONMENT_TOKEN_FILE", tokenFile)
	set, err := s.adapters(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.adapters) != 2 || set.adapters[1].role != wire.RoleEnvironment || set.adapters[1].provider != s.Environment.Provider || len(set.adapters[1].offered) != 0 {
		t.Fatalf("adapters = %+v, want the runtime and an environment without replay", set.adapters)
	}
	if set.adapters[1].port.Role() != contracts.RoleEnvironment {
		t.Fatalf("environment port role = %s", set.adapters[1].port.Role())
	}
}
