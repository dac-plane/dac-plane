// Package contracts defines the provider-neutral adapter protocol: the
// versioned control frames exchanged with runtime, environment, enforcement and
// artifact-store providers, the replayable event stream, event binding,
// action interception hooks and stateful-action preconditions.
//
// Providers report native state; only the control-plane application decides
// whether an observation changes product records. Nothing here performs I/O.
package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const (
	maxArgumentsBytes = 64 << 10
	maxMessageLength  = 1024
	maxNameLength     = 128
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// ErrInvalid reports a malformed contract value.
var ErrInvalid = errors.New("invalid adapter contract value")

func validName(name string) bool {
	return len(name) <= maxNameLength && namePattern.MatchString(name)
}

// FailureCode is a typed protocol or provider failure.
type FailureCode string

const (
	FailureProtocolMismatch      FailureCode = "protocol_mismatch"
	FailureUnsupportedCapability FailureCode = "unsupported_capability"
	FailureMalformedFrame        FailureCode = "malformed_frame"
	FailureUnknownOperation      FailureCode = "unknown_operation"
	FailureInvalidArguments      FailureCode = "invalid_arguments"
	FailureUnauthenticated       FailureCode = "unauthenticated"
	FailureUnauthorized          FailureCode = "unauthorized"
	FailureCancelled             FailureCode = "cancelled"
	FailureDeadlineExceeded      FailureCode = "deadline_exceeded"
	FailureTransportUnavailable  FailureCode = "transport_unavailable"
	FailureProviderFailed        FailureCode = "provider_failed"
	FailureStalePrecondition     FailureCode = "stale_precondition"
	FailureResourceBusy          FailureCode = "resource_busy"
)

// FailureCodes lists every FailureCode.
func FailureCodes() []FailureCode {
	return []FailureCode{
		FailureProtocolMismatch, FailureUnsupportedCapability, FailureMalformedFrame,
		FailureUnknownOperation, FailureInvalidArguments, FailureUnauthenticated, FailureUnauthorized,
		FailureCancelled, FailureDeadlineExceeded, FailureTransportUnavailable, FailureProviderFailed,
		FailureStalePrecondition, FailureResourceBusy,
	}
}

// Failure is a typed failure with a bounded, human-readable message.
type Failure struct {
	Code    FailureCode `json:"code"`
	Message string      `json:"message"`
}

func (f *Failure) Error() string {
	return string(f.Code) + ": " + f.Message
}

// Validate reports whether the failure is typed and bounded.
func (f Failure) Validate() error {
	if !slices.Contains(FailureCodes(), f.Code) || f.Message == "" || len(f.Message) > maxMessageLength {
		return fmt.Errorf("%w: failure requires a known code and a bounded message", ErrInvalid)
	}
	return nil
}

func failure(code FailureCode, format string, args ...any) *Failure {
	return &Failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// FrameKind is the family of a control frame.
type FrameKind string

const (
	FrameLifecycle   FrameKind = "lifecycle"
	FrameRequest     FrameKind = "request"
	FrameCancel      FrameKind = "cancel"
	FrameReply       FrameKind = "reply"
	FrameEvent       FrameKind = "event"
	FrameSubscribe   FrameKind = "subscribe"
	FrameUnsubscribe FrameKind = "unsubscribe"
)

// FrameKinds lists every FrameKind.
func FrameKinds() []FrameKind {
	return []FrameKind{FrameLifecycle, FrameRequest, FrameCancel, FrameReply, FrameEvent, FrameSubscribe, FrameUnsubscribe}
}

// Frame is one control-protocol message: exactly one body matching Kind.
type Frame struct {
	Kind        FrameKind    `json:"kind"`
	Lifecycle   *Lifecycle   `json:"lifecycle,omitempty"`
	Request     *Request     `json:"request,omitempty"`
	Cancel      *Cancel      `json:"cancel,omitempty"`
	Reply       *Reply       `json:"reply,omitempty"`
	Event       *Event       `json:"event,omitempty"`
	Subscribe   *Subscribe   `json:"subscribe,omitempty"`
	Unsubscribe *Unsubscribe `json:"unsubscribe,omitempty"`
}

// Validate reports whether exactly the body named by Kind is present and valid.
func (f Frame) Validate() error {
	bodies := map[FrameKind]bool{
		FrameLifecycle: f.Lifecycle != nil, FrameRequest: f.Request != nil, FrameCancel: f.Cancel != nil,
		FrameReply: f.Reply != nil, FrameEvent: f.Event != nil,
		FrameSubscribe: f.Subscribe != nil, FrameUnsubscribe: f.Unsubscribe != nil,
	}
	for kind, present := range bodies {
		if present != (kind == f.Kind) {
			return fmt.Errorf("%w: frame must carry exactly the %q body", ErrInvalid, f.Kind)
		}
	}
	switch f.Kind {
	case FrameLifecycle:
		return f.Lifecycle.Validate()
	case FrameRequest:
		return f.Request.Validate()
	case FrameCancel:
		return f.Cancel.Validate()
	case FrameReply:
		return f.Reply.Validate()
	case FrameEvent:
		return f.Event.Validate()
	case FrameSubscribe:
		return f.Subscribe.Validate()
	case FrameUnsubscribe:
		return f.Unsubscribe.Validate()
	}
	return fmt.Errorf("%w: unknown frame kind", ErrInvalid)
}

// Phase is a lifecycle step of a session.
type Phase string

const (
	PhaseHello    Phase = "hello"
	PhaseReady    Phase = "ready"
	PhaseShutdown Phase = "shutdown"
)

// Phases lists every Phase.
func Phases() []Phase {
	return []Phase{PhaseHello, PhaseReady, PhaseShutdown}
}

// Capability names an optional protocol feature, for example "stream.replay".
type Capability string

const (
	// CapabilityOperationReplay permits a replacement delivery of the same
	// stable operation and payload after a committed send marker survives its
	// worker. The adapter must return the recorded result when the operation
	// already took effect and must not apply the effect twice.
	CapabilityOperationReplay Capability = "operation.replay"
)

// Lifecycle establishes or ends a session. A hello advertises supported
// versions and offered capabilities, some of which the sender requires; a
// ready acknowledges the single selected version and capability set; a
// shutdown ends the session, with a failure when caused by a protocol error.
type Lifecycle struct {
	Phase    Phase        `json:"phase"`
	Versions []uint32     `json:"versions,omitempty"`
	Offered  []Capability `json:"offered,omitempty"`
	Required []Capability `json:"required,omitempty"`
	Version  uint32       `json:"version,omitempty"`
	Selected []Capability `json:"selected,omitempty"`
	Failure  *Failure     `json:"failure,omitempty"`
}

// Validate reports whether the fields match the phase.
func (l Lifecycle) Validate() error {
	for _, capability := range slices.Concat(l.Offered, l.Required, l.Selected) {
		if !validName(string(capability)) {
			return fmt.Errorf("%w: malformed capability", ErrInvalid)
		}
	}
	switch l.Phase {
	case PhaseHello:
		if len(l.Versions) == 0 || slices.Contains(l.Versions, 0) || l.Version != 0 || len(l.Selected) > 0 || l.Failure != nil {
			return fmt.Errorf("%w: hello carries versions and capabilities only", ErrInvalid)
		}
		for _, required := range l.Required {
			if !slices.Contains(l.Offered, required) {
				return fmt.Errorf("%w: a required capability must be offered", ErrInvalid)
			}
		}
	case PhaseReady:
		if l.Version == 0 || len(l.Versions) > 0 || len(l.Offered) > 0 || len(l.Required) > 0 || l.Failure != nil {
			return fmt.Errorf("%w: ready carries the selected version and capabilities only", ErrInvalid)
		}
	case PhaseShutdown:
		if len(l.Versions) > 0 || len(l.Offered) > 0 || len(l.Required) > 0 || l.Version != 0 || len(l.Selected) > 0 {
			return fmt.Errorf("%w: shutdown carries at most a failure", ErrInvalid)
		}
		if l.Failure != nil {
			return l.Failure.Validate()
		}
	default:
		return fmt.Errorf("%w: unknown lifecycle phase", ErrInvalid)
	}
	return nil
}

// Demand is a scheduling hint. Person-facing work may be preferred; demand
// never changes authorization, budget, permission ceilings or lifecycle rules.
type Demand string

const (
	DemandPerson     Demand = "person"
	DemandBackground Demand = "background"
)

// Demands lists every Demand.
func Demands() []Demand {
	return []Demand{DemandPerson, DemandBackground}
}

// Request asks a provider to perform one operation. Operation is the stable
// identity of the work and the provider's idempotency key: a redelivery keeps
// it and gets a new Delivery. The request is bound to its tenant and Attempt.
type Request struct {
	Operation ids.OperationID `json:"operation_id"`
	Delivery  ids.DeliveryID  `json:"delivery_id"`
	Tenant    ids.TenantID    `json:"tenant_id"`
	Attempt   ids.AttemptID   `json:"attempt_id"`
	Role      Role            `json:"role"`
	Method    Method          `json:"method"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Demand    Demand          `json:"demand"`
	Deadline  time.Time       `json:"deadline"`
}

// Validate reports whether the request is complete, names a method of its
// role, and carries bounded JSON-object arguments.
func (r Request) Validate() error {
	if r.Operation.IsZero() || r.Delivery.IsZero() || r.Tenant.IsZero() || r.Attempt.IsZero() || r.Deadline.IsZero() {
		return fmt.Errorf("%w: request requires operation, delivery, tenant, attempt and deadline", ErrInvalid)
	}
	if !slices.Contains(Demands(), r.Demand) {
		return fmt.Errorf("%w: unknown demand", ErrInvalid)
	}
	if !slices.Contains(Methods(r.Role), r.Method) {
		return fmt.Errorf("%w: %s is not a %s method", ErrInvalid, r.Method, r.Role)
	}
	return validArguments(r.Arguments)
}

func validArguments(arguments json.RawMessage) error {
	if len(arguments) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if len(arguments) > maxArgumentsBytes || json.Unmarshal(arguments, &object) != nil {
		return fmt.Errorf("%w: arguments must be a bounded JSON object", ErrInvalid)
	}
	return nil
}

// Cancel asks the provider to stop the in-flight operation. Cancellation is
// acknowledged by the reply to that operation, not by this frame.
type Cancel struct {
	Operation ids.OperationID `json:"operation_id"`
	Delivery  ids.DeliveryID  `json:"delivery_id"`
}

// Validate reports whether the cancel names its target.
func (c Cancel) Validate() error {
	if c.Operation.IsZero() || c.Delivery.IsZero() {
		return fmt.Errorf("%w: cancel requires the target operation and its delivery", ErrInvalid)
	}
	return nil
}

// Subscribe opens a provider's event stream from a cursor on the session that
// sends it. The provider answers with event frames for the cursor's operation
// and stream, starting with connected, until either side unsubscribes or the
// session ends.
type Subscribe struct {
	Cursor Cursor `json:"cursor"`
}

// Validate reports whether the cursor is complete.
func (s Subscribe) Validate() error {
	return s.Cursor.Validate()
}

// Unsubscribe closes the stream a subscribe opened on the same session.
type Unsubscribe struct {
	Operation ids.OperationID `json:"operation_id"`
	Stream    string          `json:"stream"`
}

// Validate reports whether the unsubscribe names its stream.
func (u Unsubscribe) Validate() error {
	if u.Operation.IsZero() || !validName(u.Stream) {
		return fmt.Errorf("%w: unsubscribe requires the operation and a named stream", ErrInvalid)
	}
	return nil
}

// Outcome is whether an operation succeeded.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeFailed Outcome = "failed"
)

// Outcomes lists every Outcome.
func Outcomes() []Outcome {
	return []Outcome{OutcomeOK, OutcomeFailed}
}

// Reply answers one delivery of a request with a typed success or failure and
// the provider's durability of the request. A success was durably accepted or
// is pending; a failure was rejected with no effect, or has unknown
// durability, which the product reconciles and never treats as no effect.
type Reply struct {
	Operation  ids.OperationID      `json:"operation_id"`
	Delivery   ids.DeliveryID       `json:"delivery_id"`
	Outcome    Outcome              `json:"outcome"`
	Durability lifecycle.SendStatus `json:"durability"`
	Result     json.RawMessage      `json:"result,omitempty"`
	Failure    *Failure             `json:"failure,omitempty"`
}

// Validate reports whether a failure is present exactly when the outcome failed.
func (r Reply) Validate() error {
	if r.Operation.IsZero() || r.Delivery.IsZero() {
		return fmt.Errorf("%w: reply requires the operation and delivery it answers", ErrInvalid)
	}
	switch r.Outcome {
	case OutcomeOK:
		if r.Failure != nil || (r.Durability != lifecycle.SendAccepted && r.Durability != lifecycle.SendPending) {
			return fmt.Errorf("%w: a successful reply is accepted or pending and carries no failure", ErrInvalid)
		}
		return validArguments(r.Result)
	case OutcomeFailed:
		if r.Failure == nil || len(r.Result) > 0 || (r.Durability != lifecycle.SendRejected && r.Durability != lifecycle.SendUnknown) {
			return fmt.Errorf("%w: a failed reply is rejected or of unknown durability and carries a failure", ErrInvalid)
		}
		return r.Failure.Validate()
	}
	return fmt.Errorf("%w: unknown reply outcome", ErrInvalid)
}
