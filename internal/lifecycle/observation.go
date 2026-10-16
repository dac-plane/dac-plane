package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const maxFailureSummaryLength = 512

// TurnOutcome is how a runtime turn ended.
type TurnOutcome string

const (
	TurnCompleted     TurnOutcome = "completed"
	TurnAwaitingInput TurnOutcome = "awaiting_input"
	TurnFailed        TurnOutcome = "failed"
	TurnInterrupted   TurnOutcome = "interrupted"
)

// TurnOutcomes lists every TurnOutcome.
func TurnOutcomes() []TurnOutcome {
	return []TurnOutcome{TurnCompleted, TurnAwaitingInput, TurnFailed, TurnInterrupted}
}

// TurnFailureCode classifies a failed turn without provider-specific names.
type TurnFailureCode string

const (
	TurnFailureRateLimited      TurnFailureCode = "rate_limited"
	TurnFailureTimeout          TurnFailureCode = "timeout"
	TurnFailureContextExhausted TurnFailureCode = "context_exhausted"
	TurnFailureToolFailed       TurnFailureCode = "tool_failed"
	TurnFailureProvider         TurnFailureCode = "provider_error"
	TurnFailureProtocol         TurnFailureCode = "protocol_violation"
	TurnFailureUnknown          TurnFailureCode = "unknown"
)

// TurnFailureCodes lists every TurnFailureCode.
func TurnFailureCodes() []TurnFailureCode {
	return []TurnFailureCode{
		TurnFailureRateLimited, TurnFailureTimeout, TurnFailureContextExhausted, TurnFailureToolFailed,
		TurnFailureProvider, TurnFailureProtocol, TurnFailureUnknown,
	}
}

// TurnFailure is a typed failure report. Summary is a bounded, human-readable
// line; full provider error details are kept out of the record and referenced
// by digest only.
type TurnFailure struct {
	Code          TurnFailureCode `json:"code"`
	Summary       string          `json:"summary"`
	DetailsDigest identity.Digest `json:"details_digest,omitzero"`
}

// Validate reports whether the failure is typed and bounded.
func (f TurnFailure) Validate() error {
	if !slices.Contains(TurnFailureCodes(), f.Code) {
		return fmt.Errorf("%w: unknown turn failure code", ErrInvalid)
	}
	if f.Summary == "" || len(f.Summary) > maxFailureSummaryLength {
		return fmt.Errorf("%w: turn failure requires a bounded summary", ErrInvalid)
	}
	if f.DetailsDigest != "" {
		return validDigest(f.DetailsDigest)
	}
	return nil
}

// TurnEnd is the observed end of one runtime turn of a bound session. A turn
// end is an observation: the application decides what it means for the
// Attempt. awaiting_input is the explicit signal that work waits for a person.
type TurnEnd struct {
	Tenant     ids.TenantID   `json:"tenant_id"`
	Attempt    ids.AttemptID  `json:"attempt_id"`
	Provider   ids.ProviderID `json:"provider_id"`
	SessionRef string         `json:"session_ref"`
	TurnRef    string         `json:"turn_ref"`
	Outcome    TurnOutcome    `json:"outcome"`
	Failure    *TurnFailure   `json:"failure,omitempty"`
	EndedAt    time.Time      `json:"ended_at"`
}

// Validate reports whether the turn end is complete and a failure is present
// exactly when the turn failed.
func (e TurnEnd) Validate() error {
	if e.Tenant.IsZero() || e.Attempt.IsZero() || e.Provider.IsZero() || e.EndedAt.IsZero() {
		return fmt.Errorf("%w: turn end requires tenant, attempt, provider and time", ErrInvalid)
	}
	if !validNativeRef(e.SessionRef) || !validNativeRef(e.TurnRef) {
		return fmt.Errorf("%w: turn end requires bounded session and turn references", ErrInvalid)
	}
	if !slices.Contains(TurnOutcomes(), e.Outcome) {
		return fmt.Errorf("%w: unknown turn outcome", ErrInvalid)
	}
	if (e.Outcome == TurnFailed) != (e.Failure != nil) {
		return fmt.Errorf("%w: a failure is reported exactly when the turn failed", ErrInvalid)
	}
	if e.Failure != nil {
		return e.Failure.Validate()
	}
	return nil
}

// RunState is a provider's observed state of an environment resource.
type RunState string

const (
	RunAbsent     RunState = "absent"
	RunStarting   RunState = "starting"
	RunRunning    RunState = "running"
	RunHibernated RunState = "hibernated"
	RunUnknown    RunState = "unknown"
)

// RunStates lists every RunState.
func RunStates() []RunState {
	return []RunState{RunAbsent, RunStarting, RunRunning, RunHibernated, RunUnknown}
}

// EnvironmentObservation records a provider's report of a bound environment
// resource. The product stores observed state and correlation; it does not
// become a second compute controller. Sequence is the provider's monotonic
// order for the resource, and unknown is a first-class partial observation.
type EnvironmentObservation struct {
	Tenant      ids.TenantID   `json:"tenant_id"`
	Attempt     ids.AttemptID  `json:"attempt_id"`
	Provider    ids.ProviderID `json:"provider_id"`
	ResourceRef string         `json:"resource_ref"`
	State       RunState       `json:"state"`
	Sequence    uint64         `json:"sequence"`
	ObservedAt  time.Time      `json:"observed_at"`
}

// Validate reports whether the observation is complete.
func (o EnvironmentObservation) Validate() error {
	if o.Tenant.IsZero() || o.Attempt.IsZero() || o.Provider.IsZero() || o.ObservedAt.IsZero() || o.Sequence == 0 {
		return fmt.Errorf("%w: observation requires identifiers, a positive sequence and time", ErrInvalid)
	}
	if !validNativeRef(o.ResourceRef) {
		return fmt.Errorf("%w: observation requires a bounded resource reference", ErrInvalid)
	}
	if !slices.Contains(RunStates(), o.State) {
		return fmt.Errorf("%w: unknown run state", ErrInvalid)
	}
	return nil
}

// Supersedes reports whether o is a newer observation of the same resource
// than previous. Replayed or out-of-order observations do not supersede.
func (o EnvironmentObservation) Supersedes(previous EnvironmentObservation) bool {
	return o.Tenant == previous.Tenant && o.Provider == previous.Provider &&
		o.ResourceRef == previous.ResourceRef && o.Sequence > previous.Sequence
}

// Stopped reports whether the resource is observed as no longer running,
// which is the environment half of an Attempt's stop confirmation.
func (o EnvironmentObservation) Stopped() bool {
	return o.State == RunAbsent || o.State == RunHibernated
}
