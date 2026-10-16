// Package postgres Claim repositories persist maintenance claims, harness qualifications and
// migration passes. Holder operations run the pure fencing logic in memory
// and commit under the row lock with version predicates, so a stale epoch
// updates zero rows.
package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// InsertClaim persists a scheduled claim with replay comparison.
func InsertClaim(ctx context.Context, u *UnitOfWork, claim lifecycle.MaintenanceClaim) (lifecycle.MaintenanceClaim, bool, error) {
	if err := claim.Validate(); err != nil {
		return lifecycle.MaintenanceClaim{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into maintenance_claim (tenant_id, claim_id, provider_id, resource_ref, state, claimant, epoch, expires_at, version, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		claim.Tenant.String(), claim.ID.String(), claim.Provider.String(), claim.ResourceRef,
		string(claim.State), textOrNil(claim.Claimant), int64(claim.Epoch),
		timeOrNil(claim.ExpiresAt), int64(claim.Version), claim.UpdatedAt,
	)
	if err != nil {
		return lifecycle.MaintenanceClaim{}, false, err
	}
	if inserted {
		return claim, false, nil
	}
	stored, loadErr := GetClaim(ctx, u, claim.Tenant, claim.ID)
	if isNotFound(loadErr) {
		return lifecycle.MaintenanceClaim{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.MaintenanceClaim{}, false, loadErr
	}
	if !claimsEqual(stored, claim) {
		return lifecycle.MaintenanceClaim{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

func claimsEqual(a, b lifecycle.MaintenanceClaim) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Provider == b.Provider &&
		a.ResourceRef == b.ResourceRef && a.State == b.State && a.Claimant == b.Claimant &&
		a.Epoch == b.Epoch && sameInstant(a.ExpiresAt, b.ExpiresAt) && a.Version == b.Version
}

// GetClaim loads a claim by tenant and id.
func GetClaim(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ClaimID) (lifecycle.MaintenanceClaim, error) {
	var claim lifecycle.MaintenanceClaim
	var state, idText, tenantText, providerText, resourceText string
	var claimantText *string
	var epoch, version int64
	var expires *time.Time
	err := q.QueryRow(ctx, `
		select claim_id::text, tenant_id::text, provider_id::text, resource_ref, state, claimant, epoch, expires_at, version, updated_at
		from maintenance_claim where tenant_id = $1 and claim_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &providerText, &resourceText, &state, &claimantText, &epoch, &expires, &version, &claim.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.MaintenanceClaim{}, ErrNotFound
		}
		return lifecycle.MaintenanceClaim{}, err
	}
	var parseErr error
	if claim.ID, parseErr = parseID[ids.Claim](`maintenance_claim.claim_id`, idText); parseErr != nil {
		return lifecycle.MaintenanceClaim{}, parseErr
	}
	if claim.Tenant, parseErr = parseID[ids.Tenant](`maintenance_claim.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.MaintenanceClaim{}, parseErr
	}
	if claim.Provider, parseErr = parseID[ids.Provider](`maintenance_claim.provider_id`, providerText); parseErr != nil {
		return lifecycle.MaintenanceClaim{}, parseErr
	}
	claim.ResourceRef = resourceText
	claim.State = lifecycle.ClaimState(state)
	if claimantText != nil {
		claim.Claimant = *claimantText
	}
	claim.Epoch = uint64(epoch)
	if expires != nil {
		claim.ExpiresAt = *expires
	}
	claim.Version = uint64(version)
	if err := claim.Validate(); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	return claim, nil
}

// GetClaimForUpdate locks a claim row for holder operations.
func GetClaimForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ClaimID) (lifecycle.MaintenanceClaim, error) {
	if err := lockRow(ctx, u, "maintenance_claim", "claim_id", tenant.String(), id.String()); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	return GetClaim(ctx, u, tenant, id)
}

// updateClaimRow writes a claim after a holder operation. The version and
// epoch predicates fence stale holders: zero rows mean someone else moved it.
func updateClaimRow(ctx context.Context, u *UnitOfWork, claim lifecycle.MaintenanceClaim, expectedVersion, expectedEpoch uint64) error {
	if err := execVersionedUpdate(ctx, u, `
		update maintenance_claim set state = $3, claimant = $4, epoch = $5, expires_at = $6, version = $7, updated_at = $8
		where tenant_id = $1 and claim_id = $2 and version = $9 and epoch = $10`,
		claim.Tenant.String(), claim.ID.String(), string(claim.State), textOrNil(claim.Claimant),
		int64(claim.Epoch), timeOrNil(claim.ExpiresAt), int64(claim.Version), claim.UpdatedAt,
		int64(expectedVersion), int64(expectedEpoch),
	); err != nil {
		return err
	}
	return nil
}

// TakeClaim claims or takes over a claim: row update plus history commit
// together, with refusal evidence on rejection.
func TakeClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ClaimID, claimant string, ttl time.Duration, tr Transition) (lifecycle.MaintenanceClaim, error) {
	claim, err := GetClaimForUpdate(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	priorVersion, priorEpoch := claim.Version, claim.Epoch
	next, change, claimErr := claim.Claim(claimant, ttl, tr.At)
	if claimErr != nil {
		if terr, ok := claimErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.MaintenanceClaim{}, histErr
			}
		}
		return lifecycle.MaintenanceClaim{}, claimErr
	}
	if err := updateClaimRow(ctx, u, next, priorVersion, priorEpoch); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	return next, nil
}

// HoldClaim runs one holder operation (renew, complete or release) selected
// by op. The epoch predicate fences holders that lost the claim.
func HoldClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ClaimID, op func(lifecycle.MaintenanceClaim) (lifecycle.MaintenanceClaim, lifecycle.Change, error), tr Transition) (lifecycle.MaintenanceClaim, error) {
	claim, err := GetClaimForUpdate(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	priorVersion, priorEpoch := claim.Version, claim.Epoch
	next, change, opErr := op(claim)
	if opErr != nil {
		if terr, ok := opErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.MaintenanceClaim{}, histErr
			}
		}
		return lifecycle.MaintenanceClaim{}, opErr
	}
	if err := updateClaimRow(ctx, u, next, priorVersion, priorEpoch); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.MaintenanceClaim{}, err
	}
	return next, nil
}

// UpsertHarness persists a harness qualification, replacing the tenant's
// provider row unconditionally. Stepwise moves go through ApplyHarness so
// the version predicate and history still apply; this is the initial seed
// and test path, not a transition.
func UpsertHarness(ctx context.Context, u *UnitOfWork, harness lifecycle.HarnessQualification) (lifecycle.HarnessQualification, error) {
	if err := harness.Validate(); err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	_, err := u.Exec(ctx, `
		insert into harness_qualification (tenant_id, provider_id, mode, version, updated_at)
		values ($1, $2, $3, $4, $5)
		on conflict (tenant_id, provider_id) do update set mode = excluded.mode, version = excluded.version, updated_at = excluded.updated_at`,
		harness.Tenant.String(), harness.Provider.String(), string(harness.Mode),
		int64(harness.Version), harness.UpdatedAt,
	)
	if err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	return harness, nil
}

// GetHarness loads a tenant's qualification of a provider.
func GetHarness(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID) (lifecycle.HarnessQualification, error) {
	var harness lifecycle.HarnessQualification
	var mode string
	var version int64
	err := q.QueryRow(ctx, `
		select mode, version, updated_at from harness_qualification where tenant_id = $1 and provider_id = $2`,
		tenant.String(), provider.String(),
	).Scan(&mode, &version, &harness.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.HarnessQualification{}, ErrNotFound
		}
		return lifecycle.HarnessQualification{}, err
	}
	harness.Tenant, harness.Provider = tenant, provider
	harness.Mode = lifecycle.HarnessMode(mode)
	harness.Version = uint64(version)
	if err := harness.Validate(); err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	return harness, nil
}

// ApplyHarness performs a harness event at the expected version with atomic
// history. Promotion requires a principal actor; the pure Apply enforces it.
func ApplyHarness(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, provider ids.ProviderID, event lifecycle.HarnessEvent, tr Transition) (lifecycle.HarnessQualification, error) {
	harness, err := GetHarnessForUpdate(ctx, u, tenant, provider)
	if err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	next, change, applyErr := harness.Apply(event, tr.ExpectedVersion, tr.Actor, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.HarnessQualification{}, histErr
			}
		}
		return lifecycle.HarnessQualification{}, applyErr
	}
	if err := execVersionedUpdate(ctx, u, `
		update harness_qualification set mode = $3, version = $4, updated_at = $5
		where tenant_id = $1 and provider_id = $2 and version = $6`,
		tenant.String(), provider.String(), string(next.Mode), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	); err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	return next, nil
}

// GetHarnessForUpdate locks a harness row for read-modify-write.
func GetHarnessForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, provider ids.ProviderID) (lifecycle.HarnessQualification, error) {
	if err := lockRow(ctx, u, "harness_qualification", "provider_id", tenant.String(), provider.String()); err != nil {
		return lifecycle.HarnessQualification{}, err
	}
	return GetHarness(ctx, u, tenant, provider)
}

// InsertMigrationPass persists a planned pass with replay comparison.
func InsertMigrationPass(ctx context.Context, u *UnitOfWork, pass lifecycle.MigrationPass) (lifecycle.MigrationPass, bool, error) {
	if err := pass.Validate(); err != nil {
		return lifecycle.MigrationPass{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into migration_pass (tenant_id, migration_id, from_provider_id, to_provider_id, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		pass.Tenant.String(), pass.ID.String(), pass.From.String(), pass.To.String(),
		string(pass.State), int64(pass.Version), pass.CreatedAt, pass.UpdatedAt,
	)
	if err != nil {
		return lifecycle.MigrationPass{}, false, err
	}
	if inserted {
		return pass, false, nil
	}
	stored, loadErr := GetMigrationPass(ctx, u, pass.Tenant, pass.ID)
	if isNotFound(loadErr) {
		return lifecycle.MigrationPass{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.MigrationPass{}, false, loadErr
	}
	if stored.From != pass.From || stored.To != pass.To || stored.State != pass.State || stored.Version != pass.Version {
		return lifecycle.MigrationPass{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetMigrationPass loads a migration pass by tenant and id.
func GetMigrationPass(ctx context.Context, q Querier, tenant ids.TenantID, id ids.MigrationID) (lifecycle.MigrationPass, error) {
	var pass lifecycle.MigrationPass
	var state, idText, tenantText, fromText, toText string
	err := q.QueryRow(ctx, `
		select migration_id::text, tenant_id::text, from_provider_id::text, to_provider_id::text, state, version, created_at, updated_at
		from migration_pass where tenant_id = $1 and migration_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &fromText, &toText, &state, &pass.Version, &pass.CreatedAt, &pass.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.MigrationPass{}, ErrNotFound
		}
		return lifecycle.MigrationPass{}, err
	}
	var parseErr error
	if pass.ID, parseErr = parseID[ids.Migration](`migration_pass.migration_id`, idText); parseErr != nil {
		return lifecycle.MigrationPass{}, parseErr
	}
	if pass.Tenant, parseErr = parseID[ids.Tenant](`migration_pass.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.MigrationPass{}, parseErr
	}
	if pass.From, parseErr = parseID[ids.Provider](`migration_pass.from_provider_id`, fromText); parseErr != nil {
		return lifecycle.MigrationPass{}, parseErr
	}
	if pass.To, parseErr = parseID[ids.Provider](`migration_pass.to_provider_id`, toText); parseErr != nil {
		return lifecycle.MigrationPass{}, parseErr
	}
	pass.State = lifecycle.MigrationState(state)
	if err := pass.Validate(); err != nil {
		return lifecycle.MigrationPass{}, err
	}
	return pass, nil
}

// ApplyMigrationPass performs a migration event at the expected version with
// atomic history. Preview-after-hold rules live in the pure machine.
func ApplyMigrationPass(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.MigrationID, event lifecycle.MigrationEvent, tr Transition) (lifecycle.MigrationPass, error) {
	var locked bool
	err := u.QueryRow(ctx, `select true from migration_pass where tenant_id = $1 and migration_id = $2 for update`,
		tenant.String(), id.String()).Scan(&locked)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.MigrationPass{}, ErrNotFound
		}
		return lifecycle.MigrationPass{}, err
	}
	pass, err := GetMigrationPass(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.MigrationPass{}, err
	}
	next, change, applyErr := pass.Apply(event, tr.ExpectedVersion, tr.Actor, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.MigrationPass{}, histErr
			}
		}
		return lifecycle.MigrationPass{}, applyErr
	}
	if err := execVersionedUpdate(ctx, u, `
		update migration_pass set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and migration_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	); err != nil {
		return lifecycle.MigrationPass{}, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.MigrationPass{}, err
	}
	return next, nil
}
