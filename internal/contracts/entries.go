package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// MaxInlinePayload bounds an entry's inline payload; larger content travels
// with payload_omitted and its hash. The default follows the historical
// specification's proposed event limit.
const MaxInlinePayload = 256 << 10

const maxRefLength = 256

// Entry kinds the product understands. Names follow the documented event
// families: runtime and environment observations, turn progress and the
// reconciliation outcome. Payloads are provider observations only: the
// product takes tenant, Attempt, provider and operation from the bound event,
// never from a payload.
const (
	EntryRuntimeStarted      = "runtime.started"
	EntryTurnStarted         = "turn.started"
	EntryTurnFinished        = "turn.finished"
	EntryEnvironmentObserved = "environment.observed"
	EntryOperationOutcome    = "operation_outcome"
	EntryAttentionRequested  = "attention.requested"
	EntryDecisionConsumed    = "decision.consumed"
	EntryRuntimeStopped      = "runtime.stopped"
	EntryResultProposed      = "result.proposed"
)

// progressKinds are known observations that carry no product state.
var progressKinds = []string{"progress", "message.delta", "tool.proposed", "tool.finished", "checkpoint.created"}

// stateChangingPrefixes name entry families that carry lifecycle or
// permission semantics; an unknown kind in one of them is refused.
var stateChangingPrefixes = []string{"task.", "attempt.", "attention.", "decision.", "action.", "result.", "environment."}

// EntryClass is how the product treats an entry kind.
type EntryClass string

const (
	// EntryApply is a known kind with product effects.
	EntryApply EntryClass = "apply"
	// EntryProgress is acknowledged and changes nothing: known progress, or an
	// unknown kind without state-changing or permission semantics.
	EntryProgress EntryClass = "progress"
	// EntryRefuse is an unknown kind that claims state-changing or permission
	// semantics. An observation cannot grant permission by naming itself.
	EntryRefuse EntryClass = "refuse"
)

// ClassifyEntry sorts an entry kind into apply, progress or refuse.
func ClassifyEntry(kind string) EntryClass {
	switch {
	case slices.Contains([]string{
		EntryRuntimeStarted, EntryTurnStarted, EntryTurnFinished, EntryEnvironmentObserved,
		EntryOperationOutcome, EntryAttentionRequested, EntryDecisionConsumed, EntryRuntimeStopped, EntryResultProposed,
	}, kind):
		return EntryApply
	case slices.Contains(progressKinds, kind):
		return EntryProgress
	}
	lower := strings.ToLower(kind)
	for _, prefix := range stateChangingPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return EntryRefuse
		}
	}
	for _, word := range []string{"approv", "grant", "permission"} {
		if strings.Contains(lower, word) {
			return EntryRefuse
		}
	}
	return EntryProgress
}

func validRef(ref string) bool {
	return ref != "" && len(ref) <= maxRefLength
}

// RuntimeStarted reports a runtime session starting for the event's Attempt.
type RuntimeStarted struct {
	NativeRef string `json:"native_ref"`
}

// Validate reports whether the session reference is bounded.
func (r RuntimeStarted) Validate() error {
	if !validRef(r.NativeRef) {
		return fmt.Errorf("%w: runtime.started requires a bounded session reference", ErrInvalid)
	}
	return nil
}

// TurnStarted reports a turn starting in a session.
type TurnStarted struct {
	NativeRef  string `json:"native_ref"`
	SessionRef string `json:"session_ref"`
}

// Validate reports whether both references are bounded.
func (t TurnStarted) Validate() error {
	if !validRef(t.NativeRef) || !validRef(t.SessionRef) {
		return fmt.Errorf("%w: turn.started requires bounded turn and session references", ErrInvalid)
	}
	return nil
}

// TurnFinished reports how a turn ended, with a typed failure when it failed.
type TurnFinished struct {
	SessionRef string                 `json:"session_ref"`
	TurnRef    string                 `json:"turn_ref"`
	Outcome    lifecycle.TurnOutcome  `json:"outcome"`
	Failure    *lifecycle.TurnFailure `json:"failure,omitempty"`
	EndedAt    time.Time              `json:"ended_at"`
}

// Validate reports whether the observation forms a valid turn end.
func (t TurnFinished) Validate() error {
	if !validRef(t.SessionRef) || !validRef(t.TurnRef) || t.EndedAt.IsZero() {
		return fmt.Errorf("%w: turn.finished requires session, turn and end time", ErrInvalid)
	}
	if !slices.Contains(lifecycle.TurnOutcomes(), t.Outcome) || (t.Outcome == lifecycle.TurnFailed) != (t.Failure != nil) {
		return fmt.Errorf("%w: turn.finished requires a known outcome and a failure exactly when failed", ErrInvalid)
	}
	if t.Failure != nil {
		if err := t.Failure.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// EnvironmentObserved reports an environment resource's run state.
type EnvironmentObserved struct {
	ResourceRef string             `json:"resource_ref"`
	State       lifecycle.RunState `json:"state"`
	Sequence    uint64             `json:"sequence"`
	ObservedAt  time.Time          `json:"observed_at"`
}

// Validate reports whether the observation is complete.
func (e EnvironmentObserved) Validate() error {
	if !validRef(e.ResourceRef) || e.Sequence == 0 || e.ObservedAt.IsZero() || !slices.Contains(lifecycle.RunStates(), e.State) {
		return fmt.Errorf("%w: environment.observed requires a resource, a known state, a sequence and a time", ErrInvalid)
	}
	return nil
}

// OperationOutcome reports the provider's settled outcome of the operation.
type OperationOutcome struct {
	State lifecycle.OperationState `json:"state"`
}

// Validate reports whether the outcome is one a provider can settle on.
func (o OperationOutcome) Validate() error {
	switch o.State {
	case lifecycle.OperationAccepted, lifecycle.OperationSucceeded, lifecycle.OperationFailed, lifecycle.OperationRejected:
		return nil
	}
	return fmt.Errorf("%w: operation_outcome requires accepted, succeeded, failed or rejected", ErrInvalid)
}

// AttentionRequested reports that the runtime needs a responder: a question, an
// action approval naming the governed action and its digest, an authentication
// handoff or blocked work. The request never names product identities; the
// ingestion boundary resolves them from the bound event.
type AttentionRequested struct {
	Scope        identity.Scope          `json:"scope,omitzero"`
	NativeRef    string                  `json:"native_ref"`
	Kind         lifecycle.AttentionKind `json:"kind"`
	Action       identity.ActionClass    `json:"action,omitempty"`
	ActionDigest identity.Digest         `json:"action_digest,omitempty"`
	ExpiresAt    time.Time               `json:"expires_at"`
}

// Validate reports whether the request names a known kind, and an approval
// carries its action exactly.
func (a AttentionRequested) Validate() error {
	if !validRef(a.NativeRef) || a.ExpiresAt.IsZero() || !slices.Contains(lifecycle.AttentionKinds(), a.Kind) {
		return fmt.Errorf("%w: attention.requested requires a bounded native reference, a known kind and an expiry", ErrInvalid)
	}
	if a.Kind == lifecycle.AttentionActionApproval {
		if a.Scope != (identity.Scope{}) {
			if err := a.Scope.Validate(); err != nil {
				return err
			}
		}
		if a.Action == "" || a.ActionDigest == "" {
			return fmt.Errorf("%w: attention.requested approval requires an action and its digest", ErrInvalid)
		}
		if err := a.Action.Validate(); err != nil {
			return fmt.Errorf("%w: attention.requested action: %w", ErrInvalid, err)
		}
		if err := a.ActionDigest.Validate(); err != nil {
			return fmt.Errorf("%w: attention.requested action digest: %w", ErrInvalid, err)
		}
		return nil
	}
	if a.Action != "" || a.ActionDigest != "" || a.Scope != (identity.Scope{}) {
		return fmt.Errorf("%w: only an action approval may carry an action", ErrInvalid)
	}
	return nil
}

// DecisionConsumed reports the runtime consuming a delivered decision.
type DecisionConsumed struct {
	NativeRef string `json:"native_ref"`
}

// Validate reports whether the observation names the native request it consumed.
func (d DecisionConsumed) Validate() error {
	if !validRef(d.NativeRef) {
		return fmt.Errorf("%w: decision.consumed requires a bounded native reference", ErrInvalid)
	}
	return nil
}

// RuntimeStopped reports the runtime's own end of work after a stop request,
// mirroring the interrupt acknowledgement: whether a run was actually active.
type RuntimeStopped struct {
	HadActiveRun bool `json:"had_active_run"`
}

// UnmarshalJSON preserves the distinction between a reported false value and
// an omitted required field. A plain bool would decode both as false and make
// the Go boundary weaker than the JSON Schema contract.
func (r *RuntimeStopped) UnmarshalJSON(data []byte) error {
	var wire struct {
		HadActiveRun *bool `json:"had_active_run"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if wire.HadActiveRun == nil {
		return fmt.Errorf("%w: runtime.stopped requires had_active_run", ErrInvalid)
	}
	r.HadActiveRun = *wire.HadActiveRun
	return nil
}

// Validate reports whether the observation is a well-formed stop.
func (r RuntimeStopped) Validate() error {
	return nil
}

// DecodeEntry decodes and validates an inline payload into T, rejecting
// unknown fields so a payload cannot smuggle identities or extra claims.
func DecodeEntry[T interface{ Validate() error }](entry Entry) (T, error) {
	var value T
	if entry.PayloadOmitted || len(entry.Payload) == 0 {
		return value, fmt.Errorf("%w: %s needs its payload inline", ErrInvalid, entry.Kind)
	}
	if len(entry.Payload) > MaxInlinePayload {
		return value, fmt.Errorf("%w: inline payload over %d bytes", ErrInvalid, MaxInlinePayload)
	}
	decoder := json.NewDecoder(strings.NewReader(string(entry.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("%w: %s payload: %w", ErrInvalid, entry.Kind, err)
	}
	return value, value.Validate()
}
