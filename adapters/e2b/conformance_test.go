package e2badapter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/conformance"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

func TestAdapterKitConformance(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	provider := httptest.NewServer(fake)
	defer provider.Close()
	operation := "019a0d5b-f74c-7000-8000-000000000092"
	tenant := "019a0d5b-f74c-7000-8000-000000000094"
	attempt := "019a0d5b-f74c-7000-8000-000000000095"
	arguments, _ := json.Marshal(migrationRequest{Migration: "019a0d5b-f74c-7000-8000-000000000096", Operation: operation, Source: "019a0d5b-f74c-7000-8000-000000000097", Target: attempt, Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
	request := wire.Request{Operation: operation, Delivery: "019a0d5b-f74c-7000-8000-000000000093", Tenant: tenant, Attempt: attempt, Role: wire.RoleEnvironment, Method: "provision", Arguments: arguments, Demand: "background"}
	providerID := "019a0d5b-f74c-7000-8000-000000000098"
	fixture := conformance.Fixture{Provider: providerID, Role: wire.RoleEnvironment, Request: request, Cursor: &wire.Cursor{Tenant: request.Tenant, Attempt: request.Attempt, Operation: operation, Stream: "environment", Generation: 1}}
	conformance.Run(t, conformance.Target{Fixture: fixture, Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
		state, err := journal.Open(t.TempDir() + "/state.db")
		if err != nil {
			return nil, err
		}
		handler, err := New(Config{BaseURL: provider.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: fixture.Provider, Journal: state})
		if err != nil {
			_ = state.Close()
			return nil, err
		}
		runCtx, cancel := context.WithCancel(ctx)
		token := func(context.Context) (adapterkit.Token, error) {
			return adapterkit.Token{Value: "conformance-token", Expiry: time.Now().Add(time.Hour)}, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = adapterkit.Run(runCtx, adapterkit.Config{URL: endpoint.URL, Token: token, Provider: fixture.Provider, Role: wire.RoleEnvironment, Handler: handler, TLS: &tls.Config{RootCAs: endpoint.RootCAs, MinVersion: tls.VersionTLS12}})
		}()
		return func() { cancel(); <-done; _ = state.Close() }, nil
	}})
}

func TestObservationProjectionIsStrictAndOrdered(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{"sbx-1": {SandboxID: "sbx-1", State: "paused"}}}
	a, _ := newTestAdapter(t, fake)
	bad := wire.Request{Operation: "observe-bad", Delivery: "delivery-bad", Tenant: "tenant-1", Attempt: "attempt-target", Role: wire.RoleEnvironment, Method: "observe", Arguments: []byte(`{"environment_ref":"sandbox:sbx-1","unexpected":true}`)}
	got, err := a.Send(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != wire.OutcomeFailed {
		t.Fatalf("unexpected response %+v", got)
	}
	good := bad
	good.Operation = "observe-good"
	good.Arguments = []byte(`{"environment_ref":"sandbox:sbx-1"}`)
	got, err = a.Send(context.Background(), good)
	if err != nil {
		t.Fatal(err)
	}
	var observation observation
	if err := json.Unmarshal(got.Result, &observation); err != nil {
		t.Fatal(err)
	}
	if observation.State != "hibernated" || observation.Sequence != 1 {
		t.Fatalf("unexpected observation %+v", observation)
	}
}
