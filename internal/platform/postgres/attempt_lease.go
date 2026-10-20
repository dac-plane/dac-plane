package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// AttemptLease is the fence a worker holds while it drives one attempt: the
// owner name plus the epoch the claim granted. Present the epoch on renewal
// and release; a mismatch means another worker took the attempt first.
type AttemptLease struct {
	Owner string
	Epoch int64
}

// CheckAttemptLease fences settlement under the caller's Attempt lock, using
// server time after any lock wait. Epoch equality alone cannot revive expiry.
func CheckAttemptLease(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attempt ids.AttemptID, lease AttemptLease) error {
	var current bool
	if err := u.QueryRow(ctx, `select exists(select 1 from attempt where tenant_id=$1 and attempt_id=$2 and lease_owner=$3 and lease_epoch=$4 and lease_expires_at>clock_timestamp())`,
		tenant.String(), attempt.String(), lease.Owner, lease.Epoch).Scan(&current); err != nil {
		return err
	}
	if !current {
		return ErrStaleVersion
	}
	return nil
}

// ClaimAttemptLease takes the dispatch lease on an attempt for owner. It
// succeeds when the attempt is unowned, the lease lapsed, or owner already
// holds it (idempotent renewal under a new epoch only on takeover). A live
// lease owned by someone else reports ErrStaleVersion.
func ClaimAttemptLease(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attempt ids.AttemptID, owner string, ttl time.Duration, now time.Time) (AttemptLease, error) {
	var lease AttemptLease
	err := u.QueryRow(ctx, `
		update attempt set lease_owner = $4, lease_epoch = lease_epoch + 1, lease_expires_at = $5, updated_at = $6
		where tenant_id = $1 and attempt_id = $2
			and (lease_owner is null or lease_expires_at is null or lease_expires_at <= $3 or lease_owner = $4)
		returning lease_epoch, lease_owner`,
		tenant.String(), attempt.String(), now, owner, now.Add(ttl), now,
	).Scan(&lease.Epoch, &lease.Owner)
	if err != nil {
		if isNoRows(err) {
			return AttemptLease{}, ErrStaleVersion
		}
		return AttemptLease{}, err
	}
	return lease, nil
}

// RenewAttemptLease extends owner's lease to now+ttl. The owner-and-epoch
// predicate fences a worker whose lease was taken over: zero rows mean stale.
func RenewAttemptLease(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attempt ids.AttemptID, lease AttemptLease, ttl time.Duration, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update attempt set lease_expires_at = $5, updated_at = $6
		where tenant_id = $1 and attempt_id = $2 and lease_owner = $3 and lease_epoch = $4`,
		tenant.String(), attempt.String(), lease.Owner, lease.Epoch, now.Add(ttl), now,
	)
}

// ReleaseAttemptLease drops owner's lease when it still holds it. A taken-over
// lease is left alone: zero rows mean someone else owns the attempt now.
func ReleaseAttemptLease(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attempt ids.AttemptID, lease AttemptLease, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update attempt set lease_owner = null, lease_expires_at = null, updated_at = $5
		where tenant_id = $1 and attempt_id = $2 and lease_owner = $3 and lease_epoch = $4`,
		tenant.String(), attempt.String(), lease.Owner, lease.Epoch, now,
	)
}
