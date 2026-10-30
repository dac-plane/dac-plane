package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

type migrationClaimSnapshot struct {
	uow   *store.UnitOfWork
	pass  lifecycle.MigrationPass
	items []lifecycle.MigrationItem
	item  lifecycle.MigrationItem
	at    time.Time
}

var errMigrationBudgetExhausted = errors.New("migration cannot preserve budget coverage")

func sendMigrationRequest(ctx context.Context, port contracts.Port, request contracts.Request, timeout time.Duration) (contracts.Reply, error) {
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return port.Send(sendCtx, request)
}

func migrationItemMatchesClaim(current, claimed lifecycle.MigrationItem, state lifecycle.MigrationItemState) bool {
	return !current.SourceAttempt.IsZero() && current.State == state && current.ClaimEpoch == claimed.ClaimEpoch
}

func migrationReservationAvailable(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, attempt ids.AttemptID) (bool, error) {
	reservationID, err := ids.Parse[ids.Reservation](attempt.String())
	if err != nil {
		return false, err
	}
	reservation, err := store.GetReservationForUpdate(ctx, u, tenant, reservationID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	budget, err := store.GetBudgetForUpdate(ctx, u, tenant, reservation.Budget)
	if err != nil {
		return false, err
	}
	return reservation.State == lifecycle.ReservationHeld && reservation.Amount > reservation.Consumed && budget.Consumed+budget.Reserved <= budget.Ceiling, nil
}

func (w *Worker) beginMigrationClaimSnapshot(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, source ids.AttemptID) (migrationClaimSnapshot, error) {
	var snapshot migrationClaimSnapshot
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return snapshot, err
	}
	var now time.Time
	if err := u.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		_ = u.Rollback(ctx)
		return snapshot, err
	}
	pass, items, err := store.SnapshotMigrationClaim(ctx, u, tenant, claim, w.owner, now.UTC())
	if err != nil {
		_ = u.Rollback(ctx)
		return snapshot, err
	}
	snapshot = migrationClaimSnapshot{uow: u, pass: pass, items: items, at: now.UTC()}
	if !source.IsZero() {
		for _, item := range items {
			if item.SourceAttempt == source {
				snapshot.item = item
				break
			}
		}
	}
	return snapshot, nil
}

// driveMigrationItems begins at most one source quiesce per pass. The send
// marker is committed before Send. Reply settlement is deliberately separate:
// no interrupt reply is treated as cutover until its fenced stage receipt and
// successor allocation are committed together.
func (w *Worker) driveMigrationItems(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, pass lifecycle.MigrationPass, items []lifecycle.MigrationItem) error {
	if len(items) == 0 {
		return w.finishEmptyMigration(ctx, tenant, claim, pass.ID)
	}
	for _, item := range items {
		if item.State == lifecycle.MigrationItemUnresolved || item.State == lifecycle.MigrationItemFailed {
			return w.failMigrationItems(ctx, tenant, claim, pass.ID, items)
		}
	}
	for _, item := range items {
		switch item.State {
		case lifecycle.MigrationItemQueued:
			port := w.port(tenant, pass.From, contracts.RoleRuntime)
			if port == nil {
				return nil
			}
			ready, err := w.migrationPortsReady(ctx, tenant, pass, item)
			if err != nil || !ready {
				return err
			}
			prepared, err := w.prepareSourceQuiesce(ctx, tenant, claim, item)
			if errors.Is(err, store.ErrStaleVersion) || errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return w.sendSourceQuiesce(ctx, tenant, claim, port, pass, prepared)
		case lifecycle.MigrationItemQuiescing:
			op, err := store.GetOperation(ctx, w.pool, tenant, item.QuiesceOperation)
			if err != nil {
				return err
			}
			if op.State == lifecycle.OperationAccepted {
				if err := w.allocateMigrationTarget(ctx, tenant, claim, item, pass); err != nil {
					if errors.Is(err, store.ErrStaleVersion) || errors.Is(err, store.ErrNotFound) {
						return nil
					}
					return err
				}
				return nil
			}
			port := w.port(tenant, pass.From, contracts.RoleRuntime)
			if port == nil {
				return nil
			}
			return w.sendSourceQuiesce(ctx, tenant, claim, port, pass, item)
		case lifecycle.MigrationItemStarting:
			return w.driveMigrationTarget(ctx, tenant, claim, item, pass)
		case lifecycle.MigrationItemCleaningUp:
			return w.driveMigrationCleanup(ctx, tenant, claim, item)
		case lifecycle.MigrationItemAborting:
			return w.driveMigrationAbortCleanup(ctx, tenant, claim, item, pass)
		}
	}
	return nil
}

func (w *Worker) migrationPortsReady(ctx context.Context, tenant ids.TenantID, pass lifecycle.MigrationPass, item lifecycle.MigrationItem) (bool, error) {
	if w.port(tenant, pass.To, contracts.RoleRuntime) == nil {
		return false, nil
	}
	source, err := store.GetAttempt(ctx, w.pool, tenant, item.SourceAttempt)
	if err != nil {
		return false, err
	}
	if w.port(tenant, migrationEnvironmentProvider(source, pass), contracts.RoleEnvironment) == nil {
		return false, nil
	}
	bindings, err := store.ListAttemptBindings(ctx, w.pool, tenant, item.SourceAttempt)
	if err != nil {
		return false, err
	}
	var runtimeBound, environmentBound bool
	for _, binding := range bindings {
		if binding.Kind == lifecycle.ResourceRuntimeSession && binding.Provider == pass.From {
			runtimeBound = true
		}
		if binding.Kind == lifecycle.ResourceEnvironmentResource {
			if w.port(tenant, binding.Provider, contracts.RoleEnvironment) == nil {
				return false, nil
			}
			environmentBound = true
		}
	}
	return runtimeBound && environmentBound, nil
}

func migrationEnvironmentProvider(source lifecycle.Attempt, pass lifecycle.MigrationPass) ids.ProviderID {
	if !source.EnvironmentProvider.IsZero() {
		return source.EnvironmentProvider
	}
	return pass.To
}

func (w *Worker) finishEmptyMigration(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, migration ids.MigrationID) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, ids.AttemptID{})
	if err != nil {
		return err
	}
	u, pass, items, now := snapshot.uow, snapshot.pass, snapshot.items, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if pass.ID != migration || len(items) != 0 {
		return store.ErrStaleVersion
	}
	if err := finishMigrationPass(ctx, u, tenant, pass, lifecycle.MigrationComplete, now.UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) failMigrationItems(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, migration ids.MigrationID, snapshot []lifecycle.MigrationItem) error {
	claimSnapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, ids.AttemptID{})
	if err != nil {
		return err
	}
	u, pass, items, now := claimSnapshot.uow, claimSnapshot.pass, claimSnapshot.items, claimSnapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if pass.ID != migration || len(items) != len(snapshot) {
		return store.ErrStaleVersion
	}
	for _, item := range items {
		if item.State == lifecycle.MigrationItemQueued {
			item.State, item.Reason, item.UpdatedAt = lifecycle.MigrationItemUnresolved, "blocked_by_ineligible_source", now.UTC()
			if err := store.SetMigrationItem(ctx, u, item, item.ClaimEpoch); err != nil {
				return err
			}
		}
	}
	if err := finishMigrationPass(ctx, u, tenant, pass, lifecycle.MigrationFail, now.UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func finishMigrationPass(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, pass lifecycle.MigrationPass, event lifecycle.MigrationEvent, at time.Time) error {
	tr, err := stepTransition(pass.Version, at)
	if err != nil {
		return err
	}
	if _, err := store.ApplyMigrationPass(ctx, u, tenant, pass.ID, event, tr); err != nil {
		return err
	}
	slot, err := store.GetMigrationSlot(ctx, u, tenant)
	if err != nil {
		return err
	}
	if slot.Active == pass.ID {
		slot.Active = ids.MigrationID{}
		if err := store.SetMigrationSlot(ctx, u, tenant, slot); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) prepareSourceQuiesce(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem) (lifecycle.MigrationItem, error) {
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = u.Rollback(ctx)
		}
	}()
	var now time.Time
	if err := u.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	pass, items, err := store.SnapshotMigrationClaim(ctx, u, tenant, claim, w.owner, now.UTC())
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	var current lifecycle.MigrationItem
	for _, candidate := range items {
		if candidate.SourceAttempt == item.SourceAttempt {
			current = candidate
			break
		}
	}
	if current.SourceAttempt.IsZero() || current.State != lifecycle.MigrationItemQueued {
		return lifecycle.MigrationItem{}, store.ErrStaleVersion
	}
	if _, err := executionBrief(ctx, u, tenant, current.SourceAttempt); errors.Is(err, store.ErrNotFound) {
		current.State, current.Reason, current.UpdatedAt = lifecycle.MigrationItemUnresolved, "legacy_task_missing_execution_brief", now.UTC()
		if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
			return lifecycle.MigrationItem{}, err
		}
		if err := u.Commit(ctx); err != nil {
			return lifecycle.MigrationItem{}, err
		}
		committed = true
		return lifecycle.MigrationItem{}, store.ErrStaleVersion
	} else if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if _, _, err := store.LockResultScope(ctx, u, tenant, current.SourceAttempt); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	available, err := migrationReservationAvailable(ctx, u, tenant, current.SourceAttempt)
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if !available {
		current.State, current.Reason, current.UpdatedAt = lifecycle.MigrationItemUnresolved, "source_reservation_exhausted", now.UTC()
		if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
			return lifecycle.MigrationItem{}, err
		}
		if err := u.Commit(ctx); err != nil {
			return lifecycle.MigrationItem{}, err
		}
		committed = true
		return lifecycle.MigrationItem{}, store.ErrStaleVersion
	}
	bindings, err := store.ListAttemptBindings(ctx, u, tenant, current.SourceAttempt)
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	var sourceEnvironment *lifecycle.Binding
	for i := range bindings {
		if bindings[i].Kind == lifecycle.ResourceEnvironmentResource {
			if sourceEnvironment != nil {
				return lifecycle.MigrationItem{}, store.ErrPayloadMismatch
			}
			sourceEnvironment = &bindings[i]
		}
	}
	if sourceEnvironment == nil {
		return lifecycle.MigrationItem{}, store.ErrNotFound
	}
	cleanupID, err := ids.New[ids.Operation]()
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	cleanupOp, err := lifecycle.NewProviderOperation(cleanupID, tenant, current.SourceAttempt, sourceEnvironment.Provider, now.UTC())
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if _, _, err := store.InsertOperation(ctx, u, cleanupOp); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	if _, applied, _, err := step.Attempt(ctx, current.SourceAttempt, lifecycle.AttemptMigrate, lifecycle.AttemptMigrating); err != nil || !applied {
		if err != nil {
			return lifecycle.MigrationItem{}, err
		}
		return lifecycle.MigrationItem{}, store.ErrStaleVersion
	}
	operation, err := ids.New[ids.Operation]()
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	providerOperation, err := lifecycle.NewProviderOperation(operation, tenant, current.SourceAttempt, pass.From, now.UTC())
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if _, _, err := store.InsertOperation(ctx, u, providerOperation); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if _, _, err := store.ApplyOperation(ctx, u, tenant, operation, lifecycle.OperationDispatch, lifecycle.OperationDispatched, store.Transition{ExpectedVersion: providerOperation.Version, At: now.UTC(), Actor: SystemActor(), AuditID: auditID}); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	current.QuiesceOperation, current.CleanupOperation, current.State, current.UpdatedAt = operation, cleanupOp.ID, lifecycle.MigrationItemQuiescing, now.UTC()
	current.QuiesceEpoch = current.ClaimEpoch
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if err := u.Commit(ctx); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	committed = true
	return current, nil
}

func (w *Worker) sendSourceQuiesce(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, port contracts.Port, pass lifecycle.MigrationPass, item lifecycle.MigrationItem) error {
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	stageEpoch := item.QuiesceEpoch
	if stageEpoch == 0 {
		stageEpoch = item.ClaimEpoch
	}
	body, err := json.Marshal(contracts.MigrationRequest{Migration: item.Migration, Operation: item.QuiesceOperation, SourceAttempt: item.SourceAttempt, Stage: contracts.MigrationSourceQuiesced, ClaimEpoch: stageEpoch})
	if err != nil {
		return err
	}
	var stageRequest contracts.MigrationRequest
	if err := json.Unmarshal(body, &stageRequest); err != nil {
		return err
	}
	if err := stageRequest.Validate(); err != nil {
		return err
	}
	request := contracts.Request{Operation: item.QuiesceOperation, Delivery: delivery, Tenant: item.Tenant, Attempt: item.SourceAttempt, Role: contracts.RoleRuntime, Method: contracts.MethodInterrupt, Arguments: body, Demand: contracts.DemandBackground, Deadline: time.Now().UTC().Add(w.cfg.SendTimeout)}
	if err := request.Validate(); err != nil {
		return err
	}
	reply, err := sendMigrationRequest(ctx, port, request, w.cfg.SendTimeout)
	if err != nil {
		return w.markMigrationUncertain(ctx, tenant, claim, item, item.QuiesceOperation)
	}
	receipt, err := validateMigrationReceipt(reply, request, contracts.MigrationSourceQuiesced, item.Migration, item.SourceAttempt, ids.AttemptID{}, stageEpoch, "")
	if err != nil {
		if markErr := w.markMigrationUncertain(ctx, tenant, claim, item, item.QuiesceOperation); markErr != nil {
			return markErr
		}
		return err
	}
	if len(receipt.ContextRefs) == 0 {
		return w.markMigrationUncertain(ctx, tenant, claim, item, item.QuiesceOperation)
	}
	return w.settleSourceQuiesce(ctx, tenant, claim, item, receipt)
}

func validateMigrationReceipt(reply contracts.Reply, request contracts.Request, stage contracts.MigrationStage, migration ids.MigrationID, source, target ids.AttemptID, epoch uint64, recovery contracts.MigrationRecoveryMode) (contracts.MigrationReceipt, error) {
	if err := reply.Validate(); err != nil {
		return contracts.MigrationReceipt{}, err
	}
	if reply.Operation != request.Operation || reply.Delivery != request.Delivery || reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		return contracts.MigrationReceipt{}, store.ErrPayloadMismatch
	}
	var receipt contracts.MigrationReceipt
	decoder := json.NewDecoder(bytes.NewReader(reply.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return contracts.MigrationReceipt{}, err
	}
	if err := receipt.Validate(); err != nil {
		return contracts.MigrationReceipt{}, err
	}
	if receipt.Migration != migration || receipt.Operation != request.Operation || receipt.SourceAttempt != source || receipt.TargetAttempt != target || receipt.Stage != stage || receipt.ClaimEpoch != epoch || receipt.Recovery != recovery {
		return contracts.MigrationReceipt{}, store.ErrPayloadMismatch
	}
	var sent contracts.MigrationRequest
	if stage == contracts.MigrationTargetStarted {
		var start contracts.RuntimeStart
		if err := json.Unmarshal(request.Arguments, &start); err != nil {
			return contracts.MigrationReceipt{}, err
		}
		if err := start.Validate(); err != nil {
			return contracts.MigrationReceipt{}, err
		}
		sent = *start.Recovery
	} else if err := json.Unmarshal(request.Arguments, &sent); err != nil {
		return contracts.MigrationReceipt{}, err
	}
	if (stage == contracts.MigrationTargetEnvironmentReady && receipt.EnvironmentRef == "") || (stage != contracts.MigrationTargetEnvironmentReady && receipt.EnvironmentRef != sent.EnvironmentRef) || (stage != contracts.MigrationSourceQuiesced && !slices.Equal(receipt.ContextRefs, sent.ContextRefs)) {
		return contracts.MigrationReceipt{}, store.ErrPayloadMismatch
	}
	return receipt, nil
}

func (w *Worker) markMigrationUncertain(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, operation ids.OperationID) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if current.SourceAttempt.IsZero() || current.ClaimEpoch != item.ClaimEpoch {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, operation)
	if err != nil {
		return err
	}
	if op.State == lifecycle.OperationDispatched {
		auditID, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		if _, _, err := store.ApplyOperation(ctx, u, tenant, operation, lifecycle.OperationSendUnknown, lifecycle.OperationUncertain, store.Transition{ExpectedVersion: op.Version, At: now.UTC(), Actor: SystemActor(), AuditID: auditID}); err != nil {
			return err
		}
	}
	return u.Commit(ctx)
}

func (w *Worker) settleSourceQuiesce(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemQuiescing) || current.QuiesceOperation != item.QuiesceOperation {
		return store.ErrStaleVersion
	}
	if receipt.Migration != current.Migration || receipt.Operation != current.QuiesceOperation || receipt.SourceAttempt != current.SourceAttempt || receipt.ClaimEpoch != current.QuiesceEpoch || receipt.Stage != contracts.MigrationSourceQuiesced || len(receipt.ContextRefs) == 0 {
		return store.ErrPayloadMismatch
	}
	attempt, _, err := store.LockResultScope(ctx, u, tenant, current.SourceAttempt)
	if err != nil {
		return err
	}
	if attempt.State != lifecycle.AttemptMigrating {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.QuiesceOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	current.ContextRefs = receipt.ContextRefs
	current.UpdatedAt = now.UTC()
	pass, err := store.GetMigrationPass(ctx, u, tenant, current.Migration)
	if err != nil {
		return err
	}
	if err := w.allocateMigrationTargetTx(ctx, u, tenant, current, pass, now.UTC()); err != nil {
		if errors.Is(err, errMigrationBudgetExhausted) {
			current.State, current.UpdatedAt = lifecycle.MigrationItemAborting, now.UTC()
			if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
				return err
			}
			return u.Commit(ctx)
		}
		return err
	}
	if err := u.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// allocateMigrationTarget records the successor and its reservation before
// any target-side effect. Releasing and re-reserving within this transaction
// preserves the same Task budget and never increases its ceiling.
func (w *Worker) allocateMigrationTarget(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, pass lifecycle.MigrationPass) error {
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(ctx) }()
	var now time.Time
	if err := u.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	lockedPass, items, err := store.SnapshotMigrationClaim(ctx, u, tenant, claim, w.owner, now.UTC())
	if err != nil {
		return err
	}
	var current lifecycle.MigrationItem
	for _, candidate := range items {
		if candidate.SourceAttempt == item.SourceAttempt {
			current = candidate
			break
		}
	}
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemQuiescing) {
		return store.ErrStaleVersion
	}
	if lockedPass.ID != pass.ID || lockedPass.To != pass.To {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.QuiesceOperation)
	if err != nil {
		return err
	}
	if op.State != lifecycle.OperationAccepted {
		return store.ErrStaleVersion
	}
	if err := w.allocateMigrationTargetTx(ctx, u, tenant, current, lockedPass, now.UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) allocateMigrationTargetTx(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, current lifecycle.MigrationItem, pass lifecycle.MigrationPass, at time.Time) error {
	now := at
	var err error
	source, task, err := store.LockResultScope(ctx, u, tenant, current.SourceAttempt)
	if err != nil {
		return err
	}
	if source.State != lifecycle.AttemptMigrating || task.State != lifecycle.TaskRunning {
		return store.ErrStaleVersion
	}
	if _, err := u.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0))`, "migration-attempt:"+source.Task.String()); err != nil {
		return err
	}
	var nextNumber uint32
	if err := u.QueryRow(ctx, `select coalesce(max(number),0)+1 from attempt where tenant_id=$1 and task_id=$2`, tenant.String(), source.Task.String()).Scan(&nextNumber); err != nil {
		return err
	}
	targetID, err := ids.New[ids.Attempt]()
	if err != nil {
		return err
	}
	target, err := lifecycle.NewAttempt(targetID, tenant, source.Task, nextNumber, now.UTC())
	if err != nil {
		return err
	}
	reservationID, err := ids.Parse[ids.Reservation](source.ID.String())
	if err != nil {
		return err
	}
	reservation, err := store.GetReservationForUpdate(ctx, u, tenant, reservationID)
	if err != nil {
		return err
	}
	budget, err := store.GetBudgetForUpdate(ctx, u, tenant, reservation.Budget)
	if err != nil {
		return err
	}
	if reservation.State != lifecycle.ReservationHeld || reservation.Amount <= reservation.Consumed || budget.Consumed+budget.Reserved > budget.Ceiling {
		return errMigrationBudgetExhausted
	}
	environmentProvider := migrationEnvironmentProvider(source, pass)
	envOpID, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	target.EnvironmentProvider, target.EnvironmentOperation = environmentProvider, envOpID
	if _, _, err := store.InsertAttempt(ctx, u, target); err != nil {
		return err
	}
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	tr, err := stepTransition(budget.Version, now.UTC())
	if err != nil {
		return err
	}
	if _, _, err := store.ReleaseBudget(ctx, u, tenant, budget.ID, reservationID, tr); err != nil {
		return err
	}
	budget, err = store.GetBudgetForUpdate(ctx, u, tenant, reservation.Budget)
	if err != nil {
		return err
	}
	targetReservation, err := ids.Parse[ids.Reservation](target.ID.String())
	if err != nil {
		return err
	}
	tr, err = stepTransition(budget.Version, now.UTC())
	if err != nil {
		return err
	}
	if _, _, _, err := store.ReserveBudget(ctx, u, store.ReservationRequest{Tenant: tenant, Budget: budget.ID, Reservation: targetReservation, Attempt: target.ID, Amount: reservation.Amount - reservation.Consumed}, tr); err != nil {
		return err
	}
	runtimeOpID, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	targetCleanupID, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	envOp, err := lifecycle.NewProviderOperation(envOpID, tenant, target.ID, environmentProvider, now.UTC())
	if err != nil {
		return err
	}
	runtimeOp, err := lifecycle.NewProviderOperation(runtimeOpID, tenant, target.ID, pass.To, now.UTC())
	if err != nil {
		return err
	}
	targetCleanupOp, err := lifecycle.NewProviderOperation(targetCleanupID, tenant, target.ID, environmentProvider, now.UTC())
	if err != nil {
		return err
	}
	if _, _, err := store.InsertOperation(ctx, u, envOp); err != nil {
		return err
	}
	if _, _, err := store.InsertOperation(ctx, u, runtimeOp); err != nil {
		return err
	}
	if _, _, err := store.InsertOperation(ctx, u, targetCleanupOp); err != nil {
		return err
	}
	sourceBindings, err := store.ListAttemptBindings(ctx, u, tenant, source.ID)
	if err != nil {
		return err
	}
	var sourceEnvironment *lifecycle.Binding
	var sourceRuntime *lifecycle.Binding
	for i := range sourceBindings {
		if sourceBindings[i].Kind == lifecycle.ResourceEnvironmentResource {
			if sourceEnvironment != nil {
				return store.ErrPayloadMismatch
			}
			sourceEnvironment = &sourceBindings[i]
		} else if sourceBindings[i].Kind == lifecycle.ResourceRuntimeSession {
			if sourceRuntime != nil {
				return store.ErrPayloadMismatch
			}
			sourceRuntime = &sourceBindings[i]
		}
	}
	if sourceEnvironment == nil {
		return store.ErrNotFound
	}
	if !source.EnvironmentProvider.IsZero() && sourceEnvironment.Provider != source.EnvironmentProvider {
		return store.ErrPayloadMismatch
	}
	if sourceRuntime == nil || sourceRuntime.Provider != pass.From {
		return store.ErrPayloadMismatch
	}
	step = progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	if _, applied, _, err := step.Attempt(ctx, target.ID, lifecycle.AttemptProvision, lifecycle.AttemptProvisioning); err != nil {
		return err
	} else if !applied {
		return store.ErrStaleVersion
	}
	current.TargetAttempt, current.EnvironmentOperation, current.RestartOperation, current.TargetCleanupOperation = target.ID, envOp.ID, runtimeOp.ID, targetCleanupOp.ID
	current.State, current.UpdatedAt = lifecycle.MigrationItemStarting, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	return nil
}

func stepTransition(version uint64, at time.Time) (store.Transition, error) {
	id, err := ids.New[ids.Audit]()
	if err != nil {
		return store.Transition{}, err
	}
	return store.Transition{ExpectedVersion: version, At: at, Actor: SystemActor(), AuditID: id}, nil
}

func (w *Worker) driveMigrationTarget(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, pass lifecycle.MigrationPass) error {
	if item.EnvironmentRef == "" {
		envOp, err := store.GetOperation(ctx, w.pool, tenant, item.EnvironmentOperation)
		if err != nil {
			return err
		}
		port := w.port(tenant, envOp.Provider, contracts.RoleEnvironment)
		if port == nil {
			return nil
		}
		if item.EnvironmentEpoch == 0 {
			item.EnvironmentEpoch = item.ClaimEpoch
		}
		if err := w.markMigrationStageDispatched(ctx, tenant, claim, item, item.EnvironmentOperation); err != nil {
			if errors.Is(err, errMigrationBudgetExhausted) {
				return w.failMigrationForBudget(ctx, tenant, claim, item, item.EnvironmentOperation)
			}
			return err
		}
		receipt, sent, err := w.sendMigrationStage(ctx, tenant, claim, item, port, item.EnvironmentOperation, item.TargetAttempt, contracts.RoleEnvironment, contracts.MethodProvision,
			contracts.MigrationRequest{Migration: item.Migration, Operation: item.EnvironmentOperation, SourceAttempt: item.SourceAttempt, TargetAttempt: item.TargetAttempt, Stage: contracts.MigrationTargetEnvironmentReady, ClaimEpoch: item.EnvironmentEpoch, Recovery: contracts.MigrationRestartFromArtifacts, ContextRefs: item.ContextRefs}, contracts.MigrationTargetEnvironmentReady, contracts.MigrationRestartFromArtifacts)
		if err != nil {
			return err
		}
		if !sent {
			return nil
		}
		return w.settleMigrationEnvironment(ctx, tenant, claim, item, pass, receipt)
	}
	port := w.port(tenant, pass.To, contracts.RoleRuntime)
	if port == nil {
		return nil
	}
	if item.RestartEpoch == 0 {
		item.RestartEpoch = item.ClaimEpoch
	}
	if err := w.markMigrationStageDispatched(ctx, tenant, claim, item, item.RestartOperation); err != nil {
		if errors.Is(err, errMigrationBudgetExhausted) {
			return w.failMigrationForBudget(ctx, tenant, claim, item, item.RestartOperation)
		}
		return err
	}
	receipt, sent, err := w.sendMigrationStage(ctx, tenant, claim, item, port, item.RestartOperation, item.TargetAttempt, contracts.RoleRuntime, contracts.MethodStartTurn,
		contracts.MigrationRequest{Migration: item.Migration, Operation: item.RestartOperation, SourceAttempt: item.SourceAttempt, TargetAttempt: item.TargetAttempt, Stage: contracts.MigrationTargetStarted, ClaimEpoch: item.RestartEpoch, Recovery: contracts.MigrationRestartFromArtifacts, ContextRefs: item.ContextRefs, EnvironmentRef: item.EnvironmentRef}, contracts.MigrationTargetStarted, contracts.MigrationRestartFromArtifacts)
	if err != nil {
		return err
	}
	if !sent {
		return nil
	}
	return w.settleMigrationCutover(ctx, tenant, claim, item, receipt)
}

func (w *Worker) sendMigrationStage(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, port contracts.Port, operation ids.OperationID, attempt ids.AttemptID, role contracts.Role, method contracts.Method, payload contracts.MigrationRequest, stage contracts.MigrationStage, recovery contracts.MigrationRecoveryMode) (contracts.MigrationReceipt, bool, error) {
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return contracts.MigrationReceipt{}, false, err
	}
	var wirePayload any = payload
	if method == contracts.MethodStartTurn {
		brief, briefErr := w.executionBrief(ctx, tenant, attempt)
		if briefErr != nil {
			return contracts.MigrationReceipt{}, false, briefErr
		}
		start := contracts.RuntimeStart{Brief: brief, Recovery: &payload}
		if err := start.Validate(); err != nil {
			return contracts.MigrationReceipt{}, false, err
		}
		wirePayload = start
	}
	body, err := json.Marshal(wirePayload)
	if err != nil {
		return contracts.MigrationReceipt{}, false, err
	}
	req := contracts.Request{Operation: operation, Delivery: delivery, Tenant: tenant, Attempt: attempt, Role: role, Method: method, Arguments: body, Demand: contracts.DemandBackground, Deadline: time.Now().UTC().Add(w.cfg.SendTimeout)}
	if err := req.Validate(); err != nil {
		return contracts.MigrationReceipt{}, false, err
	}
	reply, err := sendMigrationRequest(ctx, port, req, w.cfg.SendTimeout)
	if err != nil {
		if markErr := w.markMigrationUncertain(ctx, tenant, claim, item, operation); markErr != nil {
			return contracts.MigrationReceipt{}, false, markErr
		}
		return contracts.MigrationReceipt{}, false, nil
	}
	receipt, err := validateMigrationReceipt(reply, req, stage, item.Migration, item.SourceAttempt, item.TargetAttempt, payload.ClaimEpoch, recovery)
	if err != nil {
		if markErr := w.markMigrationUncertain(ctx, tenant, claim, item, operation); markErr != nil {
			return contracts.MigrationReceipt{}, false, markErr
		}
		return contracts.MigrationReceipt{}, false, err
	}
	return receipt, true, nil
}

func (w *Worker) executionBrief(ctx context.Context, tenant ids.TenantID, attemptID ids.AttemptID) (lifecycle.ExecutionBrief, error) {
	return executionBrief(ctx, w.pool, tenant, attemptID)
}

func executionBrief(ctx context.Context, q store.Querier, tenant ids.TenantID, attemptID ids.AttemptID) (lifecycle.ExecutionBrief, error) {
	attempt, err := store.GetAttempt(ctx, q, tenant, attemptID)
	if err != nil {
		return lifecycle.ExecutionBrief{}, err
	}
	task, err := store.GetTask(ctx, q, tenant, attempt.Task)
	if err != nil {
		return lifecycle.ExecutionBrief{}, err
	}
	if brief, present := task.Brief(); present {
		return brief, nil
	}
	return store.GetTaskExecutionBrief(ctx, q, task)
}

func (w *Worker) markMigrationStageDispatched(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, operation ids.OperationID) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if current.SourceAttempt.IsZero() || current.ClaimEpoch != item.ClaimEpoch || (current.State != lifecycle.MigrationItemStarting && current.State != lifecycle.MigrationItemCleaningUp && current.State != lifecycle.MigrationItemAborting) {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, operation)
	if err != nil {
		return err
	}
	if op.State == lifecycle.OperationRecorded && (operation == current.EnvironmentOperation || operation == current.RestartOperation) {
		available, err := migrationReservationAvailable(ctx, u, tenant, current.TargetAttempt)
		if err != nil {
			return err
		}
		if !available {
			return errMigrationBudgetExhausted
		}
	}
	stageEpoch := &current.QuiesceEpoch
	if operation == current.EnvironmentOperation {
		stageEpoch = &current.EnvironmentEpoch
	}
	if operation == current.RestartOperation {
		stageEpoch = &current.RestartEpoch
	}
	if operation == current.CleanupOperation {
		stageEpoch = &current.CleanupEpoch
	}
	if operation == current.TargetCleanupOperation {
		stageEpoch = &current.TargetCleanupEpoch
	}
	if *stageEpoch == 0 {
		*stageEpoch = current.ClaimEpoch
	}
	if op.State == lifecycle.OperationRecorded {
		tr, err := stepTransition(op.Version, now.UTC())
		if err != nil {
			return err
		}
		if _, _, err := store.ApplyOperation(ctx, u, tenant, operation, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr); err != nil {
			return err
		}
	} else if op.State != lifecycle.OperationDispatched && op.State != lifecycle.OperationUncertain {
		return store.ErrStaleVersion
	}
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) failMigrationForBudget(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, operation ids.OperationID) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemStarting) {
		return store.ErrStaleVersion
	}
	stage, err := store.GetOperation(ctx, u, tenant, operation)
	if err != nil {
		return err
	}
	if stage.State != lifecycle.OperationRecorded || (operation != current.EnvironmentOperation && operation != current.RestartOperation) {
		return store.ErrStaleVersion
	}
	if _, _, err := store.LockResultScope(ctx, u, tenant, current.SourceAttempt); err != nil {
		return err
	}
	if _, _, err := store.LockResultScope(ctx, u, tenant, current.TargetAttempt); err != nil {
		return err
	}
	available, err := migrationReservationAvailable(ctx, u, tenant, current.TargetAttempt)
	if err != nil {
		return err
	}
	if available {
		return store.ErrStaleVersion
	}
	current.State, current.UpdatedAt = lifecycle.MigrationItemAborting, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) settleMigrationEnvironment(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, pass lifecycle.MigrationPass, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemStarting) || current.EnvironmentEpoch != receipt.ClaimEpoch {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.EnvironmentOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	binding := lifecycle.Binding{Tenant: tenant, Attempt: current.TargetAttempt, Provider: op.Provider, Operation: current.EnvironmentOperation, Kind: lifecycle.ResourceEnvironmentResource, NativeRef: receipt.EnvironmentRef, BoundAt: now.UTC()}
	if err := insertMigrationBinding(ctx, u, tenant, binding); err != nil {
		return err
	}
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	if _, applied, _, err := step.Attempt(ctx, current.TargetAttempt, lifecycle.AttemptStart, lifecycle.AttemptStarting); err != nil {
		return err
	} else if !applied {
		return store.ErrStaleVersion
	}
	current.EnvironmentRef, current.UpdatedAt = receipt.EnvironmentRef, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	return u.Commit(ctx)
}

// acceptMigrationOperation settles a normal send or an uncertain exact replay
// using the operation's stable idempotency identity.
func acceptMigrationOperation(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, op lifecycle.ProviderOperation, at time.Time) error {
	var event lifecycle.OperationEvent
	switch op.State {
	case lifecycle.OperationDispatched:
		event = lifecycle.OperationSendAccepted
	case lifecycle.OperationUncertain:
		event = lifecycle.OperationReconcile
	default:
		return store.ErrStaleVersion
	}
	tr, err := stepTransition(op.Version, at)
	if err != nil {
		return err
	}
	_, _, err = store.ApplyOperation(ctx, u, tenant, op.ID, event, lifecycle.OperationAccepted, tr)
	return err
}

func insertMigrationBinding(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, binding lifecycle.Binding) error {
	bindings, err := store.ListBindings(ctx, u, tenant, binding.Provider, binding.Kind, binding.NativeRef)
	if err != nil {
		return err
	}
	if err := lifecycle.CheckBinding(bindings, binding); err != nil {
		return err
	}
	_, _, err = store.InsertBinding(ctx, u, binding)
	return err
}

func (w *Worker) settleMigrationCutover(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemStarting) || current.RestartEpoch != receipt.ClaimEpoch || current.EnvironmentRef != receipt.EnvironmentRef {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.RestartOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	runtimeBinding := lifecycle.Binding{Tenant: tenant, Attempt: current.TargetAttempt, Provider: op.Provider, Operation: current.RestartOperation, Kind: lifecycle.ResourceRuntimeSession, NativeRef: receipt.SessionRef, BoundAt: now.UTC()}
	if err := insertMigrationBinding(ctx, u, tenant, runtimeBinding); err != nil {
		return err
	}
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	if _, applied, _, err := step.Attempt(ctx, current.TargetAttempt, lifecycle.AttemptRuntimeStarted, lifecycle.AttemptExecuting); err != nil {
		return err
	} else if !applied {
		return store.ErrStaleVersion
	}
	if _, applied, _, err := step.Attempt(ctx, current.SourceAttempt, lifecycle.AttemptMigrationSuperseded, lifecycle.AttemptMigrated); err != nil {
		return err
	} else if !applied {
		return store.ErrStaleVersion
	}
	current.State, current.UpdatedAt = lifecycle.MigrationItemCleaningUp, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) driveMigrationAbortCleanup(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, pass lifecycle.MigrationPass) error {
	if !item.TargetAttempt.IsZero() && item.EnvironmentRef != "" {
		op, err := store.GetOperation(ctx, w.pool, tenant, item.TargetCleanupOperation)
		if err != nil {
			return err
		}
		if op.State != lifecycle.OperationAccepted {
			port := w.port(tenant, op.Provider, contracts.RoleEnvironment)
			if port == nil {
				return nil
			}
			if item.TargetCleanupEpoch == 0 {
				item.TargetCleanupEpoch = item.ClaimEpoch
			}
			if err := w.markMigrationStageDispatched(ctx, tenant, claim, item, item.TargetCleanupOperation); err != nil {
				return err
			}
			receipt, sent, err := w.sendMigrationStage(ctx, tenant, claim, item, port, item.TargetCleanupOperation, item.TargetAttempt, contracts.RoleEnvironment, contracts.MethodStop,
				contracts.MigrationRequest{Migration: item.Migration, Operation: item.TargetCleanupOperation, SourceAttempt: item.SourceAttempt, TargetAttempt: item.TargetAttempt, Stage: contracts.MigrationTargetEnvironmentCleaned, ClaimEpoch: item.TargetCleanupEpoch, EnvironmentRef: item.EnvironmentRef}, contracts.MigrationTargetEnvironmentCleaned, "")
			if err != nil || !sent {
				return err
			}
			return w.settleMigrationAbortTargetCleanup(ctx, tenant, claim, item, receipt)
		}
	}

	sourceOperation, err := store.GetOperation(ctx, w.pool, tenant, item.CleanupOperation)
	if err != nil {
		return err
	}
	if sourceOperation.State != lifecycle.OperationAccepted {
		bindings, err := store.ListAttemptBindings(ctx, w.pool, tenant, item.SourceAttempt)
		if err != nil {
			return err
		}
		var sourceEnvironment *lifecycle.Binding
		for i := range bindings {
			if bindings[i].Kind == lifecycle.ResourceEnvironmentResource && bindings[i].Provider == sourceOperation.Provider {
				if sourceEnvironment != nil {
					return store.ErrPayloadMismatch
				}
				sourceEnvironment = &bindings[i]
			}
		}
		if sourceEnvironment == nil {
			return store.ErrNotFound
		}
		port := w.port(tenant, sourceOperation.Provider, contracts.RoleEnvironment)
		if port == nil {
			return nil
		}
		if item.CleanupEpoch == 0 {
			item.CleanupEpoch = item.ClaimEpoch
		}
		if err := w.markMigrationStageDispatched(ctx, tenant, claim, item, item.CleanupOperation); err != nil {
			return err
		}
		receipt, sent, err := w.sendMigrationStage(ctx, tenant, claim, item, port, item.CleanupOperation, item.SourceAttempt, contracts.RoleEnvironment, contracts.MethodStop,
			contracts.MigrationRequest{Migration: item.Migration, Operation: item.CleanupOperation, SourceAttempt: item.SourceAttempt, TargetAttempt: item.TargetAttempt, Stage: contracts.MigrationSourceCleaned, ClaimEpoch: item.CleanupEpoch, EnvironmentRef: sourceEnvironment.NativeRef}, contracts.MigrationSourceCleaned, "")
		if err != nil || !sent {
			return err
		}
		return w.settleMigrationAbortSourceCleanup(ctx, tenant, claim, item, receipt)
	}
	return w.finishMigrationAbort(ctx, tenant, claim, item)
}

func (w *Worker) settleMigrationAbortTargetCleanup(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemAborting) || current.TargetCleanupEpoch != receipt.ClaimEpoch || receipt.Stage != contracts.MigrationTargetEnvironmentCleaned || receipt.EnvironmentRef != current.EnvironmentRef {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.TargetCleanupOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) settleMigrationAbortSourceCleanup(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, now := snapshot.uow, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemAborting) || current.CleanupEpoch != receipt.ClaimEpoch || receipt.Stage != contracts.MigrationSourceCleaned {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.CleanupOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	if err := u.Commit(ctx); err != nil {
		return err
	}
	return w.finishMigrationAbort(ctx, tenant, claim, item)
}

func (w *Worker) finishMigrationAbort(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, current, pass, now := snapshot.uow, snapshot.item, snapshot.pass, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemAborting) {
		return store.ErrStaleVersion
	}
	sourceCleanup, err := store.GetOperation(ctx, u, tenant, current.CleanupOperation)
	if err != nil || sourceCleanup.State != lifecycle.OperationAccepted {
		if err != nil {
			return err
		}
		return store.ErrStaleVersion
	}
	if !current.TargetAttempt.IsZero() && current.EnvironmentRef != "" {
		targetCleanup, err := store.GetOperation(ctx, u, tenant, current.TargetCleanupOperation)
		if err != nil || targetCleanup.State != lifecycle.OperationAccepted {
			if err != nil {
				return err
			}
			return store.ErrStaleVersion
		}
	}
	source, _, err := store.LockResultScope(ctx, u, tenant, current.SourceAttempt)
	if err != nil {
		return err
	}
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now.UTC()}
	if source.State != lifecycle.AttemptFailed {
		if _, applied, _, err := step.Attempt(ctx, source.ID, lifecycle.AttemptFail, lifecycle.AttemptFailed); err != nil {
			return err
		} else if !applied {
			return store.ErrStaleVersion
		}
	}
	if !current.TargetAttempt.IsZero() {
		target, _, err := store.LockResultScope(ctx, u, tenant, current.TargetAttempt)
		if err != nil {
			return err
		}
		if target.State != lifecycle.AttemptFailed {
			if _, applied, _, err := step.Attempt(ctx, target.ID, lifecycle.AttemptFail, lifecycle.AttemptFailed); err != nil {
				return err
			} else if !applied {
				return store.ErrStaleVersion
			}
		}
		reservationID, err := ids.Parse[ids.Reservation](current.TargetAttempt.String())
		if err != nil {
			return err
		}
		reservation, err := store.GetReservationForUpdate(ctx, u, tenant, reservationID)
		if err != nil {
			return err
		}
		if reservation.State == lifecycle.ReservationHeld {
			budget, err := store.GetBudgetForUpdate(ctx, u, tenant, reservation.Budget)
			if err != nil {
				return err
			}
			tr, err := stepTransition(budget.Version, now.UTC())
			if err != nil {
				return err
			}
			if _, _, err := store.ReleaseBudget(ctx, u, tenant, budget.ID, reservation.ID, tr); err != nil {
				return err
			}
		}
	}
	current.State, current.UpdatedAt = lifecycle.MigrationItemFailed, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	if err := finishMigrationPass(ctx, u, tenant, pass, lifecycle.MigrationFail, now.UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func (w *Worker) driveMigrationCleanup(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem) error {
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	var op lifecycle.ProviderOperation
	var sourceEnv lifecycle.Binding
	if err := u.QueryRow(ctx, `select clock_timestamp()`).Scan(new(time.Time)); err != nil {
		_ = u.Rollback(ctx)
		return err
	}
	op, err = store.GetOperation(ctx, u, tenant, item.CleanupOperation)
	if err != nil {
		_ = u.Rollback(ctx)
		return err
	}
	bindings, err := store.ListAttemptBindings(ctx, u, tenant, item.SourceAttempt)
	_ = u.Rollback(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, b := range bindings {
		if b.Kind == lifecycle.ResourceEnvironmentResource && b.Provider == op.Provider {
			sourceEnv, found = b, true
			break
		}
	}
	if !found {
		return store.ErrNotFound
	}
	port := w.port(tenant, op.Provider, contracts.RoleEnvironment)
	if port == nil {
		return nil
	}
	if item.CleanupEpoch == 0 {
		item.CleanupEpoch = item.ClaimEpoch
	}
	if err := w.markMigrationStageDispatched(ctx, tenant, claim, item, item.CleanupOperation); err != nil {
		return err
	}
	receipt, sent, err := w.sendMigrationStage(ctx, tenant, claim, item, port, item.CleanupOperation, item.SourceAttempt, contracts.RoleEnvironment, contracts.MethodStop,
		contracts.MigrationRequest{Migration: item.Migration, Operation: item.CleanupOperation, SourceAttempt: item.SourceAttempt, TargetAttempt: item.TargetAttempt, Stage: contracts.MigrationSourceCleaned, ClaimEpoch: item.CleanupEpoch, EnvironmentRef: sourceEnv.NativeRef}, contracts.MigrationSourceCleaned, "")
	if err != nil {
		return err
	}
	if !sent {
		return nil
	}
	return w.settleMigrationCleanup(ctx, tenant, claim, item, receipt)
}

func (w *Worker) settleMigrationCleanup(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, item lifecycle.MigrationItem, receipt contracts.MigrationReceipt) error {
	snapshot, err := w.beginMigrationClaimSnapshot(ctx, tenant, claim, item.SourceAttempt)
	if err != nil {
		return err
	}
	u, pass, items, current, now := snapshot.uow, snapshot.pass, snapshot.items, snapshot.item, snapshot.at
	defer func() { _ = u.Rollback(ctx) }()
	if !migrationItemMatchesClaim(current, item, lifecycle.MigrationItemCleaningUp) || current.CleanupEpoch != receipt.ClaimEpoch {
		return store.ErrStaleVersion
	}
	op, err := store.GetOperation(ctx, u, tenant, current.CleanupOperation)
	if err != nil {
		return err
	}
	if err := acceptMigrationOperation(ctx, u, tenant, op, now.UTC()); err != nil {
		return err
	}
	current.State, current.UpdatedAt = lifecycle.MigrationItemCompleted, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return err
	}
	allComplete := true
	for _, candidate := range items {
		if candidate.SourceAttempt != current.SourceAttempt && candidate.State != lifecycle.MigrationItemCompleted {
			allComplete = false
		}
	}
	if allComplete {
		tr, err := stepTransition(pass.Version, now.UTC())
		if err != nil {
			return err
		}
		if _, err := store.ApplyMigrationPass(ctx, u, tenant, pass.ID, lifecycle.MigrationComplete, tr); err != nil {
			return err
		}
		slot, err := store.GetMigrationSlot(ctx, u, tenant)
		if err != nil {
			return err
		}
		if slot.Active == pass.ID {
			slot.Active = ids.MigrationID{}
			if err := store.SetMigrationSlot(ctx, u, tenant, slot); err != nil {
				return err
			}
		}
	}
	_ = receipt
	return u.Commit(ctx)
}
