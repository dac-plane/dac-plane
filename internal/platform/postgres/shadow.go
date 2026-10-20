package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// RecordShadowObservation records only a digest of a bound observation, never
// its potentially secret-bearing payload. The caller holds the Attempt lock,
// serializing identity allocation and replay across ingestion and recovery.
func RecordShadowObservation(ctx context.Context, u *UnitOfWork, bound contracts.BoundEvent, at time.Time) error {
	event := bound.Event()
	if event.Body.Rows != nil {
		// Replay flags, batching and delivery timestamps are not new logical
		// observations. Hash each entry/tombstone with its authenticated scope.
		rows := event.Body.Rows
		for _, entry := range rows.Entries {
			event.Body = contracts.StreamFrame{Kind: contracts.StreamRows, Rows: &contracts.Rows{Entries: []contracts.Entry{entry}}}
			if err := recordShadowObservation(ctx, u, event, at); err != nil {
				return err
			}
		}
		for _, tombstone := range rows.Tombstones {
			event.Body = contracts.StreamFrame{Kind: contracts.StreamRows, Rows: &contracts.Rows{Tombstones: []contracts.Tombstone{tombstone}}}
			if err := recordShadowObservation(ctx, u, event, at); err != nil {
				return err
			}
		}
		return nil
	}
	if event.Body.Kind == contracts.StreamHeartbeat || event.Body.Kind == contracts.StreamConnected {
		return nil
	}
	return recordShadowObservation(ctx, u, event, at)
}

func recordShadowObservation(ctx context.Context, u *UnitOfWork, event contracts.Event, at time.Time) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	digest := identity.DigestOf(raw)
	var existing string
	err = u.QueryRow(ctx, `select shadow_id::text from shadow_result where tenant_id=$1 and attempt_id=$2 and summary='provider observation' and evidence_digest=$3 limit 1`,
		event.Tenant.String(), event.Attempt.String(), string(digest)).Scan(&existing)
	if err == nil {
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	id, err := ids.New[ids.Any]()
	if err != nil {
		return err
	}
	_, _, err = RecordShadowResult(ctx, u, ShadowResult{ID: id, Tenant: event.Tenant, Attempt: event.Attempt, CapturedAt: at,
		Summary: "provider observation", Comparison: "non-authoritative", Digest: digest})
	return err
}

// ObserveHarness gates observations under the same lock as qualification
// changes. Missing/off qualification cannot authorize product mutation.
// The caller holds the Attempt/Task scope before acquiring this lock.
func ObserveHarness(ctx context.Context, u *UnitOfWork, bound contracts.BoundEvent, at time.Time) (bool, error) {
	event := bound.Event()
	h, err := GetHarnessForUpdate(ctx, u, event.Tenant, event.Provider)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if h.Mode == lifecycle.HarnessShadow {
		return false, RecordShadowObservation(ctx, u, bound, at)
	}
	return h.Mode == lifecycle.HarnessLive, nil
}

// ShadowResult is one durable shadow-run comparison. Shadow harnesses record
// here and nowhere else: the table is append-only, and no authoritative
// transition reads it.
type ShadowResult struct {
	ID         ids.AnyID
	Tenant     ids.TenantID
	Attempt    ids.AttemptID
	CapturedAt time.Time
	Summary    string
	Comparison string
	Digest     identity.Digest
}

// RecordShadowResult appends one shadow-run comparison. Re-recording the same
// shadow identity is a replay only when every field matches; anything else is
// a payload mismatch.
func RecordShadowResult(ctx context.Context, u *UnitOfWork, result ShadowResult) (ShadowResult, bool, error) {
	if result.ID.IsZero() || result.Tenant.IsZero() || result.Attempt.IsZero() {
		return ShadowResult{}, false, lifecycle.ErrInvalid
	}
	if result.Summary == "" || result.CapturedAt.IsZero() {
		return ShadowResult{}, false, lifecycle.ErrInvalid
	}
	if err := result.Digest.Validate(); err != nil {
		return ShadowResult{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into shadow_result (tenant_id, shadow_id, attempt_id, captured_at, summary, comparison_summary, evidence_digest)
		values ($1, $2, $3, $4, $5, $6, $7)`,
		result.Tenant.String(), result.ID.String(), result.Attempt.String(), result.CapturedAt,
		result.Summary, result.Comparison, string(result.Digest),
	)
	if err != nil {
		return ShadowResult{}, false, err
	}
	if inserted {
		return result, false, nil
	}
	stored, loadErr := GetShadowResult(ctx, u, result.Tenant, result.ID)
	return replayResult(ShadowResult{}, stored, loadErr, shadowsEqual(stored, result))
}

// GetShadowResult loads one shadow result by tenant and id.
func GetShadowResult(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AnyID) (ShadowResult, error) {
	var result ShadowResult
	var idText, tenantText, attemptText, summary, comparison, digestText string
	err := q.QueryRow(ctx, `
		select shadow_id::text, tenant_id::text, attempt_id::text, captured_at, summary, comparison_summary, evidence_digest
		from shadow_result where tenant_id = $1 and shadow_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &attemptText, &result.CapturedAt, &summary, &comparison, &digestText)
	if err != nil {
		if isNoRows(err) {
			return ShadowResult{}, ErrNotFound
		}
		return ShadowResult{}, err
	}
	var parseErr error
	if result.ID, parseErr = parseID[ids.Any](`shadow_result.shadow_id`, idText); parseErr != nil {
		return ShadowResult{}, parseErr
	}
	if result.Tenant, parseErr = parseID[ids.Tenant](`shadow_result.tenant_id`, tenantText); parseErr != nil {
		return ShadowResult{}, parseErr
	}
	if result.Attempt, parseErr = parseID[ids.Attempt](`shadow_result.attempt_id`, attemptText); parseErr != nil {
		return ShadowResult{}, parseErr
	}
	result.Summary, result.Comparison, result.Digest = summary, comparison, identity.Digest(digestText)
	if err := result.Digest.Validate(); err != nil {
		return ShadowResult{}, err
	}
	return result, nil
}

// shadowsEqual compares shadow results field-wise, tolerating the
// storage timestamp precision like every other replay comparator.
func shadowsEqual(a, b ShadowResult) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Attempt == b.Attempt &&
		sameInstant(a.CapturedAt, b.CapturedAt) && a.Summary == b.Summary &&
		a.Comparison == b.Comparison && a.Digest == b.Digest
}

// ListShadowResults returns one attempt's shadow comparisons oldest first.
func ListShadowResults(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) ([]ShadowResult, error) {
	rows, err := q.Query(ctx, `
		select shadow_id::text from shadow_result
		where tenant_id = $1 and attempt_id = $2 order by captured_at`,
		tenant.String(), attempt.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowResult
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, err
		}
		id, err := parseID[ids.Any](`shadow_result.shadow_id`, idText)
		if err != nil {
			return nil, err
		}
		result, err := GetShadowResult(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	return out, rows.Err()
}
