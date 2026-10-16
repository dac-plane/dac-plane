package worker

import (
	"context"
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
	now := time.Now().UTC()
	claims, err := store.OpenClaims(ctx, w.pool, tenant)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now.Before(claim.ExpiresAt) {
			if claim.Claimant != w.owner || claim.ExpiresAt.Sub(now) > w.cfg.ClaimTTL/2 {
				continue
			}
			if err := w.renewClaim(ctx, tenant, claim.ID, now); err != nil {
				w.logger.Error("claim renew failed", "claim", claim.ID.String(), "error", err)
			}
			continue
		}
		if err := w.takeClaim(ctx, tenant, claim.ID, now); err != nil {
			w.logger.Error("claim takeover failed", "claim", claim.ID.String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) takeClaim(ctx context.Context, tenant ids.TenantID, id ids.ClaimID, now time.Time) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return err
	}
	if _, err := store.TakeClaim(ctx, uow, tenant, id, w.owner, w.cfg.ClaimTTL, store.Transition{
		At: now, Actor: SystemActor(), AuditID: auditID,
	}); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	w.stats.Reaped.Add(1)
	return nil
}

func (w *Worker) renewClaim(ctx context.Context, tenant ids.TenantID, id ids.ClaimID, now time.Time) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return err
	}
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
	return nil
}
