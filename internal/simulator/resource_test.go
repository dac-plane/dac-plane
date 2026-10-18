package simulator_test

import (
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestResourceRefusesStaleTokens(t *testing.T) {
	resource := simulator.NewResource("env-1")
	operation := mustID[ids.Operation](t)
	planned := contracts.ActionPrecondition{Operation: operation, Resource: "env-1", Token: resource.Token()}

	resource.Change()

	if _, failure := resource.Begin(planned); failure == nil || failure.Code != contracts.FailureStalePrecondition {
		t.Fatalf("Begin() failure = %v, want stale_precondition", failure)
	}
	if got := resource.Ledger().Effects(operation); got != 0 {
		t.Fatalf("effects after the stale begin = %d, want 0", got)
	}

	observed := planned
	observed.Token = resource.Token()
	ticket, failure := resource.Begin(observed)
	if failure != nil {
		t.Fatalf("Begin() after observing again failure = %v, want nil", failure)
	}
	observedToken := resource.Token()
	if err := ticket.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if got := resource.Ledger().Effects(operation); got != 1 {
		t.Fatalf("effects after the finish = %d, want 1", got)
	}
	if resource.Token() == observedToken {
		t.Fatalf("Token() did not rotate after the action finished")
	}
	applied := resource.Ledger().Applied()
	if len(applied) != 1 || applied[0].Kind != simulator.ResourceAction || applied[0].Detail != "env-1" {
		t.Fatalf("applied = %+v, want one resource_action on env-1", applied)
	}
}

func TestResourceSerializesActions(t *testing.T) {
	resource := simulator.NewResource("db-1")
	first := mustID[ids.Operation](t)
	second := mustID[ids.Operation](t)
	token := resource.Token()

	ticket, failure := resource.Begin(contracts.ActionPrecondition{Operation: first, Resource: "db-1", Token: token})
	if failure != nil {
		t.Fatalf("Begin() failure = %v, want nil", failure)
	}

	if _, failure := resource.Begin(contracts.ActionPrecondition{Operation: second, Resource: "db-1", Token: token}); failure == nil || failure.Code != contracts.FailureResourceBusy {
		t.Fatalf("Begin() while one action is in flight = %v, want resource_busy", failure)
	}
	if got := resource.Ledger().Effects(second); got != 0 {
		t.Fatalf("effects after the refused begin = %d, want 0", got)
	}

	if err := ticket.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if err := ticket.Finish(); err == nil {
		t.Fatalf("second Finish() error = nil, want a scenario bug")
	}

	// Finishing rotated the token and freed the resource: the next action
	// observes again and proceeds.
	next, failure := resource.Begin(contracts.ActionPrecondition{Operation: second, Resource: "db-1", Token: resource.Token()})
	if failure != nil {
		t.Fatalf("Begin() after the finish failure = %v, want nil", failure)
	}
	if err := next.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if got := resource.Ledger().Effects(first); got != 1 {
		t.Fatalf("effects for the first action = %d, want 1", got)
	}
	if got := resource.Ledger().Effects(second); got != 1 {
		t.Fatalf("effects for the second action = %d, want 1", got)
	}
}
