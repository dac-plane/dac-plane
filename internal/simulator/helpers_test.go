package simulator_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
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

// newRoleRequest scripts a person-demand request for any role's operation.
func newRoleRequest(t *testing.T, config simulator.Config, operation ids.OperationID, role contracts.Role, method contracts.Method, arguments []byte) contracts.Request {
	t.Helper()
	return contracts.Request{
		Operation: operation,
		Delivery:  mustID[ids.Delivery](t),
		Tenant:    config.Tenant,
		Attempt:   mustID[ids.Attempt](t),
		Role:      role,
		Method:    method,
		Demand:    contracts.DemandPerson,
		Arguments: arguments,
		Deadline:  testNow.Add(time.Minute),
	}
}

// newRequest scripts a person-demand runtime request for the operation.
func newRequest(t *testing.T, config simulator.Config, operation ids.OperationID, method contracts.Method) contracts.Request {
	t.Helper()
	return newRoleRequest(t, config, operation, contracts.RoleRuntime, method, nil)
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

// newEnvironment returns a scripted environment or fails the test.
func newEnvironment(t *testing.T, config simulator.Config, operations map[ids.OperationID]simulator.EnvironmentOp) *simulator.Environment {
	t.Helper()
	sim, err := simulator.NewEnvironment(config, simulator.EnvironmentScenario{Operations: operations})
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	return sim
}

// newEnforcement returns a scripted enforcement or fails the test.
func newEnforcement(t *testing.T, config simulator.Config, operations map[ids.OperationID]simulator.EnforcementOp) *simulator.Enforcement {
	t.Helper()
	sim, err := simulator.NewEnforcement(config, simulator.EnforcementScenario{Operations: operations})
	if err != nil {
		t.Fatalf("NewEnforcement() error = %v", err)
	}
	return sim
}

// observation is one valid observation of an attempt's resource, by sequence.
func observation(config simulator.Config, attempt ids.AttemptID, sequence uint64, state lifecycle.RunState) lifecycle.EnvironmentObservation {
	return lifecycle.EnvironmentObservation{
		Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider,
		ResourceRef: "env-1", State: state, Sequence: sequence, ObservedAt: testNow,
	}
}

// hookRequest is a valid pre-action hook request for the operation.
func hookRequest(config simulator.Config, operation ids.OperationID, attempt ids.AttemptID, input []byte) contracts.HookRequest {
	return contracts.HookRequest{
		Kind: contracts.HookPreAction, Operation: operation, Tenant: config.Tenant, Attempt: attempt,
		Provider: config.Provider, Action: "fs.write", Scope: identity.Scope{Class: identity.ScopeGlobal},
		Input: input, InputDigest: identity.DigestOf(input), Deadline: testNow.Add(time.Minute),
	}
}

// marshalArguments returns the JSON object a request carries for arguments.
func marshalArguments(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return data
}

// scriptedLoss is a scripted transport loss: the provider acted, the product
// never saw the reply.
var scriptedLoss = errors.New("scripted transport loss")

// scriptedFailure is a valid typed provider failure for rejection scenarios.
func scriptedFailure() *contracts.Failure {
	return &contracts.Failure{Code: contracts.FailureProviderFailed, Message: "scripted provider failure"}
}
