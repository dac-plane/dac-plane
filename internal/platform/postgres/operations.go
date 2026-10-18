// Package postgres Operation repositories persist provider operations, bindings, turn ends,
// environment observations and stream cursors. Observations and cursors are
// latest-state rows: replays compare against the stored row instead of
// writing again.
package postgres

import (
	"context"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// GetOperation loads a provider operation by tenant and id.
func GetOperation(ctx context.Context, q Querier, tenant ids.TenantID, id ids.OperationID) (lifecycle.ProviderOperation, error) {
	var operation lifecycle.ProviderOperation
	var state, idText, tenantText, attemptText, providerText string
	err := q.QueryRow(ctx, `
		select operation_id::text, tenant_id::text, attempt_id::text, provider_id::text, state, version, created_at, updated_at
		from provider_operation where tenant_id = $1 and operation_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &attemptText, &providerText, &state, &operation.Version, &operation.CreatedAt, &operation.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ProviderOperation{}, ErrNotFound
		}
		return lifecycle.ProviderOperation{}, err
	}
	var parseErr error
	if operation.ID, parseErr = parseID[ids.Operation](`provider_operation.operation_id`, idText); parseErr != nil {
		return lifecycle.ProviderOperation{}, parseErr
	}
	if operation.Tenant, parseErr = parseID[ids.Tenant](`provider_operation.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.ProviderOperation{}, parseErr
	}
	if operation.Attempt, parseErr = parseID[ids.Attempt](`provider_operation.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.ProviderOperation{}, parseErr
	}
	if operation.Provider, parseErr = parseID[ids.Provider](`provider_operation.provider_id`, providerText); parseErr != nil {
		return lifecycle.ProviderOperation{}, parseErr
	}
	operation.State = lifecycle.OperationState(state)
	if err := operation.Validate(); err != nil {
		return lifecycle.ProviderOperation{}, err
	}
	return operation, nil
}

// InsertOperation persists a recorded operation with replay comparison: the
// operation id is the stable idempotency key across redelivery.
func InsertOperation(ctx context.Context, u *UnitOfWork, operation lifecycle.ProviderOperation) (lifecycle.ProviderOperation, bool, error) {
	if err := operation.Validate(); err != nil {
		return lifecycle.ProviderOperation{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into provider_operation (tenant_id, operation_id, attempt_id, provider_id, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		operation.Tenant.String(), operation.ID.String(), operation.Attempt.String(), operation.Provider.String(),
		string(operation.State), int64(operation.Version), operation.CreatedAt, operation.UpdatedAt,
	)
	if err != nil {
		return lifecycle.ProviderOperation{}, false, err
	}
	if inserted {
		return operation, false, nil
	}
	stored, loadErr := GetOperation(ctx, u, operation.Tenant, operation.ID)
	if isNotFound(loadErr) {
		return lifecycle.ProviderOperation{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.ProviderOperation{}, false, loadErr
	}
	if stored.Tenant != operation.Tenant || stored.Attempt != operation.Attempt ||
		stored.Provider != operation.Provider || stored.State != operation.State ||
		stored.Version != operation.Version {
		return lifecycle.ProviderOperation{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// ApplyOperation performs an operation event at the expected version with
// atomic history, sharing the stale, replay and refusal rules of ApplyTask.
func ApplyOperation(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.OperationID, event lifecycle.OperationEvent, target lifecycle.OperationState, tr Transition) (lifecycle.ProviderOperation, bool, error) {
	var operation lifecycle.ProviderOperation
	var state, idText, tenantText, attemptText, providerText string
	err := u.QueryRow(ctx, `
		select operation_id::text, tenant_id::text, attempt_id::text, provider_id::text, state, version, created_at, updated_at
		from provider_operation where tenant_id = $1 and operation_id = $2 for update`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &attemptText, &providerText, &state, &operation.Version, &operation.CreatedAt, &operation.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ProviderOperation{}, false, ErrNotFound
		}
		return lifecycle.ProviderOperation{}, false, err
	}
	var parseErr error
	if operation.ID, parseErr = parseID[ids.Operation](`provider_operation.operation_id`, idText); parseErr != nil {
		return lifecycle.ProviderOperation{}, false, parseErr
	}
	if operation.Tenant, parseErr = parseID[ids.Tenant](`provider_operation.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.ProviderOperation{}, false, parseErr
	}
	if operation.Attempt, parseErr = parseID[ids.Attempt](`provider_operation.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.ProviderOperation{}, false, parseErr
	}
	if operation.Provider, parseErr = parseID[ids.Provider](`provider_operation.provider_id`, providerText); parseErr != nil {
		return lifecycle.ProviderOperation{}, false, parseErr
	}
	operation.State = lifecycle.OperationState(state)
	if err := operation.Validate(); err != nil {
		return lifecycle.ProviderOperation{}, false, err
	}
	if operation.Version != tr.ExpectedVersion {
		if operation.Version == tr.ExpectedVersion+1 {
			replayed, replayErr := replayIfCommitted(ctx, u, ReplayCheck{Tenant: tenant, Record: lifecycle.RecordOperation, RecordID: operation.ID.Untyped(), Committed: operation.Version, Event: string(event), Target: string(target)})
			if replayErr != nil {
				return lifecycle.ProviderOperation{}, false, replayErr
			}
			if replayed {
				return operation, true, nil
			}
		}
		return lifecycle.ProviderOperation{}, false, ErrStaleVersion
	}
	next, change, applyErr := operation.Apply(event, tr.ExpectedVersion, target, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.ProviderOperation{}, false, histErr
			}
		}
		return operation, false, applyErr
	}
	if err := execVersionedUpdate(ctx, u, `
		update provider_operation set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and operation_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	); err != nil {
		return lifecycle.ProviderOperation{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.ProviderOperation{}, false, err
	}
	return next, false, nil
}

// ListBindings returns the bindings for one native resource so callers can
// run the pure lifecycle.CheckBinding before inserting.
func ListBindings(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID, kind lifecycle.ResourceKind, nativeRef string) ([]lifecycle.Binding, error) {
	rows, err := q.Query(ctx, `
		select attempt_id::text, operation_id::text, session_ref, bound_at
		from binding where tenant_id = $1 and provider_id = $2 and kind = $3 and native_ref = $4`,
		tenant.String(), provider.String(), string(kind), nativeRef,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lifecycle.Binding
	for rows.Next() {
		var binding lifecycle.Binding
		var attemptText, operationText string
		var sessionText *string
		if err := rows.Scan(&attemptText, &operationText, &sessionText, &binding.BoundAt); err != nil {
			return nil, err
		}
		var parseErr error
		binding.Tenant = tenant
		binding.Provider = provider
		binding.Kind = kind
		binding.NativeRef = nativeRef
		if binding.Attempt, parseErr = parseID[ids.Attempt](`binding.attempt_id`, attemptText); parseErr != nil {
			return nil, parseErr
		}
		if binding.Operation, parseErr = parseID[ids.Operation](`binding.operation_id`, operationText); parseErr != nil {
			return nil, parseErr
		}
		if sessionText != nil {
			binding.SessionRef = *sessionText
		}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

// InsertBinding persists a binding. The natural key is the native resource:
// a conflict loads the stored row for replay comparison, so a resource bound
// to another attempt surfaces as a mismatch, never a silent rebind.
func InsertBinding(ctx context.Context, u *UnitOfWork, binding lifecycle.Binding) (lifecycle.Binding, bool, error) {
	if err := binding.Validate(); err != nil {
		return lifecycle.Binding{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into binding (tenant_id, attempt_id, provider_id, operation_id, kind, native_ref, session_ref, bound_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		binding.Tenant.String(), binding.Attempt.String(), binding.Provider.String(), binding.Operation.String(),
		string(binding.Kind), binding.NativeRef, textOrNil(binding.SessionRef), binding.BoundAt,
	)
	if err != nil {
		return lifecycle.Binding{}, false, err
	}
	if inserted {
		return binding, false, nil
	}
	existing, listErr := ListBindings(ctx, u, binding.Tenant, binding.Provider, binding.Kind, binding.NativeRef)
	if listErr != nil {
		return lifecycle.Binding{}, false, listErr
	}
	for _, stored := range existing {
		if stored.Attempt == binding.Attempt && stored.Operation == binding.Operation && stored.SessionRef == binding.SessionRef {
			return stored, true, nil
		}
	}
	return lifecycle.Binding{}, false, ErrPayloadMismatch
}

// InsertTurnEnd persists a turn-end observation. A turn ends once: the
// natural key replays identical reports and mismatches changed ones.
func InsertTurnEnd(ctx context.Context, u *UnitOfWork, end lifecycle.TurnEnd) (lifecycle.TurnEnd, bool, error) {
	if err := end.Validate(); err != nil {
		return lifecycle.TurnEnd{}, false, err
	}
	var code, summary, details any
	if end.Failure != nil {
		code, summary, details = string(end.Failure.Code), end.Failure.Summary, textOrNil(string(end.Failure.DetailsDigest))
	}
	inserted, err := insertReturning(ctx, u, `
		insert into turn_end (tenant_id, attempt_id, provider_id, session_ref, turn_ref, outcome, failure_code, failure_summary, failure_details_digest, ended_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		end.Tenant.String(), end.Attempt.String(), end.Provider.String(), end.SessionRef, end.TurnRef,
		string(end.Outcome), code, summary, details, end.EndedAt,
	)
	if err != nil {
		return lifecycle.TurnEnd{}, false, err
	}
	if inserted {
		return end, false, nil
	}
	stored, loadErr := GetTurnEnd(ctx, u, end.Tenant, end.Provider, end.SessionRef, end.TurnRef)
	if isNotFound(loadErr) {
		return lifecycle.TurnEnd{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.TurnEnd{}, false, loadErr
	}
	if !turnEndsEqual(stored, end) {
		return lifecycle.TurnEnd{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetTurnEnd loads the recorded end of one turn.
func GetTurnEnd(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID, session, turn string) (lifecycle.TurnEnd, error) {
	var end lifecycle.TurnEnd
	var outcome, attemptText string
	var code, summary, details *string
	err := q.QueryRow(ctx, `
		select attempt_id::text, outcome, failure_code, failure_summary, failure_details_digest, ended_at
		from turn_end where tenant_id = $1 and provider_id = $2 and session_ref = $3 and turn_ref = $4`,
		tenant.String(), provider.String(), session, turn,
	).Scan(&attemptText, &outcome, &code, &summary, &details, &end.EndedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.TurnEnd{}, ErrNotFound
		}
		return lifecycle.TurnEnd{}, err
	}
	var parseErr error
	end.Tenant, end.Provider = tenant, provider
	end.SessionRef, end.TurnRef = session, turn
	if end.Attempt, parseErr = parseID[ids.Attempt](`turn_end.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.TurnEnd{}, parseErr
	}
	end.Outcome = lifecycle.TurnOutcome(outcome)
	if code != nil {
		failure := lifecycle.TurnFailure{Code: lifecycle.TurnFailureCode(*code)}
		if summary != nil {
			failure.Summary = *summary
		}
		if details != nil {
			failure.DetailsDigest = identityDigest(*details)
		}
		end.Failure = &failure
	}
	if err := end.Validate(); err != nil {
		return lifecycle.TurnEnd{}, err
	}
	return end, nil
}

func turnEndsEqual(a, b lifecycle.TurnEnd) bool {
	if a.Tenant != b.Tenant || a.Attempt != b.Attempt || a.Provider != b.Provider ||
		a.SessionRef != b.SessionRef || a.TurnRef != b.TurnRef || a.Outcome != b.Outcome ||
		!sameInstant(a.EndedAt, b.EndedAt) {
		return false
	}
	if (a.Failure == nil) != (b.Failure == nil) {
		return false
	}
	return a.Failure == nil || *a.Failure == *b.Failure
}

// UpsertObservation persists the latest observation of a resource. Newer
// sequences replace older ones; replays and out-of-order reports keep the
// stored row and report replayed.
func UpsertObservation(ctx context.Context, u *UnitOfWork, observation lifecycle.EnvironmentObservation) (lifecycle.EnvironmentObservation, bool, error) {
	if err := observation.Validate(); err != nil {
		return lifecycle.EnvironmentObservation{}, false, err
	}
	_, err := u.Exec(ctx, `
		insert into environment_observation (tenant_id, attempt_id, provider_id, resource_ref, state, sequence, observed_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (tenant_id, provider_id, resource_ref) do update set
			attempt_id = excluded.attempt_id, state = excluded.state,
			sequence = excluded.sequence, observed_at = excluded.observed_at
		where environment_observation.sequence < excluded.sequence`,
		observation.Tenant.String(), observation.Attempt.String(), observation.Provider.String(),
		observation.ResourceRef, string(observation.State), int64(observation.Sequence), observation.ObservedAt,
	)
	if err != nil {
		return lifecycle.EnvironmentObservation{}, false, err
	}
	stored, loadErr := GetObservation(ctx, u, observation.Tenant, observation.Provider, observation.ResourceRef)
	if loadErr != nil {
		return lifecycle.EnvironmentObservation{}, false, loadErr
	}
	return stored, stored.Sequence != observation.Sequence || stored.State != observation.State, nil
}

// GetObservation loads the latest observation of a resource.
func GetObservation(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID, resource string) (lifecycle.EnvironmentObservation, error) {
	var observation lifecycle.EnvironmentObservation
	var state, attemptText string
	var sequence int64
	err := q.QueryRow(ctx, `
		select attempt_id::text, state, sequence, observed_at
		from environment_observation where tenant_id = $1 and provider_id = $2 and resource_ref = $3`,
		tenant.String(), provider.String(), resource,
	).Scan(&attemptText, &state, &sequence, &observation.ObservedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.EnvironmentObservation{}, ErrNotFound
		}
		return lifecycle.EnvironmentObservation{}, err
	}
	var parseErr error
	observation.Tenant, observation.Provider = tenant, provider
	observation.ResourceRef = resource
	if observation.Attempt, parseErr = parseID[ids.Attempt](`environment_observation.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.EnvironmentObservation{}, parseErr
	}
	observation.State = lifecycle.RunState(state)
	observation.Sequence = uint64(sequence)
	if err := observation.Validate(); err != nil {
		return lifecycle.EnvironmentObservation{}, err
	}
	return observation, nil
}

// UpsertCursor persists ingestion progress for a stream. Advancement is
// computed by contracts.Ingest in memory; the store writes the result.
func UpsertCursor(ctx context.Context, u *UnitOfWork, cursor contracts.Cursor) (contracts.Cursor, error) {
	if err := cursor.Validate(); err != nil {
		return contracts.Cursor{}, err
	}
	_, err := u.Exec(ctx, `
		insert into stream_cursor (tenant_id, attempt_id, operation_id, stream, generation, after_updated_sequence, updated_at)
		values ($1, $2, $3, $4, $5, $6, now())
		on conflict (tenant_id, operation_id, stream) do update set
			generation = excluded.generation, after_updated_sequence = excluded.after_updated_sequence, updated_at = now()`,
		cursor.Tenant.String(), cursor.Attempt.String(), cursor.Operation.String(),
		cursor.Stream, int64(cursor.Generation), int64(cursor.After),
	)
	if err != nil {
		return contracts.Cursor{}, err
	}
	return cursor, nil
}

// GetCursor loads ingestion progress for a stream.
func GetCursor(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID, stream string) (contracts.Cursor, error) {
	var cursor contracts.Cursor
	var tenantText, attemptText, operationText string
	var generation, after int64
	err := q.QueryRow(ctx, `
		select tenant_id::text, attempt_id::text, operation_id::text, generation, after_updated_sequence
		from stream_cursor where tenant_id = $1 and operation_id = $2 and stream = $3`,
		tenant.String(), operation.String(), stream,
	).Scan(&tenantText, &attemptText, &operationText, &generation, &after)
	if err != nil {
		if isNoRows(err) {
			return contracts.Cursor{}, ErrNotFound
		}
		return contracts.Cursor{}, err
	}
	var parseErr error
	if cursor.Tenant, parseErr = parseID[ids.Tenant](`stream_cursor.tenant_id`, tenantText); parseErr != nil {
		return contracts.Cursor{}, parseErr
	}
	if cursor.Attempt, parseErr = parseID[ids.Attempt](`stream_cursor.attempt_id`, attemptText); parseErr != nil {
		return contracts.Cursor{}, parseErr
	}
	if cursor.Operation, parseErr = parseID[ids.Operation](`stream_cursor.operation_id`, operationText); parseErr != nil {
		return contracts.Cursor{}, parseErr
	}
	cursor.Stream = stream
	cursor.Generation, cursor.After = uint64(generation), uint64(after)
	if err := cursor.Validate(); err != nil {
		return contracts.Cursor{}, err
	}
	return cursor, nil
}

// ListCursors returns every cursor of an attempt for restart resume.
func ListCursors(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) ([]contracts.Cursor, error) {
	rows, err := q.Query(ctx, `
		select operation_id::text, stream, generation, after_updated_sequence
		from stream_cursor where tenant_id = $1 and attempt_id = $2 order by stream`,
		tenant.String(), attempt.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Cursor
	for rows.Next() {
		var cursor contracts.Cursor
		var operationText, stream string
		var generation, after int64
		if err := rows.Scan(&operationText, &stream, &generation, &after); err != nil {
			return nil, err
		}
		var parseErr error
		cursor.Tenant, cursor.Attempt = tenant, attempt
		if cursor.Operation, parseErr = parseID[ids.Operation](`stream_cursor.operation_id`, operationText); parseErr != nil {
			return nil, parseErr
		}
		cursor.Stream = stream
		cursor.Generation, cursor.After = uint64(generation), uint64(after)
		if err := cursor.Validate(); err != nil {
			return nil, err
		}
		out = append(out, cursor)
	}
	return out, rows.Err()
}

// identityDigest casts stored digest text without revalidating here; the
// record Validate called by every loader enforces the shape.
func identityDigest(s string) identity.Digest {
	return identity.Digest(s)
}

// LockCursor returns a stream's cursor locked for the rest of the unit of
// work, creating it from start when the stream has none. Ingestion of one
// stream is serialized by this lock, so a slower ingest of an older frame can
// never move the cursor backwards.
func LockCursor(ctx context.Context, u *UnitOfWork, start contracts.Cursor) (contracts.Cursor, error) {
	if err := start.Validate(); err != nil {
		return contracts.Cursor{}, err
	}
	if _, err := u.Exec(ctx, `
		insert into stream_cursor (tenant_id, attempt_id, operation_id, stream, generation, after_updated_sequence, updated_at)
		values ($1, $2, $3, $4, $5, $6, now())
		on conflict (tenant_id, operation_id, stream) do nothing`,
		start.Tenant.String(), start.Attempt.String(), start.Operation.String(),
		start.Stream, int64(start.Generation), int64(start.After),
	); err != nil {
		return contracts.Cursor{}, err
	}
	if _, err := u.Exec(ctx, `
		select 1 from stream_cursor where tenant_id = $1 and operation_id = $2 and stream = $3 for update`,
		start.Tenant.String(), start.Operation.String(), start.Stream,
	); err != nil {
		return contracts.Cursor{}, err
	}
	return GetCursor(ctx, u, start.Tenant, start.Operation, start.Stream)
}
