package lifecycle

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const maxNativeRefLength = 256

// ErrBindingConflict reports a native resource already bound to another Attempt.
var ErrBindingConflict = errors.New("native resource is bound to another attempt")

// ResourceKind is the kind of provider-native resource a Binding correlates.
type ResourceKind string

const (
	ResourceRuntimeSession      ResourceKind = "runtime_session"
	ResourceRuntimeTurn         ResourceKind = "runtime_turn"
	ResourceEnvironmentResource ResourceKind = "environment_resource"
)

// ResourceKinds lists every ResourceKind.
func ResourceKinds() []ResourceKind {
	return []ResourceKind{ResourceRuntimeSession, ResourceRuntimeTurn, ResourceEnvironmentResource}
}

// Binding correlates one provider-native resource to exactly one Attempt and
// the provider operation that produced it. The resource stays provider-owned:
// a runtime session or turn is never a product Task, and an environment
// resource's lifecycle stays with its provider. NativeRef is the provider's
// opaque identifier; a turn also names the session it belongs to.
type Binding struct {
	Tenant     ids.TenantID    `json:"tenant_id"`
	Attempt    ids.AttemptID   `json:"attempt_id"`
	Provider   ids.ProviderID  `json:"provider_id"`
	Operation  ids.OperationID `json:"operation_id"`
	Kind       ResourceKind    `json:"kind"`
	NativeRef  string          `json:"native_ref"`
	SessionRef string          `json:"session_ref,omitzero"`
	BoundAt    time.Time       `json:"bound_at"`
}

// Validate reports whether the binding is complete.
func (b Binding) Validate() error {
	if b.Tenant.IsZero() || b.Attempt.IsZero() || b.Provider.IsZero() || b.Operation.IsZero() || b.BoundAt.IsZero() {
		return fmt.Errorf("%w: binding requires tenant, attempt, provider, operation and time", ErrInvalid)
	}
	if !slices.Contains(ResourceKinds(), b.Kind) {
		return fmt.Errorf("%w: unknown resource kind", ErrInvalid)
	}
	if !validNativeRef(b.NativeRef) {
		return fmt.Errorf("%w: binding requires a bounded native reference", ErrInvalid)
	}
	if (b.Kind == ResourceRuntimeTurn) != (b.SessionRef != "") || len(b.SessionRef) > maxNativeRefLength {
		return fmt.Errorf("%w: a turn, and only a turn, names its session", ErrInvalid)
	}
	return nil
}

// sameResource reports whether two bindings name the same native resource.
func (b Binding) sameResource(other Binding) bool {
	return b.Tenant == other.Tenant && b.Provider == other.Provider && b.Kind == other.Kind && b.NativeRef == other.NativeRef
}

// CheckBinding rejects a candidate whose native resource is already bound to a
// different Attempt. Rebinding the same resource to the same Attempt is an
// idempotent replay and is accepted.
func CheckBinding(existing []Binding, candidate Binding) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	for _, bound := range existing {
		if bound.sameResource(candidate) && bound.Attempt != candidate.Attempt {
			return ErrBindingConflict
		}
	}
	return nil
}

func validNativeRef(ref string) bool {
	return ref != "" && len(ref) <= maxNativeRefLength
}

// OperationState is the product's view of one provider operation.
type OperationState string

const (
	OperationRecorded   OperationState = "recorded"
	OperationDispatched OperationState = "dispatched"
	OperationAccepted   OperationState = "accepted"
	OperationUncertain  OperationState = "uncertain"
	OperationSucceeded  OperationState = "succeeded"
	OperationFailed     OperationState = "failed"
	OperationRejected   OperationState = "rejected"
)

// OperationStates lists every OperationState.
func OperationStates() []OperationState {
	return []OperationState{
		OperationRecorded, OperationDispatched, OperationAccepted, OperationUncertain,
		OperationSucceeded, OperationFailed, OperationRejected,
	}
}

// OperationEvent is a dispatch, a send result or an outcome observation.
type OperationEvent string

const (
	OperationDispatch       OperationEvent = "dispatch"
	OperationSendAccepted   OperationEvent = "send_accepted"
	OperationSendRejected   OperationEvent = "send_rejected"
	OperationSendUnknown    OperationEvent = "send_unknown"
	OperationSucceed        OperationEvent = "succeed"
	OperationFail           OperationEvent = "fail"
	OperationOutcomeUnknown OperationEvent = "outcome_unknown"
	OperationReconcile      OperationEvent = "reconcile"
)

// OperationEvents lists every OperationEvent.
func OperationEvents() []OperationEvent {
	return []OperationEvent{
		OperationDispatch, OperationSendAccepted, OperationSendRejected, OperationSendUnknown,
		OperationSucceed, OperationFail, OperationOutcomeUnknown, OperationReconcile,
	}
}

// SendStatus is a provider's report of whether it durably received a request.
type SendStatus string

const (
	SendAccepted SendStatus = "accepted"
	SendRejected SendStatus = "rejected"
	SendPending  SendStatus = "pending"
	SendUnknown  SendStatus = "unknown"
)

// SendStatuses lists every SendStatus.
func SendStatuses() []SendStatus {
	return []SendStatus{SendAccepted, SendRejected, SendPending, SendUnknown}
}

// Event maps a send status to the operation event it causes. Pending changes
// nothing and returns "". Unknown durability is an indeterminate outcome, not
// a failure: it enters reconciliation and never permits redispatch.
func (s SendStatus) Event() OperationEvent {
	switch s {
	case SendAccepted:
		return OperationSendAccepted
	case SendRejected:
		return OperationSendRejected
	case SendUnknown:
		return OperationSendUnknown
	default:
		return ""
	}
}

// operationMachine never leads from uncertain back to recorded or dispatched:
// an operation whose effect is unknown is reconciled, never resent.
var operationMachine = newMachine(OperationStates(), OperationEvents(),
	[]OperationState{OperationSucceeded, OperationFailed, OperationRejected},
	map[OperationState]map[OperationEvent][]OperationState{
		OperationRecorded: {
			OperationDispatch: {OperationDispatched},
		},
		OperationDispatched: {
			OperationSendAccepted: {OperationAccepted},
			OperationSendRejected: {OperationRejected},
			OperationSendUnknown:  {OperationUncertain},
		},
		OperationAccepted: {
			OperationSucceed:        {OperationSucceeded},
			OperationFail:           {OperationFailed},
			OperationOutcomeUnknown: {OperationUncertain},
		},
		OperationUncertain: {
			OperationReconcile: {OperationAccepted, OperationSucceeded, OperationFailed, OperationRejected},
		},
	},
)

// ProviderOperation is the stable identity of one request to a provider. The
// same identity is used for idempotent delivery and for reconciliation.
type ProviderOperation struct {
	ID        ids.OperationID `json:"operation_id"`
	Tenant    ids.TenantID    `json:"tenant_id"`
	Attempt   ids.AttemptID   `json:"attempt_id"`
	Provider  ids.ProviderID  `json:"provider_id"`
	State     OperationState  `json:"state"`
	Version   uint64          `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// NewProviderOperation returns a recorded operation at version 1.
func NewProviderOperation(id ids.OperationID, tenant ids.TenantID, attempt ids.AttemptID, provider ids.ProviderID, at time.Time) (ProviderOperation, error) {
	operation := ProviderOperation{
		ID: id, Tenant: tenant, Attempt: attempt, Provider: provider,
		State: OperationRecorded, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	return operation, operation.Validate()
}

// Validate reports whether the record is complete.
func (o ProviderOperation) Validate() error {
	if o.ID.IsZero() || o.Tenant.IsZero() || o.Attempt.IsZero() || o.Provider.IsZero() || o.Version == 0 {
		return fmt.Errorf("%w: provider operation requires identifiers and a positive version", ErrInvalid)
	}
	if !slices.Contains(OperationStates(), o.State) {
		return fmt.Errorf("%w: unknown operation state", ErrInvalid)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.Before(o.CreatedAt) {
		return fmt.Errorf("%w: provider operation timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Apply performs event if the operation is still at expectedVersion.
func (o ProviderOperation) Apply(event OperationEvent, expectedVersion uint64, target OperationState, at time.Time) (ProviderOperation, Change, error) {
	change := Change{
		Tenant: o.Tenant, Record: RecordOperation, RecordID: o.ID.Untyped(),
		Event: string(event), From: string(o.State), Version: o.Version,
	}
	next, change, err := transition(operationMachine, change, o.State, o.Version, expectedVersion, event, target)
	if err != nil {
		return o, change, err
	}
	o.State, o.Version, o.UpdatedAt = next, change.Version, at
	return o, change, nil
}
