// Package progress applies the Attempt and Task transitions that follow from
// provider operations and observations. The dispatch worker and the event
// ingestion boundary both drive lifecycle state from what providers report;
// sharing these steps keeps them from drifting apart. The rules themselves are
// in internal/lifecycle; this package only loads, checks and stores.
package progress

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

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
	return next, true, nil, nil
}

// Task applies event to the Task when its current state permits it, with the
// same no-op contract as Attempt.
func (s Step) Task(ctx context.Context, id ids.TaskID, event lifecycle.TaskEvent) (bool, error) {
	task, err := store.GetTask(ctx, s.UOW, s.Tenant, id)
	if err != nil {
		return false, err
	}
	if _, _, check := task.Apply(event, task.Version, "", s.At); check != nil {
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
	_, _, err = store.ApplyTask(ctx, s.UOW, s.Tenant, id, event, "", tr)
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
	event, target, ok := lifecycle.AttemptFollowsOperation(operation.State, attempt.State)
	if !ok {
		return nil
	}
	_, _, _, err = s.Attempt(ctx, attempt.ID, event, target)
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
