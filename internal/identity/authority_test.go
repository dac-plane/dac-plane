package identity

import (
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

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

	grant := AuthorityGrant{
		Tenant:           tenant,
		Attempt:          attempt,
		Provider:         provider,
		Action:           "fs.write",
		GrantedBy:        approver,
		PolicyGeneration: 3,
		IssuedAt:         testNow.Add(-time.Minute),
		ExpiresAt:        testNow.Add(time.Hour),
	}
	standing := grant
	standing.ID = newID[ids.Grant](t)
	standing.Mode = GrantStanding
	standing.Scope = Scope{Class: ScopeSubject, Value: "repo/main.go"}

	allowOnce := grant
	allowOnce.ID = newID[ids.Grant](t)
	allowOnce.Mode = GrantAllowOnce
	allowOnce.Scope = Scope{Class: ScopeCall}
	allowOnce.Operation = operation

	return fixture{
		request: ActionRequest{
			Tenant:    tenant,
			Attempt:   attempt,
			Provider:  provider,
			Operation: operation,
			Action:    "fs.write",
			Setting:   "workspace",
			Subject:   "repo/main.go",
		},
		standing:  standing,
		allowOnce: allowOnce,
		policy: Policy{
			Tenant:     tenant,
			Generation: 3,
			Ceiling:    map[ActionClass]ScopeClass{"fs.write": ScopeSetting},
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

	// The standing grant keeps authorizing further calls in the same scope.
	next := f.request
	next.Operation = newID[ids.Operation](t)
	if use, _, _ := Authorize(next, &f.standing, f.policy, testNow); use.Outcome != UseAllowed {
		t.Fatalf("second standing use = %s/%s, want allowed", use.Outcome, use.Reason)
	}
}

func TestAuthorizeRetiresAllowOnceGrant(t *testing.T) {
	f := newFixture(t)
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
			f.request.Action = "fs.delete"
			f.policy.Ceiling["fs.delete"] = ScopeGlobal
			return &f.standing
		},
		DenialWrongOperation: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.Operation = newID[ids.Operation](t)
			return &f.allowOnce
		},
		DenialScopeMismatch: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.request.Subject = "repo/other.go"
			return &f.standing
		},
		DenialRevoked: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.standing.RevokedAt = testNow.Add(-time.Second)
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
			f.policy.Ceiling["fs.write"] = ScopeCall
			return &f.standing
		},
		DenialByPolicy: func(t *testing.T, f *fixture) *AuthorityGrant {
			f.policy.Denies = []DenyPolicy{{Action: "fs.write", Scope: Scope{Class: ScopeSetting, Value: "workspace"}}}
			return &f.standing
		},
	}

	for reason, arrange := range cases {
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

func TestAuthorizeIgnoresNeverRuleOutsideItsScope(t *testing.T) {
	f := newFixture(t)
	f.policy.Denies = []DenyPolicy{
		{Action: "fs.write", Scope: Scope{Class: ScopeSubject, Value: "repo/other.go"}},
		{Action: "fs.write", Scope: Scope{Class: ScopeSetting, Value: "other-workspace"}},
		{Action: "fs.delete", Scope: Scope{Class: ScopeGlobal}},
	}
	use, _, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != UseAllowed {
		t.Fatalf("Authorize() = %s/%s, want allowed", use.Outcome, use.Reason)
	}
}

func TestAuthorizeTreatsUnlistedActionAsAboveCeiling(t *testing.T) {
	f := newFixture(t)
	f.policy.Ceiling = map[ActionClass]ScopeClass{}
	use, _, err := Authorize(f.request, &f.allowOnce, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Reason != DenialAboveCeiling {
		t.Fatalf("Authorize() reason = %s, want above_ceiling", use.Reason)
	}
}

func TestAuthorizeRejectsMalformedInputs(t *testing.T) {
	f := newFixture(t)
	badGrant := f.standing
	badGrant.Mode = "always"

	for name, call := range map[string]func() error{
		"request": func() error {
			_, _, err := Authorize(ActionRequest{}, &f.standing, f.policy, testNow)
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
		"standing within ceiling": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant { return f.standing },
		},
		"allow once": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant { return f.allowOnce },
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
		"above ceiling": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.standing.Scope = Scope{Class: ScopeGlobal}
				return f.standing
			},
			want: DenialAboveCeiling,
		},
		"never rule on same scope": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Denies = []DenyPolicy{{Action: "fs.write", Scope: f.standing.Scope}}
				return f.standing
			},
			want: DenialByPolicy,
		},
		"global never rule": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Denies = []DenyPolicy{{Action: "fs.write", Scope: Scope{Class: ScopeGlobal}}}
				return f.allowOnce
			},
			want: DenialByPolicy,
		},
		"never rule on other action": {
			arrange: func(t *testing.T, f *fixture) AuthorityGrant {
				f.policy.Denies = []DenyPolicy{{Action: "fs.delete", Scope: Scope{Class: ScopeGlobal}}}
				return f.standing
			},
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
		"allow once without call scope": func() AuthorityGrant {
			g := f.allowOnce
			g.Scope = Scope{Class: ScopeGlobal}
			g.Operation = ids.OperationID{}
			return g
		},
		"standing with call scope": func() AuthorityGrant {
			g := f.standing
			g.Scope = Scope{Class: ScopeCall}
			g.Operation = f.request.Operation
			return g
		},
		"call scope without operation": func() AuthorityGrant {
			g := f.allowOnce
			g.Operation = ids.OperationID{}
			return g
		},
		"operation outside call scope": func() AuthorityGrant {
			g := f.standing
			g.Operation = f.request.Operation
			return g
		},
		"retired standing grant": func() AuthorityGrant {
			g := f.standing
			g.RetiredAt = testNow
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
			g.Action = "FS Write"
			return g
		},
		"subject scope without value": func() AuthorityGrant {
			g := f.standing
			g.Scope = Scope{Class: ScopeSubject}
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

func TestDenyPolicyCannotBeCallScoped(t *testing.T) {
	deny := DenyPolicy{Action: "fs.write", Scope: Scope{Class: ScopeCall}}
	if err := deny.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() error = %v, want ErrInvalid", err)
	}
}

func TestEveryDenialReasonIsExercised(t *testing.T) {
	// TestAuthorizeDenials covers every reason except retired, which
	// TestAuthorizeRetiresAllowOnceGrant covers. This guards the enum: a new
	// reason must get a test before this count changes.
	if got := len(DenialReasons()); got != 13 {
		t.Fatalf("len(DenialReasons()) = %d; add Authorize coverage for the new reason", got)
	}
}
