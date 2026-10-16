package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

type attentionFixture struct {
	request   AttentionRequest
	responder ids.PrincipalID
	decision  Decision
}

func newAttentionFixture(t *testing.T) attentionFixture {
	t.Helper()
	responder := newID[ids.Principal](t)
	request, err := NewAttentionRequest(AttentionRequest{
		ID:               newID[ids.Attention](t),
		Tenant:           newID[ids.Tenant](t),
		Task:             newID[ids.Task](t),
		Attempt:          newID[ids.Attempt](t),
		Kind:             AttentionActionApproval,
		Provider:         newID[ids.Provider](t),
		Operation:        newID[ids.Operation](t),
		ActionDigest:     testDigest,
		PolicyGeneration: testGeneration,
		Responders:       []ids.PrincipalID{responder},
		ExpiresAt:        testNow.Add(time.Hour),
	}, testNow)
	if err != nil {
		t.Fatalf("NewAttentionRequest() error = %v", err)
	}
	return attentionFixture{
		request:   request,
		responder: responder,
		decision: Decision{
			ID:             newID[ids.Decision](t),
			Tenant:         request.Tenant,
			Request:        request.ID,
			RequestVersion: request.Version,
			Principal:      responder,
			Kind:           DecisionApproveOnce,
			DecidedAt:      testNow.Add(time.Minute),
		},
	}
}

func TestDecideThenDeliverAndConsume(t *testing.T) {
	f := newAttentionFixture(t)
	decided, change, err := f.request.Decide(f.decision, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if decided.State != AttentionDecided || decided.Decision != f.decision.ID || change.To != "decided" {
		t.Fatalf("Decide() = %s/%s", decided.State, decided.Decision)
	}
	if err := decided.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	request := decided
	for _, event := range []AttentionEvent{AttentionDispatch, AttentionRedeliver, AttentionDeliver, AttentionConsume} {
		request, _, err = request.Apply(event, request.Version, testNow)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	if request.State != AttentionConsumed {
		t.Fatalf("final state = %s, want consumed", request.State)
	}
}

func TestDenialIsDeliveredLikeAnyDecision(t *testing.T) {
	f := newAttentionFixture(t)
	f.decision.Kind = DecisionDeny
	decided, _, err := f.request.Decide(f.decision, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if _, _, err := decided.Apply(AttentionDispatch, decided.Version, testNow); err != nil {
		t.Fatalf("dispatching a denial: %v", err)
	}
}

func TestFirstDecisionWins(t *testing.T) {
	f := newAttentionFixture(t)
	decided, _, err := f.request.Decide(f.decision, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	second := f.decision
	second.ID = newID[ids.Decision](t)
	second.Kind = DecisionDeny
	_, _, err = decided.Decide(second, testGeneration)
	testRejectCode(t, err, RejectVersionConflict)

	second.RequestVersion = decided.Version
	_, _, err = decided.Decide(second, testGeneration)
	testRejectCode(t, err, RejectInvalidTransition)
}

func TestDecideRejections(t *testing.T) {
	cases := map[RejectionCode]func(t *testing.T, f *attentionFixture){
		RejectCrossTenant:       func(t *testing.T, f *attentionFixture) { f.decision.Tenant = newID[ids.Tenant](t) },
		RejectWrongRecord:       func(t *testing.T, f *attentionFixture) { f.decision.Request = newID[ids.Attention](t) },
		RejectUnauthorizedActor: func(t *testing.T, f *attentionFixture) { f.decision.Principal = newID[ids.Principal](t) },
		RejectDecisionNotAllowed: func(t *testing.T, f *attentionFixture) {
			f.decision.Kind = DecisionAnswer
			f.decision.AnswerDigest = testDigest
		},
		RejectRequestExpired:  func(t *testing.T, f *attentionFixture) { f.decision.DecidedAt = f.request.ExpiresAt },
		RejectVersionConflict: func(t *testing.T, f *attentionFixture) { f.decision.RequestVersion = 2 },
		RejectStalePolicy:     func(t *testing.T, f *attentionFixture) { f.request.PolicyGeneration = testGeneration - 1 },
	}
	for code, arrange := range cases {
		t.Run(string(code), func(t *testing.T) {
			f := newAttentionFixture(t)
			arrange(t, &f)
			next, _, err := f.request.Decide(f.decision, testGeneration)
			testRejectCode(t, err, code)
			if next.State != AttentionPending {
				t.Fatalf("rejected Decide() moved request to %s", next.State)
			}
		})
	}
}

func TestApproveOnlyForActionApprovals(t *testing.T) {
	f := newAttentionFixture(t)
	question := f.request
	question.Kind = AttentionQuestion
	question.Provider = ids.ProviderID{}
	question.Operation = ids.OperationID{}
	question.ActionDigest = ""
	_, _, err := question.Decide(f.decision, testGeneration)
	testRejectCode(t, err, RejectDecisionNotAllowed)

	answer := f.decision
	answer.Kind = DecisionAnswer
	answer.AnswerDigest = testDigest
	if _, _, err := question.Decide(answer, testGeneration); err != nil {
		t.Fatalf("answering a question: %v", err)
	}
}

func TestApplyRefusesDecideEvent(t *testing.T) {
	f := newAttentionFixture(t)
	_, _, err := f.request.Apply(AttentionDecide, f.request.Version, testNow)
	testRejectCode(t, err, RejectInvalidTransition)
}

func TestAttentionAndDecisionValidation(t *testing.T) {
	f := newAttentionFixture(t)
	request, decision := f.request, f.decision
	for name, err := range map[string]error{
		"approval without operation": func() error { x := request; x.Operation = ids.OperationID{}; return x.Validate() }(),
		"approval without digest":    func() error { x := request; x.ActionDigest = ""; return x.Validate() }(),
		"question with operation": func() error {
			x := request
			x.Kind = AttentionQuestion
			x.ActionDigest = ""
			x.Provider = ids.ProviderID{}
			return x.Validate()
		}(),
		"question with provider": func() error {
			x := request
			x.Kind = AttentionQuestion
			x.ActionDigest = ""
			x.Operation = ids.OperationID{}
			return x.Validate()
		}(),
		"approval without provider":   func() error { x := request; x.Provider = ids.ProviderID{}; return x.Validate() }(),
		"no policy generation":        func() error { x := request; x.PolicyGeneration = 0; return x.Validate() }(),
		"no responders":               func() error { x := request; x.Responders = nil; return x.Validate() }(),
		"pending with decision":       func() error { x := request; x.Decision = decision.ID; return x.Validate() }(),
		"decided without decision":    func() error { x := request; x.State = AttentionDecided; return x.Validate() }(),
		"expiry not after creation":   func() error { x := request; x.ExpiresAt = x.CreatedAt; return x.Validate() }(),
		"answer without digest":       func() error { x := decision; x.Kind = DecisionAnswer; return x.Validate() }(),
		"approval with answer digest": func() error { x := decision; x.AnswerDigest = testDigest; return x.Validate() }(),
		"decision zero version":       func() error { x := decision; x.RequestVersion = 0; return x.Validate() }(),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestApprovalConfersAllowOnceGrantUnderItsPolicy(t *testing.T) {
	f := newAttentionFixture(t)
	decided, _, err := f.request.Decide(f.decision, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	grant, err := decided.AllowOnceGrant(f.decision, newID[ids.Grant](t), "fs.write", testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("AllowOnceGrant() error = %v", err)
	}
	if grant.Operation != f.request.Operation || grant.InputDigest != f.request.ActionDigest ||
		grant.Provider != f.request.Provider || grant.Attempt != f.request.Attempt ||
		grant.GrantedBy != f.responder || grant.PolicyGeneration != testGeneration ||
		grant.Mode != identity.GrantAllowOnce {
		t.Fatalf("AllowOnceGrant() = %+v, not bound to the approved request", grant)
	}

	policy := identity.Policy{
		Tenant: f.request.Tenant, Generation: testGeneration,
		Settings: map[identity.ActionClass]identity.Permission{"fs.write": identity.PermissionAsk},
		Ceiling:  map[identity.ActionClass]identity.Permission{"fs.write": identity.PermissionAsk},
	}
	if err := identity.IssueGrant(grant, policy); err != nil {
		t.Fatalf("IssueGrant() under the same policy: %v", err)
	}
	policy.Generation++
	if err := identity.IssueGrant(grant, policy); !errors.Is(err, identity.DenialStaleGeneration) {
		t.Fatalf("IssueGrant() after a policy change = %v, want stale_generation", err)
	}
}

func TestAllowOnceGrantRequiresARecordedApproval(t *testing.T) {
	f := newAttentionFixture(t)
	if _, err := f.request.AllowOnceGrant(f.decision, newID[ids.Grant](t), "fs.write", testNow.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant from an undecided request = %v, want ErrInvalid", err)
	}
	deny := f.decision
	deny.Kind = DecisionDeny
	denied, _, err := f.request.Decide(deny, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if _, err := denied.AllowOnceGrant(deny, newID[ids.Grant](t), "fs.write", testNow.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant from a denial = %v, want ErrInvalid", err)
	}
	other := f.decision
	other.ID = newID[ids.Decision](t)
	approved, _, _ := f.request.Decide(f.decision, testGeneration)
	if _, err := approved.AllowOnceGrant(other, newID[ids.Grant](t), "fs.write", testNow.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant from an unrecorded decision = %v, want ErrInvalid", err)
	}
}
