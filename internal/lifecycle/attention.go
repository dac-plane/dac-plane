package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// AttentionKind is what an attention request asks of a person.
type AttentionKind string

const (
	AttentionQuestion       AttentionKind = "question"
	AttentionActionApproval AttentionKind = "action_approval"
	AttentionAuthentication AttentionKind = "authentication_handoff"
	AttentionBlockedWork    AttentionKind = "blocked_work"
)

// AttentionKinds lists every AttentionKind.
func AttentionKinds() []AttentionKind {
	return []AttentionKind{AttentionQuestion, AttentionActionApproval, AttentionAuthentication, AttentionBlockedWork}
}

// AttentionState is the lifecycle state of an attention request.
type AttentionState string

const (
	AttentionPending     AttentionState = "pending"
	AttentionDecided     AttentionState = "decided"
	AttentionDispatching AttentionState = "dispatching"
	AttentionDelivered   AttentionState = "delivered"
	AttentionConsumed    AttentionState = "consumed"
	AttentionExpired     AttentionState = "expired"
	AttentionCancelled   AttentionState = "cancelled"
	AttentionSuperseded  AttentionState = "superseded"
)

// AttentionStates lists every AttentionState.
func AttentionStates() []AttentionState {
	return []AttentionState{
		AttentionPending, AttentionDecided, AttentionDispatching, AttentionDelivered,
		AttentionConsumed, AttentionExpired, AttentionCancelled, AttentionSuperseded,
	}
}

// AttentionEvent is a command or observation that may change an attention request.
type AttentionEvent string

const (
	AttentionDecide    AttentionEvent = "decide"
	AttentionDispatch  AttentionEvent = "dispatch"
	AttentionRedeliver AttentionEvent = "redeliver"
	AttentionDeliver   AttentionEvent = "deliver"
	AttentionConsume   AttentionEvent = "consume"
	AttentionExpire    AttentionEvent = "expire"
	AttentionCancel    AttentionEvent = "cancel"
	AttentionSupersede AttentionEvent = "supersede"
)

// AttentionEvents lists every AttentionEvent.
func AttentionEvents() []AttentionEvent {
	return []AttentionEvent{
		AttentionDecide, AttentionDispatch, AttentionRedeliver, AttentionDeliver,
		AttentionConsume, AttentionExpire, AttentionCancel, AttentionSupersede,
	}
}

var decidedStates = []AttentionState{AttentionDecided, AttentionDispatching, AttentionDelivered, AttentionConsumed}

// unconsumedAttentionStates are the states in which a request still holds its
// Attempt's wait: answered or not, the native wait is real until its product
// effect is observed or the request closes otherwise.
var unconsumedAttentionStates = []AttentionState{
	AttentionPending, AttentionDecided, AttentionDispatching, AttentionDelivered,
}

// UnconsumedAttentionStates lists every state in which a request is still an
// open wait.
func UnconsumedAttentionStates() []AttentionState {
	return slices.Clone(unconsumedAttentionStates)
}

// unconsumed adds the transitions shared by every undelivered-or-unconsumed state:
// the task can be cancelled or the native request superseded.
func unconsumed(transitions map[AttentionEvent][]AttentionState) map[AttentionEvent][]AttentionState {
	transitions[AttentionCancel] = []AttentionState{AttentionCancelled}
	transitions[AttentionSupersede] = []AttentionState{AttentionSuperseded}
	return transitions
}

// attentionMachine follows the historical specification §13. Every decision,
// including a denial, is delivered to the runtime and its consumption
// observed: the runtime must learn that an action was denied.
var attentionMachine = newMachine(AttentionStates(), AttentionEvents(),
	[]AttentionState{AttentionConsumed, AttentionExpired, AttentionCancelled, AttentionSuperseded},
	map[AttentionState]map[AttentionEvent][]AttentionState{
		AttentionPending: unconsumed(map[AttentionEvent][]AttentionState{
			AttentionDecide: {AttentionDecided},
			AttentionExpire: {AttentionExpired},
		}),
		AttentionDecided: unconsumed(map[AttentionEvent][]AttentionState{
			AttentionDispatch: {AttentionDispatching},
		}),
		// Redelivery reuses the same decision identity.
		AttentionDispatching: unconsumed(map[AttentionEvent][]AttentionState{
			AttentionRedeliver: {AttentionDispatching},
			AttentionDeliver:   {AttentionDelivered},
			// An authenticated consumption observation may race ahead of the
			// send reply; consuming the exact native request proves delivery.
			AttentionConsume: {AttentionConsumed},
		}),
		AttentionDelivered: unconsumed(map[AttentionEvent][]AttentionState{
			AttentionConsume: {AttentionConsumed},
		}),
	},
)

// AttentionRequest asks authorized responders to unblock an Attempt. An action
// approval names the provider, operation and digest of the exact action it
// would approve, so a changed action needs a new request. PolicyGeneration is
// the tenant policy the request was raised under: a policy change invalidates
// any decision on it, so the person re-evaluates under the new policy.
type AttentionRequest struct {
	Scope            identity.Scope    `json:"scope,omitzero"`
	ID               ids.AttentionID   `json:"attention_id"`
	Tenant           ids.TenantID      `json:"tenant_id"`
	Task             ids.TaskID        `json:"task_id"`
	Attempt          ids.AttemptID     `json:"attempt_id"`
	Kind             AttentionKind     `json:"kind"`
	Provider         ids.ProviderID    `json:"provider_id,omitzero"`
	Operation        ids.OperationID   `json:"operation_id,omitzero"`
	ActionDigest     identity.Digest   `json:"action_digest,omitzero"`
	PolicyGeneration uint64            `json:"policy_generation"`
	Responders       []ids.PrincipalID `json:"responders"`
	State            AttentionState    `json:"state"`
	Version          uint64            `json:"version"`
	Decision         ids.DecisionID    `json:"decision_id,omitzero"`
	ExpiresAt        time.Time         `json:"expires_at"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// NewAttentionRequest returns a pending request at version 1.
func NewAttentionRequest(request AttentionRequest, at time.Time) (AttentionRequest, error) {
	request.State = AttentionPending
	request.Version = 1
	request.Decision = ids.DecisionID{}
	request.CreatedAt, request.UpdatedAt = at, at
	return request, request.Validate()
}

// Validate reports whether the record is complete and consistent.
func (r AttentionRequest) Validate() error {
	if r.Scope != (identity.Scope{}) {
		if r.Kind != AttentionActionApproval {
			return fmt.Errorf("%w: only action approvals have scope", ErrInvalid)
		}
		if err := r.Scope.Validate(); err != nil {
			return err
		}
	}
	if r.ID.IsZero() || r.Tenant.IsZero() || r.Task.IsZero() || r.Attempt.IsZero() || r.Version == 0 || r.PolicyGeneration == 0 {
		return fmt.Errorf("%w: attention request requires identifiers, a positive version and a policy generation", ErrInvalid)
	}
	if !slices.Contains(AttentionKinds(), r.Kind) || !slices.Contains(AttentionStates(), r.State) {
		return fmt.Errorf("%w: unknown attention kind or state", ErrInvalid)
	}
	if r.Kind == AttentionActionApproval {
		if r.Provider.IsZero() || r.Operation.IsZero() {
			return fmt.Errorf("%w: action approval requires a provider and an operation", ErrInvalid)
		}
		if err := validDigest(r.ActionDigest); err != nil {
			return err
		}
	} else if !r.Provider.IsZero() || !r.Operation.IsZero() || r.ActionDigest != "" {
		return fmt.Errorf("%w: only action approvals name a provider, operation and action digest", ErrInvalid)
	}
	if len(r.Responders) == 0 || slices.ContainsFunc(r.Responders, ids.PrincipalID.IsZero) {
		return fmt.Errorf("%w: attention request requires authorized responders", ErrInvalid)
	}
	// A request that ended without delivery (expired, cancelled, superseded)
	// may or may not have been decided first.
	decided := slices.Contains(decidedStates, r.State)
	if (decided && r.Decision.IsZero()) || (r.State == AttentionPending && !r.Decision.IsZero()) {
		return fmt.Errorf("%w: a decision exists exactly once the request is decided", ErrInvalid)
	}
	if r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("%w: attention request timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Apply performs a non-decision event if the request is still at
// expectedVersion. Decisions go through Decide.
func (r AttentionRequest) Apply(event AttentionEvent, expectedVersion uint64, at time.Time) (AttentionRequest, Change, error) {
	change := r.change(event)
	if event == AttentionDecide {
		return r, change, reject(change, RejectInvalidTransition)
	}
	return r.apply(change, event, expectedVersion, at)
}

func (r AttentionRequest) change(event AttentionEvent) Change {
	return Change{
		Tenant: r.Tenant, Record: RecordAttention, RecordID: r.ID.Untyped(),
		Event: string(event), From: string(r.State), Version: r.Version,
	}
}

func (r AttentionRequest) apply(change Change, event AttentionEvent, expectedVersion uint64, at time.Time) (AttentionRequest, Change, error) {
	next, code := attentionMachine.next(r.State, r.Version, expectedVersion, event, "")
	if code != "" {
		return r, change, reject(change, code)
	}
	r.State = next
	r.Version++
	r.UpdatedAt = at
	change.To = string(next)
	change.Version = r.Version
	return r, change, nil
}

// DecisionKind is a responder's answer to an attention request.
type DecisionKind string

const (
	DecisionApproveOnce     DecisionKind = "approve_once"
	DecisionApproveStanding DecisionKind = "approve_standing"
	DecisionDeny            DecisionKind = "deny"
	DecisionAnswer          DecisionKind = "answer"
)

// DecisionKinds lists every DecisionKind.
func DecisionKinds() []DecisionKind {
	return []DecisionKind{DecisionApproveOnce, DecisionApproveStanding, DecisionDeny, DecisionAnswer}
}

// allowedFor reports whether a decision kind answers a request kind. Only an
// action approval can be approved; any request can be denied.
func (k DecisionKind) allowedFor(kind AttentionKind) bool {
	switch k {
	case DecisionApproveOnce, DecisionApproveStanding:
		return kind == AttentionActionApproval
	case DecisionAnswer:
		return kind != AttentionActionApproval
	default:
		return k == DecisionDeny
	}
}

// Decision is an immutable answer to one version of one attention request. An
// approval decision does not itself authorize the action: the application
// issues an allow-once AuthorityGrant for the request's operation and action
// digest (identity.AuthorityGrant).
type Decision struct {
	ID             ids.DecisionID  `json:"decision_id"`
	Tenant         ids.TenantID    `json:"tenant_id"`
	Request        ids.AttentionID `json:"attention_id"`
	RequestVersion uint64          `json:"request_version"`
	Principal      ids.PrincipalID `json:"principal_id"`
	Kind           DecisionKind    `json:"kind"`
	AnswerDigest   identity.Digest `json:"answer_digest,omitzero"`
	DecidedAt      time.Time       `json:"decided_at"`
}

// Validate reports whether the record is complete. An answer is referenced by
// the digest of its content, which is stored outside the decision record.
func (d Decision) Validate() error {
	if d.ID.IsZero() || d.Tenant.IsZero() || d.Request.IsZero() || d.Principal.IsZero() ||
		d.RequestVersion == 0 || d.DecidedAt.IsZero() {
		return fmt.Errorf("%w: decision requires identifiers, request version and time", ErrInvalid)
	}
	if !slices.Contains(DecisionKinds(), d.Kind) {
		return fmt.Errorf("%w: unknown decision kind", ErrInvalid)
	}
	if d.Kind == DecisionAnswer {
		return validDigest(d.AnswerDigest)
	}
	if d.AnswerDigest != "" {
		return fmt.Errorf("%w: only an answer carries an answer digest", ErrInvalid)
	}
	return nil
}

// Decide records decision against the request version it was made on, under
// the tenant's current policy generation. The first valid decision wins: any
// later decision, or one made on a stale view, is a version conflict. The
// decider must be an authorized responder, the request must not have expired,
// and the policy must not have changed since the request was raised.
func (r AttentionRequest) Decide(decision Decision, currentPolicyGeneration uint64) (AttentionRequest, Change, error) {
	change := r.change(AttentionDecide)
	if err := decision.Validate(); err != nil {
		return r, change, err
	}
	switch {
	case decision.Tenant != r.Tenant:
		return r, change, reject(change, RejectCrossTenant)
	case decision.Request != r.ID:
		return r, change, reject(change, RejectWrongRecord)
	case !slices.Contains(r.Responders, decision.Principal):
		return r, change, reject(change, RejectUnauthorizedActor)
	case !decision.Kind.allowedFor(r.Kind):
		return r, change, reject(change, RejectDecisionNotAllowed)
	case r.PolicyGeneration != currentPolicyGeneration:
		return r, change, reject(change, RejectStalePolicy)
	case r.State == AttentionPending && r.Version == decision.RequestVersion && !decision.DecidedAt.Before(r.ExpiresAt):
		return r, change, reject(change, RejectRequestExpired)
	}
	next, change, err := r.apply(change, AttentionDecide, decision.RequestVersion, decision.DecidedAt)
	if err != nil {
		return r, change, err
	}
	next.Decision = decision.ID
	return next, change, nil
}

// AllowOnceGrant derives the one-time authority an approval confers: bound to
// the request's attempt, provider, operation, exact action digest and policy
// generation, and attributed to the approving principal. identity.IssueGrant
// then refuses it if the policy has changed since the request was raised.
func (r AttentionRequest) AllowOnceGrant(decision Decision, id ids.GrantID, action identity.ActionClass, expiresAt time.Time) (identity.AuthorityGrant, error) {
	if r.Kind != AttentionActionApproval || decision.Kind != DecisionApproveOnce ||
		decision.ID != r.Decision || !slices.Contains(decidedStates, r.State) {
		return identity.AuthorityGrant{}, fmt.Errorf("%w: only a recorded approval of an action approval confers authority", ErrInvalid)
	}
	grant := identity.AuthorityGrant{
		Decision: decision.ID,
		ID:       id, Tenant: r.Tenant, Attempt: r.Attempt, Provider: r.Provider, Action: action,
		Mode: identity.GrantAllowOnce, Operation: r.Operation, InputDigest: r.ActionDigest,
		GrantedBy: decision.Principal, PolicyGeneration: r.PolicyGeneration,
		IssuedAt: decision.DecidedAt, ExpiresAt: expiresAt,
	}
	return grant, grant.Validate()
}

// StandingGrant derives renewable-use authority for the approval's exact scope.
func (r AttentionRequest) StandingGrant(decision Decision, id ids.GrantID, action identity.ActionClass, expiresAt time.Time) (identity.AuthorityGrant, error) {
	if r.Kind != AttentionActionApproval || decision.Kind != DecisionApproveStanding || decision.ID != r.Decision || !slices.Contains(decidedStates, r.State) {
		return identity.AuthorityGrant{}, fmt.Errorf("%w: standing authority requires a recorded standing approval", ErrInvalid)
	}
	grant := identity.AuthorityGrant{ID: id, Tenant: r.Tenant, Attempt: r.Attempt, Provider: r.Provider,
		Action: action, Mode: identity.GrantStanding, Scope: r.Scope, Decision: decision.ID,
		GrantedBy: decision.Principal, PolicyGeneration: r.PolicyGeneration,
		IssuedAt: decision.DecidedAt, ExpiresAt: expiresAt}
	return grant, grant.Validate()
}

// validDigest reports a malformed digest as a lifecycle error that also matches
// the identity error it wraps.
func validDigest(digest identity.Digest) error {
	if err := digest.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}
