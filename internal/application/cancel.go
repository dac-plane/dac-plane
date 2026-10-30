// Package application The cancellation command stops delegated work: it persists the stop intent
// before any provider is told, cancels the Attempt's open native waits and
// revokes its unused grants, and converges only through the frozen stop
// observations — never by assuming the provider complied.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

const (
	commandCancelTask = "cancel_task"
	// cancelReason is the command boundary's single stop reason: the user
	// asking for the stop. Observation reasons belong to turn-end handling,
	// not the command boundary.
	cancelReason = "user_stop"
	// interruptHorizon bounds the interrupt call the cancel row dispatches;
	// an interruption observes through lifecycle stops, not this deadline.
	interruptHorizon = 24 * time.Hour
)

// CancelTask cancels a Task before its work finished. ExpectedVersion is the
// Task version the cancellation was decided on: a stale version refuses with
// evidence instead of racing a concurrent state change.
type CancelTask struct {
	Command
	// Task is the task in scope.
	Task ids.TaskID
	// At is the cancellation time.
	At time.Time
}

func (c CancelTask) validate() error {
	if err := c.Command.validate(false); err != nil {
		return err
	}
	if c.At.IsZero() {
		return fmt.Errorf("%w: a cancellation time is required", ErrInvalidCommand)
	}
	if c.Task.IsZero() {
		return fmt.Errorf("%w: a task is required", ErrInvalidCommand)
	}
	return nil
}

// CancelView is the result of a cancellation: the task, its attempt and their
// states after the command, and the stop reason recorded against it.
type CancelView struct {
	Nonce        string
	TaskID       string
	State        string
	AttemptID    string
	AttemptState string
	Reason       string
}

type cancelPayload struct {
	Task            string `json:"task"`
	Reason          string `json:"reason"`
	ExpectedVersion uint64 `json:"expected_version"`
}

// Cancel runs the cancellation. A replay with the same nonce and payload
// returns the stored result; a retry with a different nonce refuses on the
// Task version once the first cancel moved it.
func Cancel(ctx context.Context, pool *pgxpool.Pool, cmd CancelTask) (CancelView, error) {
	if err := cmd.validate(); err != nil {
		return CancelView{}, err
	}
	digest, err := fingerprint(commandCancelTask, cancelPayload{
		Task: cmd.Task.String(), Reason: cancelReason, ExpectedVersion: cmd.ExpectedVersion,
	})
	if err != nil {
		return CancelView{}, err
	}
	result, err := execute(ctx, pool, cmd.Command, commandCancelTask, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		return cancel(ctx, u, cmd)
	})
	if err != nil {
		return CancelView{}, err
	}
	var view CancelView
	if err := json.Unmarshal(result, &view); err != nil {
		return CancelView{}, fmt.Errorf("cancel record: %w", err)
	}
	view.Nonce = cmd.Nonce
	return view, nil
}

// cancel moves the task into cancellation and, when work may be live, records
// the intent on the attempt: one commit carries the task move, the attempt's
// cancellation request together with its interrupt dispatch, the cancelled
// native waits and the revoked grants. The frozen machines own every ordering
// the boundary relies on; their refusals are the command's refusals.
func cancel(ctx context.Context, u *unit, cmd CancelTask) (json.RawMessage, error) {
	tenant := cmd.Actor.Principal().Tenant
	// The task is loaded in the actor's own tenant only: another tenant's
	// task reads as absent.
	task, err := store.GetTask(ctx, u.uow, tenant, cmd.Task)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if task.Owner != cmd.Actor.Principal().ID {
		tr, err := u.transition(task.Version, nil)
		if err != nil {
			return nil, err
		}
		return nil, &lifecycle.TransitionError{Code: lifecycle.RejectUnauthorizedActor, Change: lifecycle.Change{
			Tenant: tenant, Record: lifecycle.RecordTask, RecordID: task.ID.Untyped(),
			Event: string(lifecycle.TaskCancel), From: string(task.State), Version: tr.ExpectedVersion,
		}}
	}
	attempt, err := store.GetAttemptByTask(ctx, u.uow, tenant, task.ID)
	if err != nil {
		return nil, err
	}
	attempt, task, err = store.LockResultScope(ctx, u.uow, tenant, attempt.ID)
	if err != nil {
		return nil, err
	}
	// The expected version is the view the cancellation was decided on: a
	// task a concurrent writer already moved past it is this command's
	// version conflict, while a task already cancelling or taken is refused
	// by its own machine with evidence — both recorded through the ledger's
	// refusal replay.
	tr, err := u.transition(cmd.ExpectedVersion, nil)
	if err != nil {
		return nil, err
	}
	cancelled, replayed, err := store.ApplyTask(ctx, u.uow, tenant, task.ID, lifecycle.TaskCancel, "", tr)
	if err != nil {
		if replayed || errors.Is(err, store.ErrStaleVersion) {
			// Another writer moved the task between this read and its
			// write: this cancel decided on a stale view.
			return nil, staleRefusal(tenant, lifecycle.RecordTask, task.ID.Untyped(), string(lifecycle.TaskCancel), string(task.State), tr.ExpectedVersion)
		}
		return nil, err
	}
	view := CancelView{
		TaskID: task.ID.String(), State: string(cancelled.State),
		AttemptID: attempt.ID.String(), Reason: cancelReason,
	}

	if cancelled.State == lifecycle.TaskCancelled {
		switch {
		case attempt.State == lifecycle.AttemptAllocated:
			// Nothing was started, so cancelling needs no stop observations:
			// request_cancel terminates the allocated Attempt and releases its
			// reservation in this commit.
			step := progress.Step{UOW: u.uow, Tenant: tenant, Actor: u.actor, At: u.at}
			next, _, refusal, err := step.Attempt(ctx, attempt.ID, lifecycle.AttemptRequestCancel, "")
			if refusal != nil {
				return nil, refusal
			}
			if err != nil {
				return nil, err
			}
			view.AttemptState = string(next.State)
		case slices.Contains(lifecycle.AttemptTerminalStates(), attempt.State):
			// Review-time cancellation changes the Task disposition only. Its
			// Attempt already ended and must not be rewritten as terminated;
			// any unused authority from its earlier work is still revoked.
			if err := cancelOpenAttention(ctx, u, tenant, attempt.ID); err != nil {
				return nil, err
			}
			if err := revokeOpenGrants(ctx, u, tenant, attempt.ID); err != nil {
				return nil, err
			}
			view.AttemptState = string(attempt.State)
		default:
			return nil, fmt.Errorf("%w: direct task cancellation requires an allocated or terminal attempt", ErrInvalidCommand)
		}
		return json.Marshal(view)
	}

	// Work may be live: record the intent on the attempt before anything is
	// told. The attempt was read with the task, so its CAS is exact; a
	// concurrent writer moving it first is this command's refusal.
	operation, operationErr := store.GetOperationByAttempt(ctx, u.uow, tenant, attempt.ID)
	runtimeMayHaveStarted := false
	if operationErr == nil {
		row, rowErr := store.GetOutboxRow(ctx, u.uow, tenant, operation.ID.Untyped())
		if rowErr == nil {
			runtimeMayHaveStarted = row.Attempts > 0 || row.Status == "processing" || row.LastError == "send_started"
		} else if !errors.Is(rowErr, store.ErrNotFound) {
			return nil, rowErr
		}
	} else if !errors.Is(operationErr, store.ErrNotFound) {
		return nil, operationErr
	} else if attempt.EnvironmentProvider.IsZero() {
		return nil, fmt.Errorf("%w: the attempt has no dispatch to interrupt", ErrInvalidCommand)
	}
	var cancellationOutbox *store.OutboxPayload
	if runtimeMayHaveStarted {
		body, err := json.Marshal(map[string]any{
			"operation": operation.ID.String(), "role": string(contracts.RoleRuntime), "method": string(contracts.MethodInterrupt),
			"deadline": u.at.Add(interruptHorizon).Format(time.RFC3339Nano), "cancel_reason": cancelReason,
		})
		if err != nil {
			return nil, err
		}
		cancellationOutbox = &store.OutboxPayload{ID: attempt.ID.Untyped(), Attempt: attempt.ID, Demand: contracts.DemandPerson, Body: body}
	}
	tr, err = u.transition(attempt.Version, cancellationOutbox)
	if err != nil {
		return nil, err
	}
	requested, replayed, err := store.ApplyAttempt(ctx, u.uow, tenant, attempt.ID, lifecycle.AttemptRequestCancel, "", tr)
	if err != nil {
		if replayed || errors.Is(err, store.ErrStaleVersion) {
			return nil, staleRefusal(tenant, lifecycle.RecordAttempt, attempt.ID.Untyped(), string(lifecycle.AttemptRequestCancel), string(attempt.State), tr.ExpectedVersion)
		}
		return nil, err
	}
	if !runtimeMayHaveStarted {
		step := progress.Step{UOW: u.uow, Tenant: tenant, Actor: u.actor, At: u.at}
		next, applied, refusal, err := step.Attempt(ctx, attempt.ID, lifecycle.AttemptRuntimeStopped, "")
		if err != nil {
			return nil, err
		}
		if !applied && refusal != nil {
			return nil, refusal
		}
		requested = next
	}
	if !attempt.EnvironmentProvider.IsZero() {
		if err := enqueueEnvironmentStop(ctx, u, tenant, attempt); err != nil {
			return nil, err
		}
	}
	// A latest stored absent or hibernated observation remains valid stop
	// evidence even when it arrived before the cancellation command. Record
	// it now so the provider need not invent a new resource sequence merely
	// to repeat an unchanged environment state.
	environmentStopped, err := store.EnvironmentStopped(ctx, u.uow, tenant, attempt.ID)
	if err != nil {
		return nil, err
	}
	if environmentStopped {
		step := progress.Step{UOW: u.uow, Tenant: tenant, Actor: u.actor, At: u.at}
		next, applied, refusal, err := step.Attempt(ctx, attempt.ID, lifecycle.AttemptEnvironmentStopped, "")
		if err != nil {
			return nil, err
		}
		if !applied && refusal != nil {
			return nil, refusal
		}
		requested = next
	}
	view.AttemptState = string(requested.State)

	// Cancelling invalidates the open native waits and the unused grants.
	if err := cancelOpenAttention(ctx, u, tenant, attempt.ID); err != nil {
		return nil, err
	}
	if err := revokeOpenGrants(ctx, u, tenant, attempt.ID); err != nil {
		return nil, err
	}
	return json.Marshal(view)
}

func enqueueEnvironmentStop(ctx context.Context, u *unit, tenant ids.TenantID, attempt lifecycle.Attempt) error {
	operationID, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	operation, err := lifecycle.NewProviderOperation(operationID, tenant, attempt.ID, attempt.EnvironmentProvider, u.at)
	if err != nil {
		return err
	}
	if _, _, err := store.InsertOperation(ctx, u.uow, operation); err != nil {
		return err
	}
	arguments, err := json.Marshal(map[string]string{"provision_operation_id": attempt.EnvironmentOperation.String()})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"operation": operation.ID.String(), "role": contracts.RoleEnvironment, "method": contracts.MethodStop,
		"arguments": json.RawMessage(arguments), "deadline": u.at.Add(interruptHorizon).Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	tr, err := u.transition(operation.Version, &store.OutboxPayload{ID: operation.ID.Untyped(), Attempt: attempt.ID, Demand: contracts.DemandPerson, Body: body})
	if err != nil {
		return err
	}
	_, _, err = store.ApplyOperation(ctx, u.uow, tenant, operation.ID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr)
	return err
}

// cancelOpenAttention cancels every still-open native wait of the attempt in
// the caller's transaction. Each request applies against its fresh locked
// read — the machine keeps cancel legal from every unconsumed state — and a
// wait a racing writer resolved first needs nothing, so nothing is written.
func cancelOpenAttention(ctx context.Context, u *unit, tenant ids.TenantID, attempt ids.AttemptID) error {
	requests, err := store.ListOpenAttentionByAttempt(ctx, u.uow, tenant, attempt)
	if err != nil {
		return err
	}
	open := lifecycle.UnconsumedAttentionStates()
	for _, request := range requests {
		for try := 0; try < 2; try++ {
			current, err := store.GetAttentionForUpdate(ctx, u.uow, tenant, request.ID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					break
				}
				return err
			}
			if !slices.Contains(open, current.State) {
				break // resolved legitimately meanwhile; writing nothing is correct
			}
			tr, err := u.transition(current.Version, nil)
			if err != nil {
				return err
			}
			_, replayed, err := store.ApplyAttention(ctx, u.uow, tenant, request.ID, lifecycle.AttentionCancel, tr)
			if err == nil || replayed {
				break
			}
			if errors.Is(err, store.ErrStaleVersion) {
				continue // re-read; the version moved under the read
			}
			var refusal *lifecycle.TransitionError
			if errors.As(err, &refusal) {
				break // a concurrent writer closed this wait; its evidence is recorded
			}
			return err
		}
	}
	return nil
}

// revokeOpenGrants revokes every grant of the attempt that still authorizes:
// cancelling invalidates unused grants before they can be consumed.
func revokeOpenGrants(ctx context.Context, u *unit, tenant ids.TenantID, attempt ids.AttemptID) error {
	grants, err := store.ListGrantsByAttempt(ctx, u.uow, tenant, attempt)
	if err != nil {
		return err
	}
	for _, grant := range grants {
		if _, _, err := store.RevokeGrant(ctx, u.uow, tenant, grant.ID, u.at); err != nil {
			if errors.Is(err, store.ErrStaleVersion) {
				continue // a racing writer retired or revoked it first
			}
			return err
		}
	}
	return nil
}

// staleRefusal is the evidence of a command that acted on a stale view: the
// record stood where this command read it, and a concurrent writer moved past
// that version first.
func staleRefusal(tenant ids.TenantID, record lifecycle.RecordKind, recordID ids.AnyID, event, from string, version uint64) *lifecycle.TransitionError {
	return &lifecycle.TransitionError{
		Code: lifecycle.RejectVersionConflict,
		Change: lifecycle.Change{
			Tenant: tenant, Record: record, RecordID: recordID,
			Event: event, From: from, Version: version,
		},
	}
}
