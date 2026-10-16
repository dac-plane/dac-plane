package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const (
	maxActionClassLength = 128
	maxTargetLength      = 256
)

var (
	actionClassPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ActionClass names a governed action, for example "fs.write". The catalogue
// of classes belongs to provider contracts; this type only fixes the syntax.
type ActionClass string

// Validate reports whether the class is a dotted lowercase name.
func (a ActionClass) Validate() error {
	if len(a) > maxActionClassLength || !actionClassPattern.MatchString(string(a)) {
		return fmt.Errorf("%w: malformed action class", ErrInvalid)
	}
	return nil
}

// Digest is a SHA-256 digest of a canonical action input, in the form
// "sha256:<64 lowercase hex>". It binds approval to exact content without
// storing that content.
type Digest string

// Validate reports whether the digest is well formed.
func (d Digest) Validate() error {
	if !digestPattern.MatchString(string(d)) {
		return fmt.Errorf("%w: malformed digest", ErrInvalid)
	}
	return nil
}

// DigestOf returns the digest of exactly these bytes. No canonicalization is
// applied: two encodings of the same value have different digests.
func DigestOf(data []byte) Digest {
	sum := sha256.Sum256(data)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// ScopeClass is how an action is partitioned for authority. It is a property
// of the action, fixed by the action catalogue, not a breadth a grant chooses:
// global actions are authorized as a whole, setting and subject actions per
// named setting or subject, and call actions only one call at a time.
type ScopeClass string

const (
	ScopeGlobal  ScopeClass = "global"
	ScopeSetting ScopeClass = "setting"
	ScopeSubject ScopeClass = "subject"
	ScopeCall    ScopeClass = "call"
)

// ScopeClasses lists every valid ScopeClass.
func ScopeClasses() []ScopeClass {
	return []ScopeClass{ScopeGlobal, ScopeSetting, ScopeSubject, ScopeCall}
}

// targeted reports whether actions of this class name a setting or subject.
func (c ScopeClass) targeted() bool {
	return c == ScopeSetting || c == ScopeSubject
}

// Scope is where standing authority applies: the action's scope class and, for
// setting and subject classes, the named target.
type Scope struct {
	Class  ScopeClass `json:"class"`
	Target string     `json:"target,omitzero"`
}

// Validate reports whether the class is known and the target matches it.
func (s Scope) Validate() error {
	if !slices.Contains(ScopeClasses(), s.Class) {
		return fmt.Errorf("%w: unknown scope class", ErrInvalid)
	}
	if s.Class.targeted() != (s.Target != "") || len(s.Target) > maxTargetLength {
		return fmt.Errorf("%w: setting and subject scopes, and only those, name a target", ErrInvalid)
	}
	return nil
}

// Permission is a tenant's standing answer for an action class, ordered
// never < ask < always.
type Permission string

const (
	PermissionNever  Permission = "never"
	PermissionAsk    Permission = "ask"
	PermissionAlways Permission = "always"
)

// Permissions lists every Permission from most to least restrictive.
func Permissions() []Permission {
	return []Permission{PermissionNever, PermissionAsk, PermissionAlways}
}

func (p Permission) rank() int {
	return slices.Index(Permissions(), p)
}

// GrantMode is whether authority is used once or stands until it ends.
type GrantMode string

const (
	GrantAllowOnce GrantMode = "allow_once"
	GrantStanding  GrantMode = "standing"
)

// GrantModes lists every valid GrantMode.
func GrantModes() []GrantMode {
	return []GrantMode{GrantAllowOnce, GrantStanding}
}

// AuthoritySource attributes an AuthorityUse to the kind of approval behind it.
type AuthoritySource string

const (
	SourceOneTimeApproval AuthoritySource = "one_time_approval"
	SourceStandingGrant   AuthoritySource = "standing_grant"
)

// AuthoritySources lists every valid AuthoritySource.
func AuthoritySources() []AuthoritySource {
	return []AuthoritySource{SourceOneTimeApproval, SourceStandingGrant}
}

// Source derives the attribution recorded for authority of this mode.
func (m GrantMode) Source() AuthoritySource {
	if m == GrantAllowOnce {
		return SourceOneTimeApproval
	}
	return SourceStandingGrant
}

// ActionRequest is the effective action to authorize, after any hook rewrite.
// Scope is the action's catalogue scope class and target. InputDigest covers
// the effective input, so a rewrite cannot reuse an approval for other content.
type ActionRequest struct {
	Tenant      ids.TenantID    `json:"tenant_id"`
	Attempt     ids.AttemptID   `json:"attempt_id"`
	Provider    ids.ProviderID  `json:"provider_id"`
	Operation   ids.OperationID `json:"operation_id"`
	Action      ActionClass     `json:"action"`
	Scope       Scope           `json:"scope"`
	InputDigest Digest          `json:"input_digest"`
}

// Validate reports whether the request is complete.
func (r ActionRequest) Validate() error {
	if r.Tenant.IsZero() || r.Attempt.IsZero() || r.Provider.IsZero() || r.Operation.IsZero() {
		return fmt.Errorf("%w: action request requires tenant, attempt, provider and operation", ErrInvalid)
	}
	if err := r.Action.Validate(); err != nil {
		return err
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	return r.InputDigest.Validate()
}

// DenyPolicy is a never rule for one named setting or subject of an action.
// A never rule for a whole action class is PermissionNever in the policy.
type DenyPolicy struct {
	Action ActionClass `json:"action"`
	Target string      `json:"target"`
}

// Validate reports whether the rule is well formed.
func (d DenyPolicy) Validate() error {
	if d.Target == "" || len(d.Target) > maxTargetLength {
		return fmt.Errorf("%w: deny policy requires a target", ErrInvalid)
	}
	return d.Action.Validate()
}

func (d DenyPolicy) matches(action ActionClass, scope Scope) bool {
	return d.Action == action && scope.Class.targeted() && d.Target == scope.Target
}

// Policy is a tenant's authority policy at one generation. Settings are the
// tenant's chosen permissions and Ceiling the administrative maximum; the
// effective permission is the lower of the two, and an action class missing
// from either is never permitted. Any policy change advances Generation, which
// invalidates authority issued under earlier generations.
type Policy struct {
	Tenant     ids.TenantID               `json:"tenant_id"`
	Generation uint64                     `json:"generation"`
	Settings   map[ActionClass]Permission `json:"settings,omitempty"`
	Ceiling    map[ActionClass]Permission `json:"ceiling,omitempty"`
	Denies     []DenyPolicy               `json:"denies,omitempty"`
}

// Validate reports whether the policy is well formed. A setting above the
// ceiling is valid; it is clamped, never honoured.
func (p Policy) Validate() error {
	if p.Tenant.IsZero() || p.Generation == 0 {
		return fmt.Errorf("%w: policy requires tenant and a positive generation", ErrInvalid)
	}
	for _, permissions := range []map[ActionClass]Permission{p.Settings, p.Ceiling} {
		for action, permission := range permissions {
			if err := action.Validate(); err != nil {
				return err
			}
			if permission.rank() < 0 {
				return fmt.Errorf("%w: unknown permission", ErrInvalid)
			}
		}
	}
	for _, deny := range p.Denies {
		if err := deny.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Effective returns the tenant's permission for an action class, clamped by
// the ceiling.
func (p Policy) Effective(action ActionClass) Permission {
	setting, hasSetting := p.Settings[action]
	ceiling, hasCeiling := p.Ceiling[action]
	if !hasSetting || !hasCeiling {
		return PermissionNever
	}
	return Permissions()[min(setting.rank(), ceiling.rank())]
}

func (p Policy) denies(action ActionClass, scope Scope) bool {
	return slices.ContainsFunc(p.Denies, func(d DenyPolicy) bool { return d.matches(action, scope) })
}

// AuthorityGrant is authority for one Attempt to have a provider perform an
// action.
//
// An allow-once grant is a one-time approval of one operation with one exact
// input digest, for an action of any scope class; its first use retires it.
// A standing grant covers the action within one scope that must equal the
// action's catalogue scope class and target. Call-scoped actions cannot have
// standing grants. A standing grant lasts until expiry, revocation or a policy
// generation change, and requires the effective permission to be always.
type AuthorityGrant struct {
	ID               ids.GrantID     `json:"grant_id"`
	Tenant           ids.TenantID    `json:"tenant_id"`
	Attempt          ids.AttemptID   `json:"attempt_id"`
	Provider         ids.ProviderID  `json:"provider_id"`
	Action           ActionClass     `json:"action"`
	Mode             GrantMode       `json:"mode"`
	Operation        ids.OperationID `json:"operation_id,omitzero"`
	InputDigest      Digest          `json:"input_digest,omitzero"`
	Scope            Scope           `json:"scope,omitzero"`
	GrantedBy        ids.PrincipalID `json:"granted_by"`
	PolicyGeneration uint64          `json:"policy_generation"`
	IssuedAt         time.Time       `json:"issued_at"`
	ExpiresAt        time.Time       `json:"expires_at"`
	RevokedAt        time.Time       `json:"revoked_at,omitzero"`
	RetiredAt        time.Time       `json:"retired_at,omitzero"`
}

// Validate reports whether the grant is structurally consistent. It does not
// check expiry, revocation or policy; Authorize and IssueGrant do.
func (g AuthorityGrant) Validate() error {
	if g.ID.IsZero() || g.Tenant.IsZero() || g.Attempt.IsZero() || g.Provider.IsZero() || g.GrantedBy.IsZero() {
		return fmt.Errorf("%w: grant requires grant, tenant, attempt, provider and granting principal", ErrInvalid)
	}
	if err := g.Action.Validate(); err != nil {
		return err
	}
	switch g.Mode {
	case GrantAllowOnce:
		if g.Operation.IsZero() || g.Scope != (Scope{}) {
			return fmt.Errorf("%w: allow-once grant binds an operation, not a scope", ErrInvalid)
		}
		if err := g.InputDigest.Validate(); err != nil {
			return err
		}
	case GrantStanding:
		if !g.Operation.IsZero() || g.InputDigest != "" || !g.RetiredAt.IsZero() {
			return fmt.Errorf("%w: standing grant binds a scope, not an operation, and is never retired", ErrInvalid)
		}
		if err := g.Scope.Validate(); err != nil {
			return err
		}
		if g.Scope.Class == ScopeCall {
			return fmt.Errorf("%w: call-scoped actions cannot have standing grants", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown grant mode", ErrInvalid)
	}
	if g.PolicyGeneration == 0 || g.IssuedAt.IsZero() || !g.ExpiresAt.After(g.IssuedAt) {
		return fmt.Errorf("%w: grant requires a policy generation and expiry after issuance", ErrInvalid)
	}
	return nil
}

// IssueGrant checks that a new grant may be issued under the tenant's current
// policy. A refusal is returned as the DenialReason that use would produce.
func IssueGrant(g AuthorityGrant, policy Policy) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if !g.RevokedAt.IsZero() || !g.RetiredAt.IsZero() {
		return fmt.Errorf("%w: a new grant cannot be revoked or retired", ErrInvalid)
	}
	switch {
	case g.Tenant != policy.Tenant:
		return DenialCrossTenant
	case g.PolicyGeneration != policy.Generation:
		return DenialStaleGeneration
	case policy.Effective(g.Action) == PermissionNever:
		return DenialByPolicy
	case g.Mode == GrantStanding && policy.denies(g.Action, g.Scope):
		return DenialByPolicy
	case g.Mode == GrantStanding && policy.Effective(g.Action) != PermissionAlways:
		return DenialAboveCeiling
	}
	return nil
}

// DenialReason is why authority was refused. It is also an error so issuance
// refusals can be matched with errors.Is.
type DenialReason string

const (
	DenialNoGrant         DenialReason = "no_grant"
	DenialCrossTenant     DenialReason = "cross_tenant"
	DenialWrongAttempt    DenialReason = "wrong_attempt"
	DenialWrongAudience   DenialReason = "wrong_audience"
	DenialWrongAction     DenialReason = "wrong_action"
	DenialWrongOperation  DenialReason = "wrong_operation"
	DenialInputMismatch   DenialReason = "input_mismatch"
	DenialScopeMismatch   DenialReason = "scope_mismatch"
	DenialRevoked         DenialReason = "revoked"
	DenialRetired         DenialReason = "retired"
	DenialNotYetValid     DenialReason = "not_yet_valid"
	DenialExpired         DenialReason = "expired"
	DenialStaleGeneration DenialReason = "stale_generation"
	DenialAboveCeiling    DenialReason = "above_ceiling"
	DenialByPolicy        DenialReason = "denied_by_policy"
)

// DenialReasons lists every valid DenialReason.
func DenialReasons() []DenialReason {
	return []DenialReason{
		DenialNoGrant, DenialCrossTenant, DenialWrongAttempt, DenialWrongAudience,
		DenialWrongAction, DenialWrongOperation, DenialInputMismatch, DenialScopeMismatch,
		DenialRevoked, DenialRetired, DenialNotYetValid, DenialExpired,
		DenialStaleGeneration, DenialAboveCeiling, DenialByPolicy,
	}
}

func (r DenialReason) Error() string {
	return "authority denied: " + string(r)
}

// UseOutcome is whether authority was used or refused.
type UseOutcome string

const (
	UseAllowed UseOutcome = "allowed"
	UseDenied  UseOutcome = "denied"
)

// UseOutcomes lists every valid UseOutcome.
func UseOutcomes() []UseOutcome {
	return []UseOutcome{UseAllowed, UseDenied}
}

// AuthorityUse records one authorization decision for a requested operation,
// which is its stable identity. Denials are recorded too, with the grant that
// was presented when there was one.
type AuthorityUse struct {
	Tenant           ids.TenantID    `json:"tenant_id"`
	Attempt          ids.AttemptID   `json:"attempt_id"`
	Provider         ids.ProviderID  `json:"provider_id"`
	Operation        ids.OperationID `json:"operation_id"`
	Action           ActionClass     `json:"action"`
	InputDigest      Digest          `json:"input_digest"`
	Grant            ids.GrantID     `json:"grant_id,omitzero"`
	Source           AuthoritySource `json:"source,omitzero"`
	GrantedBy        ids.PrincipalID `json:"granted_by,omitzero"`
	Outcome          UseOutcome      `json:"outcome"`
	Reason           DenialReason    `json:"reason,omitzero"`
	PolicyGeneration uint64          `json:"policy_generation"`
	DecidedAt        time.Time       `json:"decided_at"`
}

// Validate reports whether the record is consistent with its outcome.
func (u AuthorityUse) Validate() error {
	if u.Tenant.IsZero() || u.Attempt.IsZero() || u.Provider.IsZero() || u.Operation.IsZero() {
		return fmt.Errorf("%w: authority use requires tenant, attempt, provider and operation", ErrInvalid)
	}
	if err := u.Action.Validate(); err != nil {
		return err
	}
	if err := u.InputDigest.Validate(); err != nil {
		return err
	}
	if u.PolicyGeneration == 0 || u.DecidedAt.IsZero() {
		return fmt.Errorf("%w: authority use requires policy generation and decision time", ErrInvalid)
	}
	if u.Grant.IsZero() != (u.Source == "") || u.Grant.IsZero() != u.GrantedBy.IsZero() {
		return fmt.Errorf("%w: grant, source and granting principal are set together", ErrInvalid)
	}
	if u.Source != "" && !slices.Contains(AuthoritySources(), u.Source) {
		return fmt.Errorf("%w: unknown authority source", ErrInvalid)
	}
	switch u.Outcome {
	case UseAllowed:
		if u.Grant.IsZero() || u.Reason != "" {
			return fmt.Errorf("%w: allowed use requires a grant and no reason", ErrInvalid)
		}
	case UseDenied:
		if !slices.Contains(DenialReasons(), u.Reason) {
			return fmt.Errorf("%w: denied use requires a known reason", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown use outcome", ErrInvalid)
	}
	return nil
}

// Authorize decides whether grant authorizes the effective request under the
// tenant's current policy at now. grant may be nil when none was presented.
//
// A refusal is an AuthorityUse with outcome denied, not an error; errors are
// reserved for malformed inputs. When an allow-once grant is used, the
// retired grant is returned and must be persisted in the same transaction as
// the AuthorityUse so it cannot authorize a second action.
func Authorize(req ActionRequest, grant *AuthorityGrant, policy Policy, now time.Time) (AuthorityUse, *AuthorityGrant, error) {
	if err := req.Validate(); err != nil {
		return AuthorityUse{}, nil, err
	}
	if err := policy.Validate(); err != nil {
		return AuthorityUse{}, nil, err
	}
	if grant != nil {
		if err := grant.Validate(); err != nil {
			return AuthorityUse{}, nil, err
		}
	}
	if now.IsZero() {
		return AuthorityUse{}, nil, fmt.Errorf("%w: decision time is required", ErrInvalid)
	}

	use := AuthorityUse{
		Tenant:           req.Tenant,
		Attempt:          req.Attempt,
		Provider:         req.Provider,
		Operation:        req.Operation,
		Action:           req.Action,
		InputDigest:      req.InputDigest,
		PolicyGeneration: policy.Generation,
		DecidedAt:        now,
	}
	if grant != nil {
		use.Grant = grant.ID
		use.Source = grant.Mode.Source()
		use.GrantedBy = grant.GrantedBy
	}

	if reason := denialReason(req, grant, policy, now); reason != "" {
		use.Outcome = UseDenied
		use.Reason = reason
		return use, nil, nil
	}

	use.Outcome = UseAllowed
	if grant.Mode != GrantAllowOnce {
		return use, nil, nil
	}
	retired := *grant
	retired.RetiredAt = now
	return use, &retired, nil
}

// denialReason returns the first failed predicate, or "" when authority holds.
// Tenant and never rules are checked before the grant, so a never rule wins
// over any grant and is reported even when no grant was presented.
func denialReason(req ActionRequest, grant *AuthorityGrant, policy Policy, now time.Time) DenialReason {
	effective := policy.Effective(req.Action)
	switch {
	case policy.Tenant != req.Tenant:
		return DenialCrossTenant
	case effective == PermissionNever || policy.denies(req.Action, req.Scope):
		return DenialByPolicy
	case grant == nil:
		return DenialNoGrant
	case grant.Tenant != req.Tenant:
		return DenialCrossTenant
	case grant.Attempt != req.Attempt:
		return DenialWrongAttempt
	case grant.Provider != req.Provider:
		return DenialWrongAudience
	case grant.Action != req.Action:
		return DenialWrongAction
	case grant.Mode == GrantAllowOnce && grant.Operation != req.Operation:
		return DenialWrongOperation
	case grant.Mode == GrantAllowOnce && grant.InputDigest != req.InputDigest:
		return DenialInputMismatch
	case grant.Mode == GrantStanding && grant.Scope != req.Scope:
		return DenialScopeMismatch
	case !grant.RevokedAt.IsZero():
		return DenialRevoked
	case !grant.RetiredAt.IsZero():
		return DenialRetired
	case now.Before(grant.IssuedAt):
		return DenialNotYetValid
	case !now.Before(grant.ExpiresAt):
		return DenialExpired
	case grant.PolicyGeneration != policy.Generation:
		return DenialStaleGeneration
	case grant.Mode == GrantStanding && effective != PermissionAlways:
		return DenialAboveCeiling
	}
	return ""
}
