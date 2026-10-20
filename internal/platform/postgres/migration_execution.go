package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// SnapshotMigrationItems freezes the source-owned nonterminal Attempts for a
// pass. The unique key makes a crashed worker's replay compare to the same
// snapshot instead of adding work which appeared after execution started.
func SnapshotMigrationItems(ctx context.Context, u *UnitOfWork, pass lifecycle.MigrationPass, epoch uint64, at time.Time) ([]lifecycle.MigrationItem, error) {
	if pass.State != lifecycle.MigrationExecuting || epoch == 0 {
		return nil, ErrPayloadMismatch
	}
	rows, err := u.Query(ctx, `
		select distinct on (a.attempt_id) a.attempt_id::text
		from attempt a join provider_operation o on o.tenant_id=a.tenant_id and o.attempt_id=a.attempt_id
		where a.tenant_id=$1 and o.provider_id=$2
		and a.state not in ('completed','failed','terminated')
		order by a.attempt_id,o.created_at desc,o.operation_id desc`, pass.Tenant.String(), pass.From.String())
	if err != nil {
		return nil, err
	}
	var sources []ids.AttemptID
	for rows.Next() {
		var sourceText string
		if err := rows.Scan(&sourceText); err != nil {
			rows.Close()
			return nil, err
		}
		source, err := parseID[ids.Attempt]("migration_item.source_attempt_id", sourceText)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, source := range sources {
		item := lifecycle.MigrationItem{Tenant: pass.Tenant, Migration: pass.ID, SourceAttempt: source, State: lifecycle.MigrationItemQueued, ClaimEpoch: epoch, UpdatedAt: at}
		if err := item.Validate(); err != nil {
			return nil, err
		}
		if _, err := u.Exec(ctx, `insert into migration_item(tenant_id,migration_id,source_attempt_id,state,claim_epoch,updated_at) values($1,$2,$3,$4,$5,$6) on conflict (tenant_id,migration_id,source_attempt_id) do nothing`, item.Tenant.String(), item.Migration.String(), item.SourceAttempt.String(), string(item.State), int64(item.ClaimEpoch), item.UpdatedAt); err != nil {
			return nil, err
		}
	}
	// A takeover advances the claim epoch. Existing unfinished items must be
	// explicitly handed to the new fence; a stale worker can no longer settle
	// them after this write.
	if _, err := u.Exec(ctx, `update migration_item set claim_epoch=$3,updated_at=$4 where tenant_id=$1 and migration_id=$2 and state not in ('completed','failed','unresolved')`, pass.Tenant.String(), pass.ID.String(), int64(epoch), at); err != nil {
		return nil, err
	}
	return ListMigrationItems(ctx, u, pass.Tenant, pass.ID)
}

// SnapshotMigrationClaim validates the current holder, the executing pass
// and its previewed qualifications before freezing or taking over work.
func SnapshotMigrationClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, claimID ids.ClaimID, owner string, at time.Time) (lifecycle.MigrationPass, []lifecycle.MigrationItem, error) {
	if err := LockMigrationAdministration(ctx, u, tenant); err != nil {
		return lifecycle.MigrationPass{}, nil, err
	}
	slot, err := GetMigrationSlot(ctx, u, tenant)
	if err != nil || slot.Claim != claimID || slot.Active.IsZero() {
		if err == nil {
			err = ErrNotFound
		}
		return lifecycle.MigrationPass{}, nil, err
	}
	pass, err := GetMigrationPassForUpdate(ctx, u, tenant, slot.Active)
	if err != nil || pass.State != lifecycle.MigrationExecuting {
		if err == nil {
			err = ErrStaleVersion
		}
		return lifecycle.MigrationPass{}, nil, err
	}
	claim, err := GetClaimForUpdate(ctx, u, tenant, claimID)
	if err != nil {
		return lifecycle.MigrationPass{}, nil, err
	}
	if claim.State != lifecycle.ClaimClaimed || claim.Claimant != owner || !at.Before(claim.ExpiresAt) {
		return lifecycle.MigrationPass{}, nil, ErrStaleVersion
	}
	preview, err := GetMigrationPreview(ctx, u, tenant, pass.ID)
	if err != nil {
		return lifecycle.MigrationPass{}, nil, err
	}
	from, to, err := LockMigrationQualifications(ctx, u, pass)
	if err != nil || !from.Mode.Authoritative() || !to.Mode.Authoritative() || from.Version != preview.FromVersion || to.Version != preview.ToVersion {
		if err == nil {
			err = ErrStaleVersion
		}
		return lifecycle.MigrationPass{}, nil, err
	}
	items, err := SnapshotMigrationItems(ctx, u, pass, claim.Epoch, at)
	return pass, items, err
}

// ListMigrationItems reads the frozen pass scope in source Attempt order.
func ListMigrationItems(ctx context.Context, q Querier, tenant ids.TenantID, migration ids.MigrationID) ([]lifecycle.MigrationItem, error) {
	rows, err := q.Query(ctx, `select source_attempt_id::text,target_attempt_id::text,quiesce_operation_id::text,restart_operation_id::text,cleanup_operation_id::text,state,claim_epoch,updated_at from migration_item where tenant_id=$1 and migration_id=$2 order by source_attempt_id`, tenant.String(), migration.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lifecycle.MigrationItem
	for rows.Next() {
		var source string
		var target *string
		var quiesce, restart, cleanup *string
		var state string
		var epoch int64
		item := lifecycle.MigrationItem{Tenant: tenant, Migration: migration}
		if err := rows.Scan(&source, &target, &quiesce, &restart, &cleanup, &state, &epoch, &item.UpdatedAt); err != nil {
			return nil, err
		}
		var err error
		if item.SourceAttempt, err = parseID[ids.Attempt]("migration_item.source_attempt_id", source); err != nil {
			return nil, err
		}
		if target != nil {
			if item.TargetAttempt, err = parseID[ids.Attempt]("migration_item.target_attempt_id", *target); err != nil {
				return nil, err
			}
		}
		if quiesce != nil {
			if item.QuiesceOperation, err = parseID[ids.Operation]("migration_item.quiesce_operation_id", *quiesce); err != nil {
				return nil, err
			}
		}
		if restart != nil {
			if item.RestartOperation, err = parseID[ids.Operation]("migration_item.restart_operation_id", *restart); err != nil {
				return nil, err
			}
		}
		if cleanup != nil {
			if item.CleanupOperation, err = parseID[ids.Operation]("migration_item.cleanup_operation_id", *cleanup); err != nil {
				return nil, err
			}
		}
		item.State, item.ClaimEpoch = lifecycle.MigrationItemState(state), uint64(epoch)
		if err := item.Validate(); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SetMigrationItem updates an item only while the current migration holder
// owns the same claim epoch. A successor must be new and an item can never
// be moved backward to a pre-cutover state.
func SetMigrationItem(ctx context.Context, u *UnitOfWork, item lifecycle.MigrationItem, epoch uint64) error {
	if err := item.Validate(); err != nil || item.ClaimEpoch != epoch {
		return ErrPayloadMismatch
	}
	var current int64
	err := u.QueryRow(ctx, `select claim_epoch from migration_item where tenant_id=$1 and migration_id=$2 and source_attempt_id=$3 for update`, item.Tenant.String(), item.Migration.String(), item.SourceAttempt.String()).Scan(&current)
	if isNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if uint64(current) != epoch {
		return ErrStaleVersion
	}
	return execVersionedUpdate(ctx, u, `update migration_item set target_attempt_id=$4,quiesce_operation_id=$5,restart_operation_id=$6,cleanup_operation_id=$7,state=$8,updated_at=$9 where tenant_id=$1 and migration_id=$2 and source_attempt_id=$3 and claim_epoch=$10`, item.Tenant.String(), item.Migration.String(), item.SourceAttempt.String(), textOrNil(item.TargetAttempt.String()), textOrNil(item.QuiesceOperation.String()), textOrNil(item.RestartOperation.String()), textOrNil(item.CleanupOperation.String()), string(item.State), item.UpdatedAt, int64(epoch))
}
