package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// AttemptState is the product lifecycle state of one execution of a Task.
type AttemptState string

const (
	AttemptAllocated             AttemptState = "allocated"
	AttemptProvisioning          AttemptState = "provisioning"
	AttemptStarting              AttemptState = "starting"
	AttemptExecuting             AttemptState = "executing"
	AttemptWaitingInput          AttemptState = "waiting_input"
	AttemptFinalizing            AttemptState = "finalizing"
	AttemptCancellationRequested AttemptState = "cancellation_requested"
	AttemptReconciling           AttemptState = "reconciling"
	AttemptCompleted             AttemptState = "completed"
	AttemptFailed                AttemptState = "failed"
	AttemptTerminated            AttemptState = "terminated"
)

// AttemptStates lists every AttemptState.
func AttemptStates() []AttemptState {
	return []AttemptState{
		AttemptAllocated, AttemptProvisioning, AttemptStarting, AttemptExecuting, AttemptWaitingInput,
		AttemptFinalizing, AttemptCancellationRequested, AttemptReconciling, AttemptCompleted,
		AttemptFailed, AttemptTerminated,
	}
}

// AttemptEvent is a command or accepted observation that may change an Attempt.
type AttemptEvent string

const (
	AttemptProvision          AttemptEvent = "provision"
	AttemptStart              AttemptEvent = "start"
	AttemptRuntimeStarted     AttemptEvent = "runtime_started"
	AttemptAwaitInput         AttemptEvent = "await_input"
	AttemptResume             AttemptEvent = "resume"
	AttemptFinalize           AttemptEvent = "finalize"
	AttemptComplete           AttemptEvent = "complete"
	AttemptFail               AttemptEvent = "fail"
	AttemptRequestCancel      AttemptEvent = "request_cancel"
	AttemptRuntimeStopped     AttemptEvent = "runtime_stopped"
	AttemptEnvironmentStopped AttemptEvent = "environment_stopped"
	AttemptOutcomeUnknown     AttemptEvent = "outcome_unknown"
	AttemptLeaseExpired       AttemptEvent = "lease_expired"
	AttemptReconciled         AttemptEvent = "reconciled"
)

// AttemptEvents lists every AttemptEvent.
func AttemptEvents() []AttemptEvent {
	return []AttemptEvent{
		AttemptProvision, AttemptStart, AttemptRuntimeStarted, AttemptAwaitInput, AttemptResume,
		AttemptFinalize, AttemptComplete, AttemptFail, AttemptRequestCancel, AttemptRuntimeStopped,
		AttemptEnvironmentStopped, AttemptOutcomeUnknown, AttemptLeaseExpired, AttemptReconciled,
	}
}

// active adds the transitions every running attempt state shares: a definite
// failure, a cancellation request, and an unknown outcome or lost lease, both
// of which go to reconciliation rather than being treated as no effect.
func active(transitions map[AttemptEvent][]AttemptState) map[AttemptEvent][]AttemptState {
	transitions[AttemptFail] = []AttemptState{AttemptFailed}
	transitions[AttemptRequestCancel] = []AttemptState{AttemptCancellationRequested}
	transitions[AttemptOutcomeUnknown] = []AttemptState{AttemptReconciling}
	transitions[AttemptLeaseExpired] = []AttemptState{AttemptReconciling}
	return transitions
}

var stopTargets = []AttemptState{AttemptCancellationRequested, AttemptTerminated}

// attemptMachine follows the historical specification §11, with an explicit
// reconciling state for unknown outcomes and lost leases. Reconciliation can
// resolve to an observed state but never back to a pre-dispatch state, so an
// unknown outcome is never redispatched.
var attemptMachine = newMachine(AttemptStates(), AttemptEvents(),
	[]AttemptState{AttemptCompleted, AttemptFailed, AttemptTerminated},
	map[AttemptState]map[AttemptEvent][]AttemptState{
		// Nothing has been started, so cancelling needs no stop observation.
		AttemptAllocated: {
			AttemptProvision:     {AttemptProvisioning},
			AttemptFail:          {AttemptFailed},
			AttemptRequestCancel: {AttemptTerminated},
		},
		AttemptProvisioning: active(map[AttemptEvent][]AttemptState{AttemptStart: {AttemptStarting}}),
		AttemptStarting:     active(map[AttemptEvent][]AttemptState{AttemptRuntimeStarted: {AttemptExecuting}}),
		AttemptExecuting: active(map[AttemptEvent][]AttemptState{
			AttemptAwaitInput: {AttemptWaitingInput},
			AttemptFinalize:   {AttemptFinalizing},
		}),
		AttemptWaitingInput: active(map[AttemptEvent][]AttemptState{AttemptResume: {AttemptExecuting}}),
		AttemptFinalizing:   active(map[AttemptEvent][]AttemptState{AttemptComplete: {AttemptCompleted}}),
		// Cancellation is intent: the attempt terminates only after both the
		// runtime and the environment stop are observed, or reconciliation
		// establishes that they stopped.
		AttemptCancellationRequested: {
			AttemptRuntimeStopped:     stopTargets,
			AttemptEnvironmentStopped: stopTargets,
			AttemptReconciled:         {AttemptTerminated},
		},
		AttemptReconciling: {
			AttemptReconciled: {
				AttemptExecuting, AttemptWaitingInput, AttemptFinalizing,
				AttemptCompleted, AttemptFailed, AttemptTerminated,
			},
			AttemptRequestCancel: {AttemptCancellationRequested},
		},
	},
)

// Attempt is one execution of a Task through a runtime and an environment.
// RuntimeStopped and EnvironmentStopped record stop observations received
// while cancellation is requested.
type Attempt struct {
	ID                 ids.AttemptID `json:"attempt_id"`
	Tenant             ids.TenantID  `json:"tenant_id"`
	Task               ids.TaskID    `json:"task_id"`
	Number             uint32        `json:"number"`
	State              AttemptState  `json:"state"`
	Version            uint64        `json:"version"`
	RuntimeStopped     bool          `json:"runtime_stopped,omitempty"`
	EnvironmentStopped bool          `json:"environment_stopped,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// NewAttempt returns an allocated Attempt at version 1. number is the 1-based
// position of this attempt among its task's attempts.
func NewAttempt(id ids.AttemptID, tenant ids.TenantID, task ids.TaskID, number uint32, at time.Time) (Attempt, error) {
	attempt := Attempt{
		ID: id, Tenant: tenant, Task: task, Number: number,
		State: AttemptAllocated, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	return attempt, attempt.Validate()
}

// Validate reports whether the record is complete. Stop observations exist
// only once cancellation has been requested.
func (a Attempt) Validate() error {
	if a.ID.IsZero() || a.Tenant.IsZero() || a.Task.IsZero() || a.Number == 0 || a.Version == 0 {
		return fmt.Errorf("%w: attempt requires identifiers, a positive number and a positive version", ErrInvalid)
	}
	if !slices.Contains(AttemptStates(), a.State) {
		return fmt.Errorf("%w: unknown attempt state", ErrInvalid)
	}
	if (a.RuntimeStopped || a.EnvironmentStopped) &&
		a.State != AttemptCancellationRequested && a.State != AttemptTerminated {
		return fmt.Errorf("%w: stop observations exist only during cancellation", ErrInvalid)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return fmt.Errorf("%w: attempt timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Apply performs event if the attempt is still at expectedVersion. target
// selects among several legal targets and may be empty when there is only one.
// For stop observations the target is determined by the observations so far
// and may be left empty.
func (a Attempt) Apply(event AttemptEvent, expectedVersion uint64, target AttemptState, at time.Time) (Attempt, Change, error) {
	change := Change{
		Tenant: a.Tenant, Record: RecordAttempt, RecordID: a.ID.Untyped(),
		Event: string(event), From: string(a.State), Version: a.Version,
	}

	runtimeStopped, environmentStopped := a.RuntimeStopped, a.EnvironmentStopped
	if a.State == AttemptCancellationRequested && a.Version == expectedVersion {
		switch event {
		case AttemptRuntimeStopped, AttemptEnvironmentStopped:
			if (event == AttemptRuntimeStopped && runtimeStopped) || (event == AttemptEnvironmentStopped && environmentStopped) {
				return a, change, reject(change, RejectStopAlreadyObserved)
			}
			runtimeStopped = runtimeStopped || event == AttemptRuntimeStopped
			environmentStopped = environmentStopped || event == AttemptEnvironmentStopped
			required := AttemptCancellationRequested
			if runtimeStopped && environmentStopped {
				required = AttemptTerminated
			}
			if target != "" && target != required {
				return a, change, reject(change, RejectInvalidTarget)
			}
			target = required
		}
	}

	next, code := attemptMachine.next(a.State, a.Version, expectedVersion, event, target)
	if code != "" {
		return a, change, reject(change, code)
	}
	a.State = next
	a.Version++
	a.RuntimeStopped, a.EnvironmentStopped = runtimeStopped, environmentStopped
	a.UpdatedAt = at
	change.To = string(next)
	change.Version = a.Version
	return a, change, nil
}
