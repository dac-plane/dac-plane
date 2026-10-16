// Package postgres History appends audit, durable-event and outbox rows atomically with the
// state change they describe. Refusals record an audit event only: the
// durable key is one row per record version, so refusal evidence must never
// compete with the applied change for it, and nothing downstream dispatches
// a refusal.
package postgres

import (
	"context"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// OutboxPayload is the dispatch record for one applied change. Payloads carry
// digests and identifiers only, never provider credentials or raw inputs.
type OutboxPayload struct {
	// ID is the stable identity of this dispatch. Retried commands reuse it.
	ID ids.AnyID
	// Attempt scopes attempt-bound dispatches; zero when not applicable.
	Attempt ids.AttemptID
	// Body is the JSON dispatch body.
	Body []byte
}

// appendHistory records audit, durable event and, when outbox is non-nil,
// outbox rows for change in the same transaction as the state mutation.
// A colliding outbox identity means the same command id was reused with
// different content and is rejected as a payload mismatch.
func appendHistory(ctx context.Context, q Querier, change lifecycle.Change, audit lifecycle.AuditEvent, outbox *OutboxPayload) error {
	if err := appendRefusal(ctx, q, audit); err != nil {
		return err
	}
	eventID, err := ids.New[ids.Any]()
	if err != nil {
		return err
	}
	var attempt any
	if outbox != nil && !outbox.Attempt.IsZero() {
		attempt = outbox.Attempt.String()
	}
	if _, err := q.Exec(ctx, `
		insert into durable_event (tenant_id, event_id, attempt_id, record_kind, record_id, event, from_state, to_state, version, occurred_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		change.Tenant.String(), eventID.String(), attempt,
		string(change.Record), change.RecordID.String(), change.Event, change.From, textOrNil(change.To), int64(change.Version), audit.OccurredAt,
	); err != nil {
		return err
	}
	if outbox == nil {
		return nil
	}
	if outbox.ID.IsZero() || len(outbox.Body) == 0 {
		return lifecycle.ErrInvalid
	}
	_, err = q.Exec(ctx, `
		insert into outbox (tenant_id, outbox_id, attempt_id, record_kind, record_id, event, payload)
		values ($1, $2, $3, $4, $5, $6, $7)`,
		change.Tenant.String(), outbox.ID.String(), attempt,
		string(change.Record), change.RecordID.String(), change.Event, string(outbox.Body),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrPayloadMismatch
		}
		return err
	}
	return nil
}

// appendRefusal records refusal audit evidence without durable or outbox
// rows. Durable keys are one row per record version, so a refusal at an
// already-committed version must not compete for them; the audit row is the
// evidence, and nothing downstream dispatches a refusal.
func appendRefusal(ctx context.Context, q Querier, audit lifecycle.AuditEvent) error {
	if err := audit.Validate(); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		insert into audit_event (tenant_id, audit_id, record_kind, record_id, actor_kind, principal_id, provider_id, event, from_state, to_state, version, outcome, reason, occurred_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		audit.Tenant.String(), audit.ID.String(), string(audit.Record), audit.RecordID.String(),
		string(audit.Actor.Kind), uuidOrNil(audit.Actor.Principal), uuidOrNil(audit.Actor.Provider),
		audit.Event, audit.From, textOrNil(audit.To), int64(audit.Version), string(audit.Outcome), textOrNil(string(audit.Reason)), audit.OccurredAt,
	)
	return err
}

// insertReturning runs an INSERT that must never fail the transaction on a
// key conflict: ON CONFLICT DO NOTHING suppresses the violation (a plain
// failed statement would abort the whole transaction in PostgreSQL) and
// RETURNING reports whether the row was written. Conflicts load the stored
// row for replay comparison at the call site.
func insertReturning(ctx context.Context, q Querier, stmt string, args ...any) (bool, error) {
	var inserted bool
	err := q.QueryRow(ctx, stmt+` on conflict do nothing returning true`, args...).Scan(&inserted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return inserted, nil
}

// findDurable returns the event and target state recorded for one version of
// a record. It backs idempotent replay: a command that finds its own version
// already committed returns the stored outcome instead of writing again.
func findDurable(ctx context.Context, q Querier, tenant ids.TenantID, record lifecycle.RecordKind, recordID ids.AnyID, version uint64) (event string, to string, found bool, err error) {
	err = q.QueryRow(ctx, `
		select event, coalesce(to_state, '') from durable_event
		where tenant_id = $1 and record_kind = $2 and record_id = $3 and version = $4`,
		tenant.String(), string(record), recordID.String(), int64(version),
	).Scan(&event, &to)
	if err != nil {
		if isNoRows(err) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return event, to, true, nil
}
