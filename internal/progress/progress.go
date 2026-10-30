// Package progress applies the Attempt and Task transitions that follow from
// provider operations and observations. The dispatch worker and the event
// ingestion boundary both drive lifecycle state from what providers report;
// sharing these steps keeps them from drifting apart. The rules themselves are
// in internal/lifecycle; this package only loads, checks and stores.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// environmentStopHorizon bounds the stop call of an environment.
const environmentStopHorizon = 24 * time.Hour

// Step is one actor's transitions inside one unit of work.
type Step struct {
	UOW    *store.UnitOfWork
	Tenant ids.TenantID
	Actor  lifecycle.Actor
	At     time.Time
}

func (s Step) transition(expectedVersion uint64) (store.Transition, error) {
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return store.Transition{}, err
	}
	return store.Transition{ExpectedVersion: expectedVersion, At: s.At, Actor: s.Actor, AuditID: auditID}, nil
}

// Attempt applies event to the Attempt when its current state permits it.
// It reports applied=false with a nil error when the state does not permit
// the event, so an observation that already took effect is a no-op; the
// returned refusal lets a caller record a late or contradictory observation.
// A failed Attempt fails its running Task in the same unit of work.
func (s Step) Attempt(ctx context.Context, id ids.AttemptID, event lifecycle.AttemptEvent, target lifecycle.AttemptState) (lifecycle.Attempt, bool, *lifecycle.TransitionError, error) {
	attempt, err := store.GetAttempt(ctx, s.UOW, s.Tenant, id)
	if err != nil {
		return lifecycle.Attempt{}, false, nil, err
	}
	if _, _, check := attempt.Apply(event, attempt.Version, target, s.At); check != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(check, &refusal) {
			return attempt, false, refusal, nil
		}
		return attempt, false, nil, check
	}
	tr, err := s.transition(attempt.Version)
	if err != nil {
		return lifecycle.Attempt{}, false, nil, err
	}
	next, _, err := store.ApplyAttempt(ctx, s.UOW, s.Tenant, id, event, target, tr)
	if err != nil {
		return lifecycle.Attempt{}, false, nil, err
	}
	if err := s.followAttempt(ctx, next); err != nil {
		return lifecycle.Attempt{}, false, nil, err
	}
	if err := s.releaseReservation(ctx, next); err != nil {
		return lifecycle.Attempt{}, false, nil, err
	}
	return next, true, nil, nil
}

// Task applies event to the Task when its current state permits it, with the
// same no-op contract as Attempt.
func (s Step) Task(ctx context.Context, id ids.TaskID, event lifecycle.TaskEvent) (bool, error) {
	task, err := store.GetTask(ctx, s.UOW, s.Tenant, id)
	if err != nil {
		return false, err
	}
	target := lifecycle.TaskState("")
	if event == lifecycle.TaskVerificationFailed {
		target = lifecycle.TaskFailed
	}
	if _, _, check := task.Apply(event, task.Version, target, s.At); check != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(check, &refusal) {
			return false, nil
		}
		return false, check
	}
	tr, err := s.transition(task.Version)
	if err != nil {
		return false, err
	}
	_, _, err = store.ApplyTask(ctx, s.UOW, s.Tenant, id, event, target, tr)
	return err == nil, err
}

// FollowOperation applies what an operation's current state implies for its
// Attempt: an uncertain operation sends a live Attempt to reconciliation and a
// reconciled one resolves it (lifecycle.AttemptFollowsOperation).
func (s Step) FollowOperation(ctx context.Context, operation lifecycle.ProviderOperation) error {
	attempt, err := store.GetAttempt(ctx, s.UOW, s.Tenant, operation.Attempt)
	if err != nil {
		return err
	}
	// A provision reply only acknowledges the request. Readiness is established
	// by an EnvironmentObserved entry, so keep the Attempt provisioning while
	// the assigned environment operation is accepted or being reconciled.
	if attempt.EnvironmentOperation == operation.ID && attempt.State == lifecycle.AttemptProvisioning &&
		(operation.State == lifecycle.OperationAccepted || operation.State == lifecycle.OperationSucceeded || operation.State == lifecycle.OperationUncertain) {
		return nil
	}
	if operation.State == lifecycle.OperationRejected && attempt.State == lifecycle.AttemptCancellationRequested && !attempt.RuntimeStopped {
		// A runtime that rejected its start ran nothing, so nothing else will
		// report the stop this Attempt is waiting for.
		runtime, err := store.GetOperationByAttempt(ctx, s.UOW, s.Tenant, attempt.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil || runtime.ID != operation.ID {
			return err
		}
		_, _, _, err = s.Attempt(ctx, attempt.ID, lifecycle.AttemptRuntimeStopped, "")
		return err
	}
	event, target, ok := lifecycle.AttemptFollowsOperation(operation.State, attempt.State)
	if !ok {
		return nil
	}
	if target == lifecycle.AttemptExecuting && attempt.State == lifecycle.AttemptReconciling {
		task, err := store.GetTask(ctx, s.UOW, s.Tenant, attempt.Task)
		if err != nil {
			return err
		}
		if task.State == lifecycle.TaskVerifying {
			if _, err = store.GetResultProposal(ctx, s.UOW, s.Tenant, attempt.ID); err != nil {
				return err
			}
			target = lifecycle.AttemptFinalizing
		}
	}
	next, applied, _, err := s.Attempt(ctx, attempt.ID, event, target)
	if err != nil || !applied {
		return err
	}
	return s.StopEndedEnvironment(ctx, next.ID)
}

// StopEndedEnvironment stops the resource a completed or failed Attempt leaves
// behind (ADR-0014): its outputs are already admitted, a rework provisions its
// own environment, and nothing else would stop this one. An unbound provision
// has no resource to stop, and a resource already observed stopped needs no
// second stop. Migration settles its own cleanup operations instead.
func (s Step) StopEndedEnvironment(ctx context.Context, id ids.AttemptID) error {
	attempt, err := store.GetAttempt(ctx, s.UOW, s.Tenant, id)
	if err != nil {
		return err
	}
	if (attempt.State != lifecycle.AttemptCompleted && attempt.State != lifecycle.AttemptFailed) || attempt.EnvironmentOperation.IsZero() {
		return nil
	}
	bound, err := store.HasEnvironmentBinding(ctx, s.UOW, s.Tenant, attempt.ID)
	if err != nil || !bound {
		return err
	}
	stopped, err := store.EnvironmentStopped(ctx, s.UOW, s.Tenant, attempt.ID)
	if err != nil || stopped {
		return err
	}
	return s.StopEnvironment(ctx, attempt)
}

// StopEnvironment records and queues a stop of the Attempt's assigned
// environment.
func (s Step) StopEnvironment(ctx context.Context, attempt lifecycle.Attempt) error {
	operationID, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	operation, err := lifecycle.NewProviderOperation(operationID, s.Tenant, attempt.ID, attempt.EnvironmentProvider, s.At)
	if err != nil {
		return err
	}
	if _, _, err := store.InsertOperation(ctx, s.UOW, operation); err != nil {
		return err
	}
	arguments, err := json.Marshal(map[string]string{"provision_operation_id": attempt.EnvironmentOperation.String()})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"operation": operation.ID.String(), "role": contracts.RoleEnvironment, "method": contracts.MethodStop,
		"arguments": json.RawMessage(arguments), "deadline": s.At.Add(environmentStopHorizon).Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	tr, err := s.transition(operation.Version)
	if err != nil {
		return err
	}
	tr.Outbox = &store.OutboxPayload{ID: operation.ID.Untyped(), Attempt: attempt.ID, Demand: contracts.DemandPerson, Body: body}
	_, _, err = store.ApplyOperation(ctx, s.UOW, s.Tenant, operation.ID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr)
	return err
}

func (s Step) followAttempt(ctx context.Context, attempt lifecycle.Attempt) error {
	task, err := store.GetTask(ctx, s.UOW, s.Tenant, attempt.Task)
	if err != nil {
		return err
	}
	if event, ok := lifecycle.TaskFollowsAttempt(attempt.State, task.State); ok {
		_, err := s.Task(ctx, task.ID, event)
		return err
	}
	return nil
}

// releaseReservation closes an Attempt's unused budget hold once its work
// ended, in the same unit of work as the transition that ended it (#15: an
// unused reservation is released exactly once at terminal completion; usage
// arriving later records overshoot). The reservation id derives from the
// attempt id, the derivation the dispatch budget gate already uses, so
// lookups need no unit; an Attempt without a reservation releases nothing and
// an already-released reservation replays as closed.
func (s Step) releaseReservation(ctx context.Context, attempt lifecycle.Attempt) error {
	if !slices.Contains(lifecycle.AttemptTerminalStates(), attempt.State) {
		return nil
	}
	reservationID, err := ids.Parse[ids.Reservation](attempt.ID.String())
	if err != nil {
		return err
	}
	reservation, err := store.GetReservation(ctx, s.UOW, s.Tenant, reservationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if reservation.State == lifecycle.ReservationReleased {
		return nil
	}
	budget, err := store.GetBudget(ctx, s.UOW, s.Tenant, reservation.Budget)
	if err != nil {
		return err
	}
	tr, err := s.transition(budget.Version)
	if err != nil {
		return err
	}
	_, _, err = store.ReleaseBudget(ctx, s.UOW, s.Tenant, reservation.Budget, reservationID, tr)
	return err
}
