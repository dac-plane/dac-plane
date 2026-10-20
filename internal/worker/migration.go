package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

// driveMigrationItems begins at most one source quiesce per pass. The send
// marker is committed before Send. Reply settlement is deliberately separate:
// no interrupt reply is treated as cutover until its fenced stage receipt and
// successor allocation are committed together.
func (w *Worker) driveMigrationItems(ctx context.Context, tenant ids.TenantID, claim ids.ClaimID, pass lifecycle.MigrationPass, items []lifecycle.MigrationItem) error {
	for _, item := range items {
		if item.State != lifecycle.MigrationItemQueued {
			continue
		}
		port := w.ports[pass.From]
		if port == nil || port.Role() != contracts.RoleRuntime {
			return nil
		}
		prepared, err := w.prepareSourceQuiesce(ctx, tenant, claim, item)
		if errors.Is(err, store.ErrStaleVersion) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return w.sendSourceQuiesce(ctx, port, pass, prepared)
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
	_, items, err := store.SnapshotMigrationClaim(ctx, u, tenant, claim, w.owner, now.UTC())
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
	current.QuiesceOperation, current.State, current.UpdatedAt = operation, lifecycle.MigrationItemQuiescing, now.UTC()
	if err := store.SetMigrationItem(ctx, u, current, current.ClaimEpoch); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	if err := u.Commit(ctx); err != nil {
		return lifecycle.MigrationItem{}, err
	}
	committed = true
	return current, nil
}

func (w *Worker) sendSourceQuiesce(ctx context.Context, port contracts.Port, pass lifecycle.MigrationPass, item lifecycle.MigrationItem) error {
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	body, err := json.Marshal(contracts.MigrationRequest{Migration: item.Migration, SourceAttempt: item.SourceAttempt, Stage: contracts.MigrationSourceQuiesced, ClaimEpoch: item.ClaimEpoch})
	if err != nil {
		return err
	}
	request := contracts.Request{Operation: item.QuiesceOperation, Delivery: delivery, Tenant: item.Tenant, Attempt: item.SourceAttempt, Role: contracts.RoleRuntime, Method: contracts.MethodInterrupt, Arguments: body, Demand: contracts.DemandBackground, Deadline: time.Now().UTC().Add(w.cfg.SendTimeout)}
	if err := request.Validate(); err != nil {
		return err
	}
	_, err = port.Send(ctx, request)
	return err
}
