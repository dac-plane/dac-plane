package identity

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const (
	maxActionClassLength = 128
	maxScopeValueLength  = 256
)

var actionClassPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

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

// ScopeClass is how broadly authority applies.
type ScopeClass string

const (
	ScopeCall    ScopeClass = "call"
	ScopeSubject ScopeClass = "subject"
	ScopeSetting ScopeClass = "setting"
	ScopeGlobal  ScopeClass = "global"
)

// ScopeClasses lists every scope class from narrowest to broadest.
func ScopeClasses() []ScopeClass {
	return []ScopeClass{ScopeCall, ScopeSubject, ScopeSetting, ScopeGlobal}
}

// breadth orders scope classes; it is -1 for an unknown class.
func (c ScopeClass) breadth() int {
	return slices.Index(ScopeClasses(), c)
}

// Scope is where authority applies. Setting and subject scopes name their
// setting or subject in Value. Global and call scopes have no value; a call
// scope binds through the grant's operation.
type Scope struct {
	Class ScopeClass `json:"class"`
	Value string     `json:"value,omitzero"`
}

// Validate reports whether the class is known and the value matches it.
func (s Scope) Validate() error {
	switch s.Class {
	case ScopeGlobal, ScopeCall:
		if s.Value != "" {
			return fmt.Errorf("%w: %s scope takes no value", ErrInvalid, s.Class)
		}
	case ScopeSetting, ScopeSubject:
		if s.Value == "" || len(s.Value) > maxScopeValueLength {
			return fmt.Errorf("%w: %s scope requires a value", ErrInvalid, s.Class)
		}
	default:
		return fmt.Errorf("%w: unknown scope class", ErrInvalid)
	}
	return nil
}

// covers reports whether a non-call scope applies to the request.
func (s Scope) covers(req ActionRequest) bool {
	switch s.Class {
	case ScopeGlobal:
		return true
	case ScopeSetting:
		return req.Setting == s.Value
	case ScopeSubject:
		return req.Subject == s.Value
	default:
		return false
	}
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
// Setting and Subject identify what the action touches for scope matching.
type ActionRequest struct {
	Tenant    ids.TenantID    `json:"tenant_id"`
	Attempt   ids.AttemptID   `json:"attempt_id"`
	Provider  ids.ProviderID  `json:"provider_id"`
	Operation ids.OperationID `json:"operation_id"`
	Action    ActionClass     `json:"action"`
	Setting   string          `json:"setting,omitzero"`
	Subject   string          `json:"subject,omitzero"`
}

// Validate reports whether the request is complete.
func (r ActionRequest) Validate() error {
	if r.Tenant.IsZero() || r.Attempt.IsZero() || r.Provider.IsZero() || r.Operation.IsZero() {
		return fmt.Errorf("%w: action request requires tenant, attempt, provider and operation", ErrInvalid)
	}
	if len(r.Setting) > maxScopeValueLength || len(r.Subject) > maxScopeValueLength {
		return fmt.Errorf("%w: action request setting or subject too long", ErrInvalid)
	}
	return r.Action.Validate()
}

// DenyPolicy is a never rule: it blocks issuance and use of authority for its
// action within its scope.
type DenyPolicy struct {
	Action ActionClass `json:"action"`
	Scope  Scope       `json:"scope"`
}

// Validate reports whether the rule is well formed. A never rule cannot be
// call scoped because it outlives any single call.
func (d DenyPolicy) Validate() error {
	if err := d.Action.Validate(); err != nil {
		return err
	}
	if d.Scope.Class == ScopeCall {
		return fmt.Errorf("%w: deny policy cannot be call scoped", ErrInvalid)
	}
	return d.Scope.Validate()
}

// blocksGrant reports whether the rule overlaps the grant's action and scope.
func (d DenyPolicy) blocksGrant(g AuthorityGrant) bool {
	if d.Action != g.Action {
		return false
	}
	return d.Scope.Class == ScopeGlobal || g.Scope.Class == ScopeGlobal || d.Scope == g.Scope
}

// Policy is a tenant's authority policy at one generation. Ceiling holds the
// broadest scope class allowed per action class; an action class missing from
// it is not permitted at all. Any policy change advances Generation, which
// invalidates authority issued under earlier generations.
type Policy struct {
	Tenant     ids.TenantID               `json:"tenant_id"`
	Generation uint64                     `json:"generation"`
	Ceiling    map[ActionClass]ScopeClass `json:"ceiling,omitempty"`
	Denies     []DenyPolicy               `json:"denies,omitempty"`
}

// Validate reports whether the policy is well formed.
func (p Policy) Validate() error {
	if p.Tenant.IsZero() || p.Generation == 0 {
		return fmt.Errorf("%w: policy requires tenant and a positive generation", ErrInvalid)
	}
	for action, class := range p.Ceiling {
		if err := action.Validate(); err != nil {
			return err
		}
		if class.breadth() < 0 {
			return fmt.Errorf("%w: unknown ceiling scope class", ErrInvalid)
		}
	}
	for _, deny := range p.Denies {
		if err := deny.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (p Policy) withinCeiling(action ActionClass, class ScopeClass) bool {
	ceiling, ok := p.Ceiling[action]
	return ok && class.breadth() <= ceiling.breadth()
}

// AuthorityGrant is authority for one Attempt to have a provider perform an
// action within a scope. An allow-once grant is call scoped to one operation
// and retired by its first use; a standing grant is never call scoped and lasts
// until expiry, revocation or a policy generation change.
type AuthorityGrant struct {
	ID               ids.GrantID     `json:"grant_id"`
	Tenant           ids.TenantID    `json:"tenant_id"`
	Attempt          ids.AttemptID   `json:"attempt_id"`
	Provider         ids.ProviderID  `json:"provider_id"`
	Operation        ids.OperationID `json:"operation_id,omitzero"`
	Action           ActionClass     `json:"action"`
	Scope            Scope           `json:"scope"`
	Mode             GrantMode       `json:"mode"`
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
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	switch g.Mode {
	case GrantAllowOnce:
		if g.Scope.Class != ScopeCall {
			return fmt.Errorf("%w: allow-once grant must be call scoped", ErrInvalid)
		}
	case GrantStanding:
		if g.Scope.Class == ScopeCall {
			return fmt.Errorf("%w: standing grant cannot be call scoped", ErrInvalid)
		}
		if !g.RetiredAt.IsZero() {
			return fmt.Errorf("%w: standing grant cannot be retired", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown grant mode", ErrInvalid)
	}
	if (g.Scope.Class == ScopeCall) == g.Operation.IsZero() {
		return fmt.Errorf("%w: grant operation is required for and only for call scope", ErrInvalid)
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
	if g.Tenant != policy.Tenant {
		return DenialCrossTenant
	}
	if g.PolicyGeneration != policy.Generation {
		return DenialStaleGeneration
	}
	if !policy.withinCeiling(g.Action, g.Scope.Class) {
		return DenialAboveCeiling
	}
	for _, deny := range policy.Denies {
		if deny.blocksGrant(g) {
			return DenialByPolicy
		}
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
	DenialWrongOperation  DenialReason = "wrong_operation"
	DenialWrongAction     DenialReason = "wrong_action"
	DenialScopeMismatch   DenialReason = "scope_mismatch"
	DenialExpired         DenialReason = "expired"
	DenialRevoked         DenialReason = "revoked"
	DenialRetired         DenialReason = "retired"
	DenialStaleGeneration DenialReason = "stale_generation"
	DenialAboveCeiling    DenialReason = "above_ceiling"
	DenialByPolicy        DenialReason = "denied_by_policy"
)

// DenialReasons lists every valid DenialReason.
func DenialReasons() []DenialReason {
	return []DenialReason{
		DenialNoGrant, DenialCrossTenant, DenialWrongAttempt, DenialWrongAudience,
		DenialWrongOperation, DenialWrongAction, DenialScopeMismatch, DenialExpired,
		DenialRevoked, DenialRetired, DenialStaleGeneration, DenialAboveCeiling, DenialByPolicy,
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
// Tenant and never rules are checked before the grant so they are reported
// even when no grant was presented.
func denialReason(req ActionRequest, grant *AuthorityGrant, policy Policy, now time.Time) DenialReason {
	if policy.Tenant != req.Tenant {
		return DenialCrossTenant
	}
	for _, deny := range policy.Denies {
		if deny.Action == req.Action && deny.Scope.covers(req) {
			return DenialByPolicy
		}
	}
	switch {
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
	case grant.Scope.Class == ScopeCall && grant.Operation != req.Operation:
		return DenialWrongOperation
	case grant.Scope.Class != ScopeCall && !grant.Scope.covers(req):
		return DenialScopeMismatch
	case !grant.RevokedAt.IsZero():
		return DenialRevoked
	case !grant.RetiredAt.IsZero():
		return DenialRetired
	case !now.Before(grant.ExpiresAt):
		return DenialExpired
	case grant.PolicyGeneration != policy.Generation:
		return DenialStaleGeneration
	case !policy.withinCeiling(req.Action, grant.Scope.Class):
		return DenialAboveCeiling
	}
	return ""
}
