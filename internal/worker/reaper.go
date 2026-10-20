package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// ReapClaims takes over lapsed maintenance claims and renews owned ones
// nearing expiry. Takeover bumps the epoch, fencing the stale holder whose
// renewals and completions the claim predicate then rejects.
func (w *Worker) ReapClaims(ctx context.Context) error {
	tenants, err := store.ListTenants(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.reapTenantClaims(ctx, tenant); err != nil {
			w.logger.Error("claim reaper failed", "tenant", tenant.String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) reapTenantClaims(ctx context.Context, tenant ids.TenantID) error {
	claims, err := store.OpenClaims(ctx, w.pool, tenant)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return err
		}
		var now time.Time
		if err := w.pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if claim.State == lifecycle.ClaimScheduled {
			if err := w.takeClaim(ctx, tenant, claim.ID); err != nil {
				w.logger.Error("claim acquisition failed", "claim", claim.ID.String(), "error", err)
			}
			continue
		}
		if now.Before(claim.ExpiresAt) {
			if claim.Claimant != w.owner || claim.ExpiresAt.Sub(now) > w.cfg.ClaimTTL/2 {
				continue
			}
			if err := w.renewClaim(ctx, tenant, claim.ID); err != nil {
				w.logger.Error("claim renew failed", "claim", claim.ID.String(), "error", err)
			}
			continue
		}
		if err := w.takeClaim(ctx, tenant, claim.ID); err != nil {
			w.logger.Error("claim takeover failed", "claim", claim.ID.String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) takeClaim(ctx context.Context, tenant ids.TenantID, id ids.ClaimID) error {
	uow, now, auditID, ready, err := w.beginClaimMaintenance(ctx, tenant, id)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if _, err := store.TakeClaim(ctx, uow, tenant, id, w.owner, w.cfg.ClaimTTL, store.Transition{
		At: now, Actor: SystemActor(), AuditID: auditID,
	}); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if err := w.snapshotMigrationClaim(ctx, tenant, id); err != nil {
		return err
	}
	w.stats.Reaped.Add(1)
	return nil
}

func (w *Worker) renewClaim(ctx context.Context, tenant ids.TenantID, id ids.ClaimID) error {
	uow, now, auditID, ready, err := w.beginClaimMaintenance(ctx, tenant, id)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	owner, ttl := w.owner, w.cfg.ClaimTTL
	if _, err := store.HoldClaim(ctx, uow, tenant, id, func(claim lifecycle.MaintenanceClaim) (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
		return claim.Renew(owner, claim.Epoch, ttl, now)
	}, store.Transition{At: now, Actor: SystemActor(), AuditID: auditID}); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return w.snapshotMigrationClaim(ctx, tenant, id)
}

func (w *Worker) beginClaimMaintenance(ctx context.Context, tenant ids.TenantID, id ids.ClaimID) (*store.UnitOfWork, time.Time, ids.AuditID, bool, error) {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return nil, time.Time{}, ids.AuditID{}, false, err
	}
	ready, err := store.PrepareMaintenanceClaim(ctx, uow, tenant, id)
	if err != nil || !ready {
		_ = uow.Rollback(ctx)
		return nil, time.Time{}, ids.AuditID{}, false, err
	}
	if _, err := store.GetClaimForUpdate(ctx, uow, tenant, id); err != nil {
		_ = uow.Rollback(ctx)
		return nil, time.Time{}, ids.AuditID{}, false, err
	}
	var now time.Time
	if err := uow.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		_ = uow.Rollback(ctx)
		return nil, time.Time{}, ids.AuditID{}, false, err
	}
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, time.Time{}, ids.AuditID{}, false, err
	}
	return uow, now, auditID, true, nil
}

// snapshotMigrationClaim freezes the tenant-wide source scope only after the
// lease-owning claim committed. It is intentionally separate from ordinary
// claim maintenance: unlinked claims retain their existing semantics.
func (w *Worker) snapshotMigrationClaim(ctx context.Context, tenant ids.TenantID, id ids.ClaimID) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = uow.Rollback(ctx) }()
	var now time.Time
	if err := uow.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	pass, items, err := store.SnapshotMigrationClaim(ctx, uow, tenant, id, w.owner, now.UTC())
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrStaleVersion) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	return w.driveMigrationItems(ctx, tenant, id, pass, items)
}
