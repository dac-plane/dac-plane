package contracts

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const maxContextItems = 16

// HookKind is the point at which a governed action is intercepted.
type HookKind string

const (
	HookPreAction     HookKind = "pre_action"
	HookPostAction    HookKind = "post_action"
	HookActionFailure HookKind = "action_failure"
)

// HookKinds lists every HookKind.
func HookKinds() []HookKind {
	return []HookKind{HookPreAction, HookPostAction, HookActionFailure}
}

// HookRequest asks the enforcement path about one governed action. A
// pre-action request carries the proposed input and a deadline; post-action
// and failure requests carry digests of input and output, the duration and,
// for a failure, its typed failure. Inputs travel only to the enforcement
// path; records keep their digests.
type HookRequest struct {
	Kind         HookKind             `json:"kind"`
	Operation    ids.OperationID      `json:"operation_id"`
	Tenant       ids.TenantID         `json:"tenant_id"`
	Attempt      ids.AttemptID        `json:"attempt_id"`
	Provider     ids.ProviderID       `json:"provider_id"`
	Action       identity.ActionClass `json:"action"`
	Scope        identity.Scope       `json:"scope"`
	Input        json.RawMessage      `json:"input,omitempty"`
	InputDigest  identity.Digest      `json:"input_digest"`
	OutputDigest identity.Digest      `json:"output_digest,omitzero"`
	Failure      *Failure             `json:"failure,omitempty"`
	Duration     time.Duration        `json:"duration_ns,omitempty"`
	Deadline     time.Time            `json:"deadline,omitzero"`
}

// Validate reports whether the request carries exactly what its kind needs.
func (r HookRequest) Validate() error {
	if r.Operation.IsZero() || r.Tenant.IsZero() || r.Attempt.IsZero() || r.Provider.IsZero() {
		return fmt.Errorf("%w: hook request requires operation, tenant, attempt and provider", ErrInvalid)
	}
	if err := r.Action.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := r.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := r.InputDigest.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	pre := r.Kind == HookPreAction
	switch {
	case !slices.Contains(HookKinds(), r.Kind):
		return fmt.Errorf("%w: unknown hook kind", ErrInvalid)
	case pre != (len(r.Input) > 0) || pre != !r.Deadline.IsZero() || pre == (r.Duration > 0):
		return fmt.Errorf("%w: only a pre-action request carries input and a deadline; others carry a duration", ErrInvalid)
	case pre && identity.DigestOf(r.Input) != r.InputDigest:
		return fmt.Errorf("%w: input does not match its digest", ErrInvalid)
	case (r.Kind == HookPostAction) != (r.OutputDigest != ""):
		return fmt.Errorf("%w: a post-action request, and only that, carries an output digest", ErrInvalid)
	case (r.Kind == HookActionFailure) != (r.Failure != nil):
		return fmt.Errorf("%w: a failure request, and only that, carries a failure", ErrInvalid)
	}
	if r.OutputDigest != "" {
		if err := r.OutputDigest.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if r.Failure != nil {
		return r.Failure.Validate()
	}
	return validArguments(r.Input)
}

// Permission is a pre-action hook's verdict.
type Permission string

const (
	PermissionAllow Permission = "allow"
	PermissionDeny  Permission = "deny"
)

// Permissions lists every Permission.
func Permissions() []Permission {
	return []Permission{PermissionAllow, PermissionDeny}
}

// UsageMeter is usage evidence reported by a hook, in a budget unit.
type UsageMeter struct {
	Unit   lifecycle.Unit `json:"unit"`
	Amount uint64         `json:"amount"`
}

// HookResponse answers a hook request. A pre-action response must give a
// permission and may rewrite the input; any response may return messages for
// the user and the agent, additional context and usage evidence. Hooks
// enforce policy and collect evidence; they never change lifecycle state.
type HookResponse struct {
	Kind              HookKind        `json:"kind"`
	Permission        Permission      `json:"permission,omitzero"`
	UpdatedInput      json.RawMessage `json:"updated_input,omitempty"`
	UserMessage       string          `json:"user_message,omitzero"`
	AgentMessage      string          `json:"agent_message,omitzero"`
	AdditionalContext []string        `json:"additional_context,omitempty"`
	Usage             []UsageMeter    `json:"usage,omitempty"`
}

// Validate reports whether the response fits its kind and is bounded.
func (r HookResponse) Validate() error {
	pre := r.Kind == HookPreAction
	switch {
	case !slices.Contains(HookKinds(), r.Kind):
		return fmt.Errorf("%w: unknown hook kind", ErrInvalid)
	case pre != slices.Contains(Permissions(), r.Permission):
		return fmt.Errorf("%w: a pre-action response, and only that, gives a permission", ErrInvalid)
	case !pre && len(r.UpdatedInput) > 0, r.Permission == PermissionDeny && len(r.UpdatedInput) > 0:
		return fmt.Errorf("%w: only an allowing pre-action response may rewrite input", ErrInvalid)
	case len(r.UserMessage) > maxMessageLength || len(r.AgentMessage) > maxMessageLength || len(r.AdditionalContext) > maxContextItems:
		return fmt.Errorf("%w: hook messages and context are bounded", ErrInvalid)
	}
	for _, context := range r.AdditionalContext {
		if context == "" || len(context) > maxMessageLength {
			return fmt.Errorf("%w: hook context items are non-empty and bounded", ErrInvalid)
		}
	}
	for _, meter := range r.Usage {
		if err := meter.Unit.Validate(); err != nil || meter.Amount > lifecycle.MaxAmount {
			return fmt.Errorf("%w: usage meters need a unit and a bounded amount", ErrInvalid)
		}
	}
	return validArguments(r.UpdatedInput)
}

// HookOutcome is the fail-closed verdict on a pre-action hook.
type HookOutcome string

const (
	HookAllowed   HookOutcome = "allowed"
	HookDenied    HookOutcome = "denied"
	HookMissing   HookOutcome = "missing"
	HookTimedOut  HookOutcome = "timed_out"
	HookMalformed HookOutcome = "malformed"
)

// HookOutcomes lists every HookOutcome.
func HookOutcomes() []HookOutcome {
	return []HookOutcome{HookAllowed, HookDenied, HookMissing, HookTimedOut, HookMalformed}
}

// HookEvidence is the safe audit record of a pre-action decision: digests of
// the original and effective inputs, never the inputs themselves.
type HookEvidence struct {
	Operation       ids.OperationID `json:"operation_id"`
	Outcome         HookOutcome     `json:"outcome"`
	OriginalDigest  identity.Digest `json:"original_digest"`
	EffectiveDigest identity.Digest `json:"effective_digest,omitzero"`
	Rewritten       bool            `json:"rewritten,omitempty"`
}

// Validate reports whether the evidence is consistent with its outcome.
func (e HookEvidence) Validate() error {
	if e.Operation.IsZero() || !slices.Contains(HookOutcomes(), e.Outcome) {
		return fmt.Errorf("%w: hook evidence requires an operation and a known outcome", ErrInvalid)
	}
	if err := e.OriginalDigest.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	allowed := e.Outcome == HookAllowed
	if allowed != (e.EffectiveDigest != "") || (!allowed && e.Rewritten) {
		return fmt.Errorf("%w: only an allowed action has an effective input", ErrInvalid)
	}
	if allowed {
		if err := e.EffectiveDigest.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if e.Rewritten == (e.EffectiveDigest == e.OriginalDigest) {
			return fmt.Errorf("%w: rewritten exactly when the effective input differs", ErrInvalid)
		}
	}
	return nil
}

// HookDecision is the result of a pre-action hook: whether the governed
// action may proceed and, if so, with which effective input.
type HookDecision struct {
	Evidence       HookEvidence
	EffectiveInput json.RawMessage
}

// Allowed reports whether the action may proceed to authorization.
func (d HookDecision) Allowed() bool {
	return d.Evidence.Outcome == HookAllowed
}

// EffectiveRequest returns base with the effective input digest, so authority
// is evaluated against the action as rewritten, never as first proposed.
func (d HookDecision) EffectiveRequest(base identity.ActionRequest) identity.ActionRequest {
	base.InputDigest = d.Evidence.EffectiveDigest
	return base
}

// DecidePreAction applies a pre-action hook's response, failing closed: a
// missing, late, malformed or mismatched response, or a denial, stops the
// action before any operation is dispatched. respondedAt is when the
// response arrived.
func DecidePreAction(request HookRequest, response *HookResponse, respondedAt time.Time) (HookDecision, error) {
	if request.Kind != HookPreAction {
		return HookDecision{}, fmt.Errorf("%w: only pre-action requests are decided", ErrInvalid)
	}
	if err := request.Validate(); err != nil {
		return HookDecision{}, err
	}
	evidence := HookEvidence{Operation: request.Operation, OriginalDigest: request.InputDigest}
	switch {
	case response == nil:
		evidence.Outcome = HookMissing
	case respondedAt.IsZero() || !respondedAt.Before(request.Deadline):
		evidence.Outcome = HookTimedOut
	case response.Kind != HookPreAction || response.Validate() != nil:
		evidence.Outcome = HookMalformed
	case response.Permission != PermissionAllow:
		evidence.Outcome = HookDenied
	default:
		effective := request.Input
		if len(response.UpdatedInput) > 0 {
			effective = response.UpdatedInput
		}
		evidence.Outcome = HookAllowed
		evidence.EffectiveDigest = identity.DigestOf(effective)
		evidence.Rewritten = evidence.EffectiveDigest != evidence.OriginalDigest
		return HookDecision{Evidence: evidence, EffectiveInput: effective}, nil
	}
	return HookDecision{Evidence: evidence}, nil
}

// ActionPrecondition binds a stateful action to the observation it was planned
// on. Token identifies that observation, for example the digest of a
// snapshot; a newer observation of the resource invalidates it.
type ActionPrecondition struct {
	Operation ids.OperationID `json:"operation_id"`
	Resource  string          `json:"resource"`
	Token     identity.Digest `json:"token"`
}

// Validate reports whether the precondition is complete.
func (p ActionPrecondition) Validate() error {
	if p.Operation.IsZero() || p.Resource == "" || len(p.Resource) > maxNameLength*2 {
		return fmt.Errorf("%w: precondition requires an operation and a bounded resource", ErrInvalid)
	}
	if err := p.Token.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// CheckPrecondition refuses an action whose observation token is no longer
// the resource's current one; the caller must observe again and replan.
func CheckPrecondition(precondition ActionPrecondition, resource string, currentToken identity.Digest) *Failure {
	if precondition.Validate() != nil || precondition.Resource != resource {
		return failure(FailureInvalidArguments, "precondition does not name this resource")
	}
	if precondition.Token != currentToken {
		return failure(FailureStalePrecondition, "resource changed since it was observed; observe again")
	}
	return nil
}

// RequireSerial refuses a stateful action on a resource that already has one
// in flight: parallel reasoning does not authorize parallel physical actions.
func RequireSerial(inFlight []ActionPrecondition, next ActionPrecondition) *Failure {
	for _, action := range inFlight {
		if action.Resource == next.Resource && action.Operation != next.Operation {
			return failure(FailureResourceBusy, "another action on this resource is in flight")
		}
	}
	return nil
}
