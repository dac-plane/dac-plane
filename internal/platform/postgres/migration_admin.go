package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// SetHarnessAdministrator is trusted provisioning, not a public command.
// Updating the same row serializes revocation with administration and replay.
func SetHarnessAdministrator(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, principal ids.PrincipalID, enabled bool) error {
	_, err := u.Exec(ctx, `insert into harness_administrator(tenant_id,principal_id,enabled,updated_at) values($1,$2,$3,clock_timestamp()) on conflict(tenant_id,principal_id) do update set enabled=excluded.enabled,updated_at=excluded.updated_at`, tenant.String(), principal.String(), enabled)
	return err
}

// LockHarnessAdministrator requires current permission, including on replay.
func LockHarnessAdministrator(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, principal ids.PrincipalID) (bool, error) {
	var enabled bool
	err := u.QueryRow(ctx, `select enabled from harness_administrator where tenant_id=$1 and principal_id=$2 for update`, tenant.String(), principal.String()).Scan(&enabled)
	if isNoRows(err) {
		return false, nil
	}
	return enabled, err
}

// LockMigrationAdministration serializes all administrative passes in a tenant.
// Lock order: permission (commands), tenant advisory, slot, pass, sorted
// qualifications, claim. The reaper joins this order without a permission row.
func LockMigrationAdministration(ctx context.Context, u *UnitOfWork, tenant ids.TenantID) error {
	_, err := u.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0))`, "migration-admin:"+tenant.String())
	return err
}

// MigrationPreview is a product snapshot, never proof of physical migration.
type MigrationPreview struct {
	FromVersion, ToVersion uint64
	At                     time.Time
}

// InsertAdminMigration records creation and history in the command transaction.
func InsertAdminMigration(ctx context.Context, u *UnitOfWork, p lifecycle.MigrationPass, tr Transition) error {
	if _, _, err := InsertMigrationPass(ctx, u, p); err != nil {
		return err
	}
	if err := SetMigrationPreview(ctx, u, p.Tenant, p.ID, MigrationPreview{}); err != nil {
		return err
	}
	change := lifecycle.Change{Tenant: p.Tenant, Record: lifecycle.RecordMigration, RecordID: p.ID.Untyped(), Event: "create", From: string(p.State), To: string(p.State), Version: p.Version}
	return appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), nil)
}

// GetMigrationPreview reads current preview evidence. Legacy passes without
// administration metadata return ErrNotFound, not a fabricated snapshot.
func GetMigrationPreview(ctx context.Context, q Querier, tenant ids.TenantID, id ids.MigrationID) (MigrationPreview, error) {
	var p MigrationPreview
	var from, to *int64
	var at *time.Time
	err := q.QueryRow(ctx, `select from_version,to_version,preview_at from migration_administration where tenant_id=$1 and migration_id=$2`, tenant.String(), id.String()).Scan(&from, &to, &at)
	if isNoRows(err) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if at != nil {
		p = MigrationPreview{FromVersion: uint64(*from), ToVersion: uint64(*to), At: *at}
	}
	return p, nil
}

// SetMigrationPreview updates the snapshot under the caller's tenant/pass locks.
func SetMigrationPreview(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.MigrationID, p MigrationPreview) error {
	_, err := u.Exec(ctx, `insert into migration_administration(tenant_id,migration_id,from_version,to_version,preview_at) values($1,$2,$3,$4,$5) on conflict(tenant_id,migration_id) do update set from_version=excluded.from_version,to_version=excluded.to_version,preview_at=excluded.preview_at`, tenant.String(), id.String(), positiveOrNil(p.FromVersion), positiveOrNil(p.ToVersion), timeOrNil(p.At))
	return err
}

func positiveOrNil(v uint64) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}

// MigrationSlot keeps one reusable administrative claim across successive passes.
type MigrationSlot struct {
	Claim  ids.ClaimID
	Active ids.MigrationID
}

// GetMigrationSlot locks the tenant slot after LockMigrationAdministration.
func GetMigrationSlot(ctx context.Context, u *UnitOfWork, tenant ids.TenantID) (MigrationSlot, error) {
	var s MigrationSlot
	var claim string
	var active *string
	err := u.QueryRow(ctx, `select claim_id::text,active_migration_id::text from migration_slot where tenant_id=$1 for update`, tenant.String()).Scan(&claim, &active)
	if isNoRows(err) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	s.Claim, err = ids.Parse[ids.Claim](claim)
	if err != nil {
		return s, err
	}
	if active != nil {
		s.Active, err = ids.Parse[ids.Migration](*active)
	}
	return s, err
}

// SetMigrationSlot assigns or clears active intent. An existing claim identity
// is never replaced; callers hold the tenant advisory and existing slot locks.
func SetMigrationSlot(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, s MigrationSlot) error {
	var active any
	if !s.Active.IsZero() {
		active = s.Active.String()
	}
	_, err := u.Exec(ctx, `insert into migration_slot(tenant_id,claim_id,active_migration_id) values($1,$2,$3) on conflict(tenant_id) do update set active_migration_id=excluded.active_migration_id`, tenant.String(), s.Claim.String(), active)
	return err
}

// GetMigrationPassForUpdate locks a pass for versioned administration.
func GetMigrationPassForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.MigrationID) (lifecycle.MigrationPass, error) {
	if err := lockRow(ctx, u, "migration_pass", "migration_id", tenant.String(), id.String()); err != nil {
		return lifecycle.MigrationPass{}, err
	}
	return GetMigrationPass(ctx, u, tenant, id)
}

// LockMigrationQualifications uses deterministic provider order to avoid
// reverse-direction migrations deadlocking each other.
func LockMigrationQualifications(ctx context.Context, u *UnitOfWork, p lifecycle.MigrationPass) (lifecycle.HarnessQualification, lifecycle.HarnessQualification, error) {
	first, second := p.From, p.To
	if first.String() > second.String() {
		first, second = second, first
	}
	a, err := GetHarnessForUpdate(ctx, u, p.Tenant, first)
	if err != nil {
		return a, lifecycle.HarnessQualification{}, err
	}
	b, err := GetHarnessForUpdate(ctx, u, p.Tenant, second)
	if first != p.From {
		a, b = b, a
	}
	return a, b, err
}

// PrepareMaintenanceClaim gates linked administrative work without assigning
// transfer semantics to a generic claim. Historical unlinked claims retain
// their original behavior. No method here completes a migration pass.
func PrepareMaintenanceClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ClaimID) (bool, error) {
	var linked bool
	if err := u.QueryRow(ctx, `select exists(select 1 from migration_slot where tenant_id=$1 and claim_id=$2)`, tenant.String(), id.String()).Scan(&linked); err != nil {
		return false, err
	}
	if !linked {
		return true, nil
	}
	if err := LockMigrationAdministration(ctx, u, tenant); err != nil {
		return false, err
	}
	s, err := GetMigrationSlot(ctx, u, tenant)
	if err != nil {
		return false, err
	}
	if s.Active.IsZero() {
		return false, nil
	}
	p, err := GetMigrationPassForUpdate(ctx, u, tenant, s.Active)
	if err != nil {
		return false, err
	}
	if p.State != lifecycle.MigrationExecuting {
		return false, nil
	}
	preview, err := GetMigrationPreview(ctx, u, tenant, p.ID)
	if err != nil {
		return false, err
	}
	a, b, err := LockMigrationQualifications(ctx, u, p)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && a.Mode.Authoritative() && b.Mode.Authoritative() && a.Version == preview.FromVersion && b.Version == preview.ToVersion, err
}
