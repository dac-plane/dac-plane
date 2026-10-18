package contracts

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

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
	case slices.Contains([]string{EntryRuntimeStarted, EntryTurnStarted, EntryTurnFinished, EntryEnvironmentObserved, EntryOperationOutcome}, kind):
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
