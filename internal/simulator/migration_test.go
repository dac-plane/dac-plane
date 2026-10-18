package simulator_test

import (
	"context"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestMigrationPreviewAndHolds(t *testing.T) {
	config := testConfig(t)
	from := mustID[ids.Provider](t)
	principal := lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: mustID[ids.Principal](t)}
	system := lifecycle.Actor{Kind: lifecycle.ActorSystem}
	attempt := mustID[ids.Attempt](t)
	observe := mustID[ids.Operation](t)
	provision := mustID[ids.Operation](t)

	// The target harness's environment: a preview reads it, an execution
	// provisions it.
	environment := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		observe:   {Observations: []lifecycle.EnvironmentObservation{observation(config, attempt, 1, lifecycle.RunRunning)}},
		provision: {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned},
	})

	pass, err := lifecycle.NewMigrationPass(mustID[ids.Migration](t), config.Tenant, from, config.Provider, testNow)
	if err != nil {
		t.Fatalf("NewMigrationPass() error = %v", err)
	}
	if pass.State != lifecycle.MigrationPlanned {
		t.Fatalf("State = %q, want planned", pass.State)
	}

	// Execution needs a fresh preview: from planned it is refused.
	if _, _, err := pass.Apply(lifecycle.MigrationExecute, pass.Version, principal, testNow); lifecycle.RejectionOf(err) != lifecycle.RejectInvalidTransition {
		t.Fatalf("execute before a preview error = %v, want invalid_transition", err)
	}

	// The preview is a read-only look at the target: observing applies
	// nothing.
	pass, _, err = pass.Apply(lifecycle.MigrationPreview, pass.Version, principal, testNow)
	if err != nil {
		t.Fatalf("Apply(preview) error = %v", err)
	}
	if _, err := environment.Send(context.Background(), newRoleRequest(t, config, observe, contracts.RoleEnvironment, contracts.MethodObserve, nil)); err != nil {
		t.Fatalf("Send(observe) error = %v", err)
	}

	// A hold interrupts: execution is refused while held, and a release
	// returns to planned, so the next execution also needs a fresh preview.
	pass, _, err = pass.Apply(lifecycle.MigrationHold, pass.Version, principal, testNow)
	if err != nil {
		t.Fatalf("Apply(hold) error = %v", err)
	}
	if pass.State != lifecycle.MigrationHeld {
		t.Fatalf("State = %q, want held", pass.State)
	}
	if _, _, err := pass.Apply(lifecycle.MigrationExecute, pass.Version, principal, testNow); lifecycle.RejectionOf(err) != lifecycle.RejectInvalidTransition {
		t.Fatalf("execute while held error = %v, want invalid_transition", err)
	}
	pass, _, err = pass.Apply(lifecycle.MigrationRelease, pass.Version, principal, testNow)
	if err != nil {
		t.Fatalf("Apply(release) error = %v", err)
	}
	if pass.State != lifecycle.MigrationPlanned {
		t.Fatalf("State = %q, want planned after the release", pass.State)
	}
	if _, _, err := pass.Apply(lifecycle.MigrationExecute, pass.Version, principal, testNow); lifecycle.RejectionOf(err) != lifecycle.RejectInvalidTransition {
		t.Fatalf("execute after a release error = %v, want invalid_transition", err)
	}

	// A fresh preview admits the execution, and the execution is the step
	// that provisions the target environment.
	pass, _, err = pass.Apply(lifecycle.MigrationPreview, pass.Version, principal, testNow)
	if err != nil {
		t.Fatalf("Apply(preview) error = %v", err)
	}
	pass, _, err = pass.Apply(lifecycle.MigrationExecute, pass.Version, principal, testNow)
	if err != nil {
		t.Fatalf("Apply(execute) error = %v", err)
	}
	if pass.State != lifecycle.MigrationExecuting {
		t.Fatalf("State = %q, want executing", pass.State)
	}
	if _, err := environment.Send(context.Background(), newRoleRequest(t, config, provision, contracts.RoleEnvironment, contracts.MethodProvision, nil)); err != nil {
		t.Fatalf("Send(provision) error = %v", err)
	}

	// The outcome is reported by the system.
	pass, _, err = pass.Apply(lifecycle.MigrationComplete, pass.Version, system, testNow)
	if err != nil {
		t.Fatalf("Apply(complete) error = %v", err)
	}
	if pass.State != lifecycle.MigrationCompleted {
		t.Fatalf("State = %q, want completed", pass.State)
	}

	// The ledger proves which step provisioned: the preview observed with no
	// effect, and only the execution applied one.
	if got := environment.Ledger().Effects(observe); got != 0 {
		t.Fatalf("preview effects = %d, want 0", got)
	}
	if got := environment.Ledger().Deliveries(observe); got != 1 {
		t.Fatalf("preview deliveries = %d, want 1", got)
	}
	applied := environment.Ledger().Applied()
	if len(applied) != 1 || applied[0].Kind != simulator.EnvironmentProvisioned {
		t.Fatalf("applied = %+v, want one environment_provisioned effect", applied)
	}
	if pass.Version != 7 {
		t.Fatalf("Version = %d, want 7", pass.Version)
	}
	if err := pass.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestStaleClaimantIsFenced(t *testing.T) {
	config := testConfig(t)
	ttl := time.Minute
	claim, err := lifecycle.NewMaintenanceClaim(mustID[ids.Claim](t), config.Tenant, config.Provider, "resource-1", testNow)
	if err != nil {
		t.Fatalf("NewMaintenanceClaim() error = %v", err)
	}
	claim, _, err = claim.Claim("worker-a", ttl, testNow)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claim.State != lifecycle.ClaimClaimed || claim.Claimant != "worker-a" || claim.Epoch != 1 {
		t.Fatalf("claim = %+v, want worker-a holding epoch 1", claim)
	}

	// A held claim cannot be taken before its lease expires.
	if _, _, err := claim.Claim("worker-b", ttl, testNow.Add(ttl/2)); lifecycle.RejectionOf(err) != lifecycle.RejectClaimHeld {
		t.Fatalf("early claim error = %v, want claim_held", err)
	}

	// At expiry the claim is takeable, and the takeover fences the old
	// holder by advancing the epoch.
	claim, _, err = claim.Claim("worker-b", ttl, testNow.Add(ttl))
	if err != nil {
		t.Fatalf("takeover error = %v", err)
	}
	if claim.Claimant != "worker-b" || claim.Epoch != 2 {
		t.Fatalf("claim = %+v, want worker-b holding epoch 2", claim)
	}

	calls := []struct {
		name string
		do   func() (lifecycle.MaintenanceClaim, lifecycle.Change, error)
	}{
		{"renew", func() (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
			return claim.Renew("worker-a", 1, ttl, testNow.Add(2*ttl))
		}},
		{"complete", func() (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
			return claim.Complete("worker-a", 1, testNow.Add(2*ttl))
		}},
		{"release", func() (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
			return claim.Release("worker-a", 1, testNow.Add(2*ttl))
		}},
	}
	for _, call := range calls {
		if _, _, err := call.do(); lifecycle.RejectionOf(err) != lifecycle.RejectStaleClaim {
			t.Fatalf("%s as the stale holder error = %v, want stale_claim", call.name, err)
		}
	}

	// The current holder still completes while its own lease runs, and a
	// completed claim is terminal.
	claim, _, err = claim.Complete("worker-b", 2, testNow.Add(ttl+ttl/2))
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if claim.State != lifecycle.ClaimCompleted {
		t.Fatalf("State = %q, want completed", claim.State)
	}
	if _, _, err := claim.Claim("worker-c", ttl, testNow.Add(3*ttl)); lifecycle.RejectionOf(err) != lifecycle.RejectTerminalState {
		t.Fatalf("claim after completion error = %v, want terminal_state", err)
	}
}
