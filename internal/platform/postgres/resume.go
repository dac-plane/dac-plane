// Package postgres Resume readers load the durable state a restarted process needs to pick up
// work: undispatched outbox rows, uncertain operations awaiting
// reconciliation, held claims, and stream cursor positions. They read; the
// relay and reconciliation loops that consume them belong to #21.
package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// OutboxRow is one undispatched durable dispatch.
type OutboxRow struct {
	ID        ids.AnyID
	Attempt   ids.AttemptID
	Record    lifecycle.RecordKind
	RecordID  ids.AnyID
	Event     string
	Payload   []byte
	Demand    contracts.Demand
	Attempts  int
	NextTryAt time.Time
	LastError string
	CreatedAt time.Time
}

// scanOutboxRow reads one outbox row in the column order of selectOutboxRow.
func scanOutboxRow(rows pgx.Rows, row *OutboxRow) error {
	var idText, recordText, recordIDText, eventText, demandText string
	var attemptText, lastErrText *string
	var payload []byte
	if err := rows.Scan(&idText, &attemptText, &recordText, &recordIDText,
		&eventText, &payload, &demandText, &row.Attempts, &row.NextTryAt,
		&lastErrText, &row.CreatedAt); err != nil {
		return err
	}
	var parseErr error
	if row.ID, parseErr = parseID[ids.Any](`outbox.outbox_id`, idText); parseErr != nil {
		return parseErr
	}
	if row.Attempt, parseErr = parseOptionalID[ids.Attempt](`outbox.attempt_id`, attemptText); parseErr != nil {
		return parseErr
	}
	row.Record = lifecycle.RecordKind(recordText)
	if row.RecordID, parseErr = parseID[ids.Any](`outbox.record_id`, recordIDText); parseErr != nil {
		return parseErr
	}
	row.Event, row.Payload, row.Demand = eventText, payload, contracts.Demand(demandText)
	if lastErrText != nil {
		row.LastError = *lastErrText
	}
	return nil
}

// selectOutboxRow is the column list every outbox reader scans with
// scanOutboxRow.
const selectOutboxRow = `select outbox_id::text, attempt_id::text, record_kind, record_id::text, event, payload, demand, attempts, next_attempt_at, last_error, created_at from outbox`

// OpenOutboxEntries returns pending outbox rows oldest first, bounded by
// limit. Non-positive limits read everything pending.
func OpenOutboxEntries(ctx context.Context, q Querier, tenant ids.TenantID, limit int) ([]OutboxRow, error) {
	query := selectOutboxRow + ` where tenant_id = $1 and status = 'pending' order by created_at`
	var args []any
	args = append(args, tenant.String())
	if limit > 0 {
		query += ` limit $2`
		args = append(args, limit)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		if err := scanOutboxRow(rows, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OutboxReadyEntries returns pending, unexhausted outbox rows whose
// not-before time has passed, oldest due first. The dispatch loop polls
// through here; the scheduler orders the returned set.
func OutboxReadyEntries(ctx context.Context, q Querier, tenant ids.TenantID, now time.Time, limit int) ([]OutboxRow, error) {
	query := selectOutboxRow + ` where tenant_id = $1 and status = 'pending' and exhausted_at is null and next_attempt_at <= $2 order by next_attempt_at`
	var args []any
	args = append(args, tenant.String(), now)
	if limit > 0 {
		query += ` limit $3`
		args = append(args, limit)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		if err := scanOutboxRow(rows, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// OutboxReadyDispatch returns the dispatchable subset of due rows: pending
// rows plus processing rows whose claim lease lapsed (crash or SendPending
// leftovers the claim path reclaims), oldest due first. It owns the
// dispatchable definition; the claim statements mirror it as an atomic
// backstop. Every other pending row belongs to future surfaces.
func OutboxReadyDispatch(ctx context.Context, q Querier, tenant ids.TenantID, now time.Time, limit int) ([]OutboxRow, error) {
	query := selectOutboxRow + ` where tenant_id = $1 and exhausted_at is null and next_attempt_at <= $2
		and ((record_kind = 'provider_operation') or (record_kind = 'attempt' and event = 'request_cancel'))
		and (status = 'pending'
			or (status = 'processing' and lease_expires_at is not null and lease_expires_at <= $2))
		order by next_attempt_at`
	var args []any
	args = append(args, tenant.String(), now)
	if limit > 0 {
		query += ` limit $3`
		args = append(args, limit)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var row OutboxRow
		if err := scanOutboxRow(rows, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UncertainOperations returns operations stuck in unknown durability for
// reconciliation. They are never redispatched; #21 resolves them.
func UncertainOperations(ctx context.Context, q Querier, tenant ids.TenantID) ([]lifecycle.ProviderOperation, error) {
	rows, err := q.Query(ctx, `
		select operation_id::text from provider_operation
		where tenant_id = $1 and state = 'uncertain' order by updated_at`,
		tenant.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lifecycle.ProviderOperation
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, err
		}
		id, err := parseID[ids.Operation](`provider_operation.operation_id`, idText)
		if err != nil {
			return nil, err
		}
		operation, err := GetOperation(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		out = append(out, operation)
	}
	return out, rows.Err()
}

// OpenClaims returns claims currently held, so a restart can compare expiry
// and take over lapsed ones. Expiry itself is evaluated by the pure
// claim logic, not here.
func OpenClaims(ctx context.Context, q Querier, tenant ids.TenantID) ([]lifecycle.MaintenanceClaim, error) {
	rows, err := q.Query(ctx, `
		select claim_id::text from maintenance_claim
		where tenant_id = $1 and state = 'claimed' order by updated_at`,
		tenant.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lifecycle.MaintenanceClaim
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, err
		}
		id, err := parseID[ids.Claim](`maintenance_claim.claim_id`, idText)
		if err != nil {
			return nil, err
		}
		claim, err := GetClaim(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	return out, rows.Err()
}
