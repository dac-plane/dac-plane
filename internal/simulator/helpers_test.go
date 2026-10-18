package simulator_test

import (
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// testNow is the fixed instant every scripted frame time uses.
var testNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// mustID returns a fresh identifier of kind K or fails the test.
func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

// testConfig returns a fresh tenant and provider with the default hello.
func testConfig(t *testing.T) simulator.Config {
	t.Helper()
	return simulator.Config{
		Tenant:   mustID[ids.Tenant](t),
		Provider: mustID[ids.Provider](t),
		Hello:    simulator.DefaultHello(),
	}
}

// newRequest scripts a person-demand runtime request for the operation.
func newRequest(t *testing.T, config simulator.Config, operation ids.OperationID, method contracts.Method) contracts.Request {
	t.Helper()
	return contracts.Request{
		Operation: operation,
		Delivery:  mustID[ids.Delivery](t),
		Tenant:    config.Tenant,
		Attempt:   mustID[ids.Attempt](t),
		Role:      contracts.RoleRuntime,
		Method:    method,
		Demand:    contracts.DemandPerson,
		Deadline:  testNow.Add(time.Minute),
	}
}

// newRuntime returns a scripted runtime or fails the test.
func newRuntime(t *testing.T, config simulator.Config, operations map[ids.OperationID]simulator.RuntimeOp) *simulator.Runtime {
	t.Helper()
	sim, err := simulator.NewRuntime(config, simulator.RuntimeScenario{Operations: operations})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return sim
}

// scriptedFailure is a valid typed provider failure for rejection scenarios.
func scriptedFailure() *contracts.Failure {
	return &contracts.Failure{Code: contracts.FailureProviderFailed, Message: "scripted provider failure"}
}
