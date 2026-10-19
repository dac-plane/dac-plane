package identity

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var (
	testNow    = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	testDigest = Digest("sha256:" + strings.Repeat("a", 64))
	otherInput = Digest("sha256:" + strings.Repeat("b", 64))
)

// fixture is one tenant's valid request, grants and policy. Tests mutate
// copies to violate exactly one predicate at a time.
type fixture struct {
	request   ActionRequest
	standing  AuthorityGrant
	allowOnce AuthorityGrant
	policy    Policy
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	tenant := newID[ids.Tenant](t)
	attempt := newID[ids.Attempt](t)
	provider := newID[ids.Provider](t)
	operation := newID[ids.Operation](t)
	approver := newID[ids.Principal](t)
	scope := Scope{Class: ScopeSubject, Target: "alice@example.com"}

	grant := AuthorityGrant{
		Tenant:           tenant,
		Attempt:          attempt,
		Provider:         provider,
		Action:           "message.send",
		GrantedBy:        approver,
		PolicyGeneration: 3,
		IssuedAt:         testNow.Add(-time.Minute),
		ExpiresAt:        testNow.Add(time.Hour),
	}
	standing := grant
	standing.ID = newID[ids.Grant](t)
	standing.Mode = GrantStanding
	standing.Scope = scope

	allowOnce := grant
	allowOnce.ID = newID[ids.Grant](t)
	allowOnce.Mode = GrantAllowOnce
	allowOnce.Operation = operation
	allowOnce.InputDigest = testDigest

	return fixture{
		request: ActionRequest{
			Tenant:      tenant,
			Attempt:     attempt,
			Provider:    provider,
			Operation:   operation,
			Action:      "message.send",
			Scope:       scope,
			InputDigest: testDigest,
		},
		standing:  standing,
		allowOnce: allowOnce,
		policy: Policy{
			Tenant:     tenant,
			Generation: 3,
			Settings:   map[ActionClass]Permission{"message.send": PermissionAlways},
			Ceiling:    map[ActionClass]Permission{"message.send": PermissionAlways},
		},
	}
}

func TestAuthorizeAllowsStandingGrantWithinExactScope(t *testing.T) {
	f := newFixture(t)
	use, retired, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != UseAllowed || use.Reason != "" {
		t.Fatalf("Authorize() = %s/%s, want allowed", use.Outcome, use.Reason)
	}
	if use.Source != SourceStandingGrant || use.Grant != f.standing.ID || use.GrantedBy != f.standing.GrantedBy {
		t.Fatalf("Authorize() attribution = %s/%s/%s", use.Source, use.Grant, use.GrantedBy)
	}
	if retired != nil {
		t.Fatal("Authorize() retired a standing grant")
	}
	if err := use.Validate(); err != nil {
		t.Fatalf("AuthorityUse.Validate() error = %v", err)
	}

	// The standing grant keeps authorizing further calls, with any input, in
	// the same scope.
	next := f.request
	next.Operation = newID[ids.Operation](t)
	next.InputDigest = otherInput
	if use, _, _ := Authorize(next, &f.standing, f.policy, testNow); use.Outcome != UseAllowed {
		t.Fatalf("second standing use = %s/%s, want allowed", use.Outcome, use.Reason)
	}
}

func TestAuthorizeRetiresAllowOnceGrant(t *testing.T) {
	f := newFixture(t)
	f.policy.Settings["message.send"] = PermissionAsk
	use, retired, err := Authorize(f.request, &f.allowOnce, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != UseAllowed || use.Source != SourceOneTimeApproval {
		t.Fatalf("Authorize() = %s/%s, want allowed one-time approval", use.Outcome, use.Source)
	}
	if retired == nil || !retired.RetiredAt.Equal(testNow) {
		t.Fatalf("Authorize() retired grant = %+v, want retired at %s", retired, testNow)
	}
	if !f.allowOnce.RetiredAt.IsZero() {
		t.Fatal("Authorize() mutated the presented grant")
	}

	// The consumed grant cannot authorize a second action, even the same one.
	second, again, err := Authorize(f.request, retired, f.policy, testNow)
	if err != nil {
		t.Fatalf("second Authorize() error = %v", err)
	}
	if second.Outcome != UseDenied || second.Reason != DenialRetired || again != nil {
		t.Fatalf("second Authorize() = %s/%s, want denied retired", second.Outcome, second.Reason)
	}
}

func TestAuthorizeAllowsOnceForCallScopedAction(t *testing.T) {
	f := newFixture(t)
	f.request.Scope = Scope{Class: ScopeCall}
	use, retired, err := Authorize(f.request, &f.allowOnce, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != UseAllowed || retired == nil {
		t.Fatalf("Authorize() = %s/%s, want allowed and retired", use.Outcome, use.Reason)
	}
}

func TestAuthorizeDenials(t *testing.T) {
	cases := map[DenialReason]func(t *testing.T, f *fixture) *AuthorityGrant{
		DenialNoGrant: func(t *testing.T, f *fixture) *AuthorityGrant { return nil },
		DenialCrossTenant: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.Tenant = newID[ids.Tenant](t)
			return &f.standing
		},
		DenialWrongAttempt: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.Attempt = newID[ids.Attempt](t)
			return &f.standing
		},
		DenialWrongAudience: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.Provider = newID[ids.Provider](t)
			return &f.standing
		},
		DenialWrongAction: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.Action = "message.delete"
			f.policy.Settings["message.delete"] = PermissionAlways
			f.policy.Ceiling["message.delete"] = PermissionAlways
			return &f.standing
		},
		DenialWrongOperation: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.Operation = newID[ids.Operation](t)
			return &f.allowOnce
		},
		DenialInputMismatch: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.InputDigest = otherInput
			return &f.allowOnce
		},
		DenialScopeMismatch: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.Scope.Target = "bob@example.com"
			return &f.standing
		},
		DenialRevoked: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.RevokedAt = testNow.Add(-time.Second)
			return &f.standing
		},
		DenialRetired: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.allowOnce.RetiredAt = testNow.Add(-time.Second)
			return &f.allowOnce
		},
		DenialNotYetValid: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.IssuedAt = testNow.Add(time.Second)
			return &f.standing
		},
		DenialExpired: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.ExpiresAt = testNow
			return &f.standing
		},
		DenialStaleGeneration: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.policy.Generation = 4
			return &f.standing
		},
		DenialAboveCeiling: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.policy.Ceiling["message.send"] = PermissionAsk
			return &f.standing
		},
		DenialByPolicy: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.policy.Denies = []DenyPolicy{{Action: "message.send", Target: "alice@example.com"}}
			return &f.standing
		},
	}

	for _, reason := range DenialReasons() {
		if reason == DenialHookRejected {
			continue
		} // Covered by TestAuthorityHookDenial; not an authority predicate.
		arrange, ok := cases[reason]
		if !ok {
			t.Errorf("no Authorize case for denial reason %s", reason)
			continue
		}
		t.Run(string(reason), func(t *testing.T) {
			f := newFixture(t)
			grant := arrange(t, &f)
			use, retired, err := Authorize(f.request, grant, f.policy, testNow)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if use.Outcome != UseDenied || use.Reason != reason {
				t.Fatalf("Authorize() = %s/%s, want denied %s", use.Outcome, use.Reason, reason)
			}
			if retired != nil {
				t.Fatal("Authorize() retired a grant on denial")
			}
			if err := use.Validate(); err != nil {
				t.Fatalf("AuthorityUse.Validate() error = %v", err)
			}
		})
	}
}

func TestAuthorityHookDenial(t *testing.T) {
	f := newFixture(t)
	use, err := DenyAfterHook(f.request, &f.allowOnce, f.policy, testNow)
	if err != nil || use.Outcome != UseDenied || use.Reason != DenialHookRejected || !f.allowOnce.RetiredAt.IsZero() {
		t.Fatalf("hook denial = %+v, %v", use, err)
	}
}

func TestAuthorityOneTimeIssuanceHonorsScopedNever(t *testing.T) {
	f := newFixture(t)
	f.policy.Denies = []DenyPolicy{{Action: f.allowOnce.Action, Target: f.request.Scope.Target}}
	if err := IssueGrantForScope(f.allowOnce, f.request.Scope, f.policy); err != DenialByPolicy {
		t.Fatalf("scoped never issued one-time authority: %v", err)
	}
}

func TestNeverPermissionOverridesAnyGrant(t *testing.T) {
	for name, adjust := range map[string]func(p *Policy){
		"setting never":         func(p *Policy) { p.Settings["message.send"] = PermissionNever },
		"ceiling never":         func(p *Policy) { p.Ceiling["message.send"] = PermissionNever },
		"action missing":        func(p *Policy) { delete(p.Settings, "message.send") },
		"ceiling entry missing": func(p *Policy) { delete(p.Ceiling, "message.send") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			adjust(&f.policy)
			for _, grant := range []*AuthorityGrant{&f.standing, &f.allowOnce} {
				use, _, err := Authorize(f.request, grant, f.policy, testNow)
				if err != nil {
					t.Fatalf("Authorize() error = %v", err)
				}
				if use.Reason != DenialByPolicy {
					t.Fatalf("%s grant reason = %s, want denied_by_policy", grant.Mode, use.Reason)
				}
			}
		})
	}
}

func TestAskPermissionAllowsOnlyOneTimeApproval(t *testing.T) {
	f := newFixture(t)
	f.policy.Settings["message.send"] = PermissionAsk
	if use, _, _ := Authorize(f.request, &f.allowOnce, f.policy, testNow); use.Outcome != UseAllowed {
		t.Fatalf("allow-once under ask = %s/%s, want allowed", use.Outcome, use.Reason)
	}
	if use, _, _ := Authorize(f.request, &f.standing, f.policy, testNow); use.Reason != DenialAboveCeiling {
		t.Fatalf("standing under ask = %s/%s, want above_ceiling", use.Outcome, use.Reason)
	}
}

func TestPolicyEffectiveClampsSettingToCeiling(t *testing.T) {
	for _, tc := range []struct {
		setting, ceiling, want Permission
	}{
		{PermissionAlways, PermissionAsk, PermissionAsk},
		{PermissionAsk, PermissionAlways, PermissionAsk},
		{PermissionAlways, PermissionAlways, PermissionAlways},
		{PermissionNever, PermissionAlways, PermissionNever},
	} {
		policy := Policy{
			Settings: map[ActionClass]Permission{"fs.write": tc.setting},
			Ceiling:  map[ActionClass]Permission{"fs.write": tc.ceiling},
		}
		if got := policy.Effective("fs.write"); got != tc.want {
			t.Errorf("Effective(setting %s, ceiling %s) = %s, want %s", tc.setting, tc.ceiling, got, tc.want)
		}
	}
}

func TestAuthorizeIgnoresNeverRuleForOtherTargets(t *testing.T) {
	f := newFixture(t)
	f.policy.Denies = []DenyPolicy{
		{Action: "message.send", Target: "bob@example.com"},
		{Action: "message.delete", Target: "alice@example.com"},
	}
	use, _, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != UseAllowed {
		t.Fatalf("Authorize() = %s/%s, want allowed", use.Outcome, use.Reason)
	}
}

func TestAuthorizeRejectsCrossTenantPolicy(t *testing.T) {
	f := newFixture(t)
	f.policy.Tenant = newID[ids.Tenant](t)
	use, _, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Reason != DenialCrossTenant {
		t.Fatalf("Authorize() reason = %s, want cross_tenant", use.Reason)
	}
}

func TestAuthorizeRejectsMalformedInputs(t *testing.T) {
	f := newFixture(t)
	badGrant := f.standing
	badGrant.Mode = "always"
	badRequest := f.request
	badRequest.InputDigest = "md5:abc"

	for name, call := range map[string]func() error{
		"request": func() error {
			_, _, err := Authorize(ActionRequest{}, &f.standing, f.policy, testNow)
			return err
		},
		"request digest": func() error {
			_, _, err := Authorize(badRequest, &f.standing, f.policy, testNow)
			return err
		},
		"policy": func() error {
			_, _, err := Authorize(f.request, &f.standing, Policy{}, testNow)
			return err
		},
		"grant": func() error {
			_, _, err := Authorize(f.request, &badGrant, f.policy, testNow)
			return err
		},
		"time": func() error {
			_, _, err := Authorize(f.request, &f.standing, f.policy, time.Time{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Authorize() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestIssueGrant(t *testing.T) {
	cases := map[string]struct {
		arrange func(t *testing.T, f *fixture) AuthorityGrant
		want    error
	}{
		"standing under always": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant { return f.standing },
		},
		"allow once under ask": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Settings["message.send"] = PermissionAsk
				return f.allowOnce
			},
		},
		"other tenant policy": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Tenant = newID[ids.Tenant](t)
				return f.standing
			},
			want: DenialCrossTenant,
		},
		"stale generation": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Generation = 4
				return f.standing
			},
			want: DenialStaleGeneration,
		},
		"standing above ceiling": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Ceiling["message.send"] = PermissionAsk
				return f.standing
			},
			want: DenialAboveCeiling,
		},
		"allow once under never": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Settings["message.send"] = PermissionNever
				return f.allowOnce
			},
			want: DenialByPolicy,
		},
		"standing on denied target": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Denies = []DenyPolicy{{Action: "message.send", Target: "alice@example.com"}}
				return f.standing
			},
			want: DenialByPolicy,
		},
		"already revoked": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.standing.RevokedAt = testNow
				return f.standing
			},
			want: ErrInvalid,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			grant := tc.arrange(t, &f)
			err := IssueGrant(grant, f.policy)
			if tc.want == nil && err != nil {
				t.Fatalf("IssueGrant() error = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("IssueGrant() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGrantValidateRejectsInconsistentShapes(t *testing.T) {
	f := newFixture(t)
	cases := map[string]func() AuthorityGrant{
		"standing on call-scoped action": func() AuthorityGrant {
			g := f.standing
			g.Scope = Scope{Class: ScopeCall}
			return g
		},
		"standing with operation": func() AuthorityGrant {
			g := f.standing
			g.Operation = f.request.Operation
			return g
		},
		"standing with input digest": func() AuthorityGrant {
			g := f.standing
			g.InputDigest = testDigest
			return g
		},
		"retired standing grant": func() AuthorityGrant {
			g := f.standing
			g.RetiredAt = testNow
			return g
		},
		"allow once without operation": func() AuthorityGrant {
			g := f.allowOnce
			g.Operation = ids.OperationID{}
			return g
		},
		"allow once without digest": func() AuthorityGrant {
			g := f.allowOnce
			g.InputDigest = ""
			return g
		},
		"allow once with scope": func() AuthorityGrant {
			g := f.allowOnce
			g.Scope = Scope{Class: ScopeGlobal}
			return g
		},
		"expiry not after issuance": func() AuthorityGrant {
			g := f.standing
			g.ExpiresAt = g.IssuedAt
			return g
		},
		"missing granting principal": func() AuthorityGrant {
			g := f.standing
			g.GrantedBy = ids.PrincipalID{}
			return g
		},
		"malformed action": func() AuthorityGrant {
			g := f.standing
			g.Action = "Message Send"
			return g
		},
		"subject scope without target": func() AuthorityGrant {
			g := f.standing
			g.Scope = Scope{Class: ScopeSubject}
			return g
		},
		"global scope with target": func() AuthorityGrant {
			g := f.standing
			g.Scope = Scope{Class: ScopeGlobal, Target: "x"}
			return g
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if err := build().Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDenyPolicyRequiresTarget(t *testing.T) {
	if err := (DenyPolicy{Action: "message.send"}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() error = %v, want ErrInvalid", err)
	}
}

func TestDigestOfIsAValidDigestOfExactBytes(t *testing.T) {
	digest := DigestOf([]byte("abc"))
	if digest != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("DigestOf(abc) = %s", digest)
	}
	if err := digest.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if DigestOf([]byte(`{"a":1}`)) == DigestOf([]byte(`{ "a": 1 }`)) {
		t.Fatal("DigestOf canonicalized its input")
	}
}
