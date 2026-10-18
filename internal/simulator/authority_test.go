package simulator_test

import (
	"context"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// actionPractice is the governed action class the authority tests authorize.
const actionPractice = identity.ActionClass("fs.write")

func TestAuthorityRetirementAllowsOnce(t *testing.T) {
	config := testConfig(t)
	attempt := mustID[ids.Attempt](t)
	entityOperation := mustID[ids.Operation](t)
	principal := mustID[ids.Principal](t)
	input := []byte(`{"path":"a.txt"}`)

	policy := identity.Policy{
		Tenant: config.Tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{actionPractice: identity.PermissionAsk},
		Ceiling:  map[identity.ActionClass]identity.Permission{actionPractice: identity.PermissionAsk},
	}
	grant := identity.AuthorityGrant{
		ID: mustID[ids.Grant](t), Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider,
		Action: actionPractice, Mode: identity.GrantAllowOnce, Operation: entityOperation,
		InputDigest: identity.DigestOf(input), GrantedBy: principal, PolicyGeneration: 1,
		IssuedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
	}
	if err := identity.IssueGrant(grant, policy); err != nil {
		t.Fatalf("IssueGrant() error = %v", err)
	}
	request := identity.ActionRequest{
		Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider, Operation: entityOperation,
		Action: actionPractice, Scope: identity.Scope{Class: identity.ScopeGlobal}, InputDigest: identity.DigestOf(input),
	}
	endpoint := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		entityOperation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})

	// use authorizes against the presented grant and, only when allowed,
	// dispatches the action to the endpoint.
	use := func(presented *identity.AuthorityGrant, at time.Time) (identity.AuthorityUse, *identity.AuthorityGrant) {
		t.Helper()
		decision, retired, err := identity.Authorize(request, presented, policy, at)
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		if err := decision.Validate(); err != nil {
			t.Fatalf("AuthorityUse does not validate: %v", err)
		}
		if decision.Outcome == identity.UseAllowed {
			if _, err := endpoint.Send(context.Background(), newRequest(t, config, entityOperation, contracts.MethodStartTurn)); err != nil {
				t.Fatalf("dispatch Send() error = %v", err)
			}
		}
		return decision, retired
	}

	first, retired := use(&grant, testNow)
	if first.Outcome != identity.UseAllowed || first.Source != identity.SourceOneTimeApproval {
		t.Fatalf("first use = %+v, want an allowed one-time approval", first)
	}
	if retired == nil || !retired.RetiredAt.Equal(testNow) {
		t.Fatalf("retired grant = %+v, want retirement recorded at the decision time", retired)
	}
	if err := retired.Validate(); err != nil {
		t.Fatalf("retired grant does not validate: %v", err)
	}

	// The retirement is what the product persists; presenting it again must
	// not authorize a second action.
	second, again := use(retired, testNow.Add(time.Minute))
	if second.Outcome != identity.UseDenied || second.Reason != identity.DenialRetired {
		t.Fatalf("second use = %+v, want a retired refusal", second)
	}
	if again != nil {
		t.Errorf("a refused use returned a retirement: %+v", again)
	}
	if got := endpoint.Ledger().Effects(entityOperation); got != 1 {
		t.Errorf("endpoint effects = %d, want 1", got)
	}
	if got := endpoint.Ledger().Deliveries(entityOperation); got != 1 {
		t.Errorf("endpoint deliveries = %d, want 1", got)
	}
}

func TestStandingGrantRevocationDenies(t *testing.T) {
	config := testConfig(t)
	attempt := mustID[ids.Attempt](t)
	entityOperation := mustID[ids.Operation](t)
	principal := mustID[ids.Principal](t)
	input := []byte(`{"path":"a.txt"}`)

	policy := identity.Policy{
		Tenant: config.Tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{actionPractice: identity.PermissionAlways},
		Ceiling:  map[identity.ActionClass]identity.Permission{actionPractice: identity.PermissionAlways},
	}
	grant := identity.AuthorityGrant{
		ID: mustID[ids.Grant](t), Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider,
		Action: actionPractice, Mode: identity.GrantStanding, Scope: identity.Scope{Class: identity.ScopeGlobal},
		GrantedBy: principal, PolicyGeneration: 1, IssuedAt: testNow, ExpiresAt: testNow.Add(time.Hour),
	}
	if err := identity.IssueGrant(grant, policy); err != nil {
		t.Fatalf("IssueGrant() error = %v", err)
	}
	request := identity.ActionRequest{
		Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider, Operation: entityOperation,
		Action: actionPractice, Scope: identity.Scope{Class: identity.ScopeGlobal}, InputDigest: identity.DigestOf(input),
	}
	endpoint := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		entityOperation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})

	// use authorizes under the given policy at at and, only when allowed,
	// dispatches the action to the endpoint.
	use := func(presented *identity.AuthorityGrant, under identity.Policy, at time.Time) identity.AuthorityUse {
		t.Helper()
		decision, retired, err := identity.Authorize(request, presented, under, at)
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		if retired != nil {
			t.Errorf("standing use returned a retirement: %+v", retired)
		}
		if err := decision.Validate(); err != nil {
			t.Fatalf("AuthorityUse does not validate: %v", err)
		}
		if decision.Outcome == identity.UseAllowed {
			if _, err := endpoint.Send(context.Background(), newRequest(t, config, entityOperation, contracts.MethodStartTurn)); err != nil {
				t.Fatalf("dispatch Send() error = %v", err)
			}
		}
		return decision
	}

	first := use(&grant, policy, testNow)
	if first.Outcome != identity.UseAllowed || first.Source != identity.SourceStandingGrant {
		t.Fatalf("first use = %+v, want an allowed standing grant", first)
	}

	revoked := grant
	revoked.RevokedAt = testNow.Add(time.Minute)
	if second := use(&revoked, policy, testNow.Add(2*time.Minute)); second.Outcome != identity.UseDenied || second.Reason != identity.DenialRevoked {
		t.Fatalf("revoked use = %+v, want a revoked refusal", second)
	}

	newer := policy
	newer.Generation++
	if stale := use(&grant, newer, testNow.Add(3*time.Minute)); stale.Outcome != identity.UseDenied || stale.Reason != identity.DenialStaleGeneration {
		t.Fatalf("older-generation use = %+v, want a stale-generation refusal", stale)
	}

	if got := endpoint.Ledger().Effects(entityOperation); got != 1 {
		t.Errorf("endpoint effects = %d, want 1", got)
	}
	if got := endpoint.Ledger().Deliveries(entityOperation); got != 1 {
		t.Errorf("endpoint deliveries = %d, want 1", got)
	}
}
