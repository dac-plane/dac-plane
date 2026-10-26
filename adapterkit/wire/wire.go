// Package wire is the ControlFrame codec an adapter speaks to the dac-plane
// control plane (ADR-0008, ADR-0010): one JSON frame per WebSocket text
// message. The types mirror the adapter JSON Schemas, and Decode and Encode
// validate every frame against those schemas, embedded here, so an adapter is
// held to the same contract as the control plane.
//
// Arguments, results and stream entry payloads are carried as raw JSON; their
// meaning belongs to the method or entry kind, not to the frame.
package wire

import (
	"encoding/json"
	"errors"
	"time"
)

// Subprotocol names the framing in the WebSocket handshake.
const Subprotocol = "dac-plane.controlframe.v1"

// ProtocolVersion is the ControlFrame protocol version this package speaks.
const ProtocolVersion uint32 = 1

// CapabilityOperationReplay promises operation-keyed idempotency: a
// replacement delivery of the same operation and payload returns its recorded
// result or applies the effect once.
const CapabilityOperationReplay = "operation.replay"

// DefaultMaxFrameBytes bounds one frame unless a peer configures another limit.
const DefaultMaxFrameBytes = 1 << 20

// ErrMalformed reports a frame that is not one valid ControlFrame.
var ErrMalformed = errors.New("malformed control frame")

// Kind is the family of a frame.
type Kind string

const (
	KindLifecycle   Kind = "lifecycle"
	KindRequest     Kind = "request"
	KindCancel      Kind = "cancel"
	KindReply       Kind = "reply"
	KindEvent       Kind = "event"
	KindSubscribe   Kind = "subscribe"
	KindUnsubscribe Kind = "unsubscribe"
)

// Phase is a lifecycle step of a session.
type Phase string

const (
	PhaseHello    Phase = "hello"
	PhaseReady    Phase = "ready"
	PhaseShutdown Phase = "shutdown"
)

// Role is the part a provider plays.
type Role string

const (
	RoleRuntime       Role = "runtime"
	RoleEnvironment   Role = "environment"
	RoleEnforcement   Role = "enforcement"
	RoleArtifactStore Role = "artifact_store"
)

// Methods is the catalogue of operations of a role.
func Methods(role Role) []string {
	switch role {
	case RoleRuntime:
		return []string{"start_turn", "steer_turn", "interrupt", "export_artifacts"}
	case RoleEnvironment:
		return []string{"provision", "stop", "observe"}
	case RoleEnforcement:
		return []string{"pre_action", "post_action", "action_failed", "execute_action"}
	case RoleArtifactStore:
		return []string{"authorize_upload", "stat_object", "commit_manifest"}
	}
	return nil
}

// Failure codes.
const (
	FailureProtocolMismatch      = "protocol_mismatch"
	FailureUnsupportedCapability = "unsupported_capability"
	FailureMalformedFrame        = "malformed_frame"
	FailureUnknownOperation      = "unknown_operation"
	FailureInvalidArguments      = "invalid_arguments"
	FailureUnauthenticated       = "unauthenticated"
	FailureUnauthorized          = "unauthorized"
	FailureCancelled             = "cancelled"
	FailureDeadlineExceeded      = "deadline_exceeded"
	FailureTransportUnavailable  = "transport_unavailable"
	FailureProviderFailed        = "provider_failed"
	FailureStalePrecondition     = "stale_precondition"
	FailureResourceBusy          = "resource_busy"
)

// Outcomes and durabilities of a reply.
const (
	OutcomeOK     = "ok"
	OutcomeFailed = "failed"

	DurabilityAccepted = "accepted"
	DurabilityPending  = "pending"
	DurabilityRejected = "rejected"
	DurabilityUnknown  = "unknown"
)

// Stream frame kinds.
const (
	StreamConnected    = "connected"
	StreamRows         = "rows"
	StreamCleared      = "cleared"
	StreamCursorTooOld = "cursor_too_old"
	StreamHeartbeat    = "heartbeat"
)

// Frame is one control-protocol message carrying exactly the body Kind names.
type Frame struct {
	Kind        Kind         `json:"kind"`
	Lifecycle   *Lifecycle   `json:"lifecycle,omitempty"`
	Request     *Request     `json:"request,omitempty"`
	Cancel      *Cancel      `json:"cancel,omitempty"`
	Reply       *Reply       `json:"reply,omitempty"`
	Event       *Event       `json:"event,omitempty"`
	Subscribe   *Subscribe   `json:"subscribe,omitempty"`
	Unsubscribe *Unsubscribe `json:"unsubscribe,omitempty"`
}

// Failure is a typed failure with a bounded message.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }

// Lifecycle establishes or ends a session.
type Lifecycle struct {
	Phase    Phase    `json:"phase"`
	Versions []uint32 `json:"versions,omitempty"`
	Offered  []string `json:"offered,omitempty"`
	Required []string `json:"required,omitempty"`
	Version  uint32   `json:"version,omitempty"`
	Selected []string `json:"selected,omitempty"`
	Failure  *Failure `json:"failure,omitempty"`
}

// Request asks the provider to perform one operation. Operation is the
// stable identity of the work and the provider's idempotency key: a
// redelivery keeps it and carries a new Delivery.
type Request struct {
	Operation string          `json:"operation_id"`
	Delivery  string          `json:"delivery_id"`
	Tenant    string          `json:"tenant_id"`
	Attempt   string          `json:"attempt_id"`
	Role      Role            `json:"role"`
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Demand    string          `json:"demand"`
	Deadline  time.Time       `json:"deadline"`
}

// Cancel asks the provider to stop an in-flight operation. It has no reply of
// its own; a stop is observed, not acknowledged.
type Cancel struct {
	Operation string `json:"operation_id"`
	Delivery  string `json:"delivery_id"`
}

// Reply answers one delivery of a request.
type Reply struct {
	Operation  string          `json:"operation_id"`
	Delivery   string          `json:"delivery_id"`
	Outcome    string          `json:"outcome"`
	Durability string          `json:"durability"`
	Result     json.RawMessage `json:"result,omitempty"`
	Failure    *Failure        `json:"failure,omitempty"`
}

// Cursor is ingestion progress on one provider stream.
type Cursor struct {
	Tenant     string `json:"tenant_id"`
	Attempt    string `json:"attempt_id"`
	Operation  string `json:"operation_id"`
	Stream     string `json:"stream"`
	Generation uint64 `json:"generation"`
	After      uint64 `json:"after_updated_sequence"`
}

// Subscribe opens the stream its cursor names on this session.
type Subscribe struct {
	Cursor Cursor `json:"cursor"`
}

// Unsubscribe closes a stream a subscribe opened on this session.
type Unsubscribe struct {
	Operation string `json:"operation_id"`
	Stream    string `json:"stream"`
}

// Entry is one revision of a logical stream entry.
type Entry struct {
	Sequence        uint64          `json:"sequence"`
	UpdatedSequence uint64          `json:"updated_sequence"`
	Kind            string          `json:"kind"`
	PayloadHash     string          `json:"payload_hash"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	PayloadOmitted  bool            `json:"payload_omitted,omitempty"`
}

// Tombstone records an entry deleted at UpdatedSequence.
type Tombstone struct {
	Sequence        uint64 `json:"sequence"`
	UpdatedSequence uint64 `json:"updated_sequence"`
}

// Rows is a batch of entries and tombstones.
type Rows struct {
	Entries    []Entry     `json:"entries,omitempty"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
	Replay     bool        `json:"replay,omitempty"`
}

// StreamFrame is one message of a provider event stream.
type StreamFrame struct {
	Kind          string    `json:"kind"`
	ServerTime    time.Time `json:"server_time,omitzero"`
	Rows          *Rows     `json:"rows,omitempty"`
	NewGeneration uint64    `json:"new_generation,omitempty"`
}

// Event is a stream frame with the binding the provider claims for it. The
// control plane checks the claim against the session's authenticated
// provider and the subscribed cursor.
type Event struct {
	Tenant     string      `json:"tenant_id"`
	Attempt    string      `json:"attempt_id"`
	Provider   string      `json:"provider_id"`
	Operation  string      `json:"operation_id"`
	Stream     string      `json:"stream"`
	Generation uint64      `json:"generation"`
	Body       StreamFrame `json:"body"`
}

// Shutdown returns a shutdown frame, with a failure when one is given.
func Shutdown(failure *Failure) Frame {
	return Frame{Kind: KindLifecycle, Lifecycle: &Lifecycle{Phase: PhaseShutdown, Failure: failure}}
}
