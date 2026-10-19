package application

import (
	"context"
	"errors"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

func (a entryApplier) resultProposed(ctx context.Context, entry contracts.Entry, p contracts.ResultProposed) (entryOutcome, error) {
	h, err := store.GetHarnessForUpdate(ctx, a.uow, a.event.Tenant, a.event.Provider)
	if err != nil {
		return 0, err
	}
	if h.Mode != lifecycle.HarnessLive {
		return a.refuse(ctx, entry, lifecycle.RejectUnauthorizedActor)
	}
	attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
	if err != nil {
		return 0, err
	}
	current, err := store.GetAttemptByTask(ctx, a.uow, a.event.Tenant, attempt.Task)
	if err != nil {
		return 0, err
	}
	if current.ID != attempt.ID || (attempt.State != lifecycle.AttemptExecuting && attempt.State != lifecycle.AttemptFinalizing) {
		return a.refuse(ctx, entry, lifecycle.RejectInvalidTransition)
	}
	if ok, err := a.boundSession(ctx, p.SessionRef); err != nil {
		return 0, err
	} else if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	if ok, err := a.bind(ctx, lifecycle.ResourceRuntimeTurn, p.TurnRef, p.SessionRef); err != nil {
		return 0, err
	} else if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	if _, _, err = store.InsertResultProposal(ctx, a.uow, store.ResultProposal{Tenant: a.event.Tenant, Task: attempt.Task, Attempt: attempt.ID, Provider: a.event.Provider, Operation: a.event.Operation, Payload: p, CreatedAt: a.step.At}); errors.Is(err, store.ErrPayloadMismatch) {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	} else if err != nil {
		return 0, err
	}
	return entryApplied, a.activateResult(ctx)
}

// activateResult joins two independently replayable observations under the
// ingestion scope locks. A successful turn without a proposal stays executing.
func (a entryApplier) activateResult(ctx context.Context) error {
	p, err := store.GetResultProposal(ctx, a.uow, a.event.Tenant, a.event.Attempt)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	h, err := store.GetHarnessForUpdate(ctx, a.uow, p.Tenant, p.Provider)
	if err != nil {
		return err
	}
	if h.Mode != lifecycle.HarnessLive {
		return nil
	}
	end, err := store.GetTurnEnd(ctx, a.uow, p.Tenant, p.Provider, p.Payload.SessionRef, p.Payload.TurnRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if end.Outcome != lifecycle.TurnCompleted {
		return nil
	}
	attempt, task, err := store.LockResultScope(ctx, a.uow, p.Tenant, p.Attempt)
	if err != nil {
		return err
	}
	if attempt.State != lifecycle.AttemptExecuting || task.State != lifecycle.TaskRunning {
		return nil
	}
	current, err := store.GetAttemptByTask(ctx, a.uow, p.Tenant, p.Task)
	if err != nil {
		return err
	}
	if current.ID != p.Attempt {
		return nil
	}
	if _, applied, _, err := a.step.Attempt(ctx, p.Attempt, lifecycle.AttemptFinalize, ""); err != nil {
		return err
	} else if !applied {
		return nil
	}
	if _, err = a.step.Task(ctx, p.Task, lifecycle.TaskProposeResult); err != nil {
		return err
	}
	return store.EnqueueArtifacts(ctx, a.uow, p, a.step.At)
}
