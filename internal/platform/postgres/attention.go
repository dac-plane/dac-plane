// Package postgres Attention record helpers for the #24 attention, decision and cancellation
// paths: the native correlation the ingestion boundary resolves, the
// attempt-scoped listings the decide and cancel commands need, and the
// lookup a give-up reconciliation uses to tell an environment stop from a
// never-bound environment.
package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// AttentionNative binds the native request reference a provider raised to
// the attention request the product owns, under the provider that reported
// it. The ingestion boundary resolves provider-native keys through this
// binding and never trusts them as product identity; for an action approval
// the row also carries the governed action class the observation named, the
// provenance a later decision needs to derive its one-time grant.
type AttentionNative struct {
	Tenant      ids.TenantID
	Provider    ids.ProviderID
	NativeRef   string
	Attention   ids.AttentionID
	ActionClass identity.ActionClass
	CreatedAt   time.Time
}

// LockAttentionNative serializes creation of one provider-native attention
// reference for the lifetime of the current transaction. The natural-key
// lookup and all lifecycle changes that follow it must run while this lock is
// held, otherwise two different Attempts can both enter a wait before the
// unique constraint chooses which request owns the reference.
func LockAttentionNative(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, provider ids.ProviderID, nativeRef string) error {
	key := tenant.String() + ":" + provider.String() + ":" + nativeRef
	_, err := u.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// InsertAttentionNative persists the correlation with replay comparison on
// its natural key: a native reference raised twice resolves the same binding,
// and a native reference bound to another attention surfaces as a mismatch,
// never a silent rebind.
func InsertAttentionNative(ctx context.Context, u *UnitOfWork, native AttentionNative) (AttentionNative, bool, error) {
	inserted, err := insertReturning(ctx, u, `
		insert into attention_native (tenant_id, provider_id, native_ref, attention_id, action_class, created_at)
		values ($1, $2, $3, $4, $5, $6)`,
		native.Tenant.String(), native.Provider.String(), native.NativeRef,
		native.Attention.String(), textOrNil(string(native.ActionClass)), native.CreatedAt,
	)
	if err != nil {
		return AttentionNative{}, false, err
	}
	if inserted {
		return native, false, nil
	}
	stored, loadErr := GetAttentionNative(ctx, u, native.Tenant, native.Provider, native.NativeRef)
	if isNotFound(loadErr) {
		return AttentionNative{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return AttentionNative{}, false, loadErr
	}
	if stored.Attention != native.Attention || stored.ActionClass != native.ActionClass {
		return AttentionNative{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetAttentionNative resolves one provider-native request reference.
func GetAttentionNative(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID, nativeRef string) (AttentionNative, error) {
	var native AttentionNative
	var attentionText, classText *string
	err := q.QueryRow(ctx, `
		select attention_id::text, action_class, created_at
		from attention_native where tenant_id = $1 and provider_id = $2 and native_ref = $3`,
		tenant.String(), provider.String(), nativeRef,
	).Scan(&attentionText, &classText, &native.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return AttentionNative{}, ErrNotFound
		}
		return AttentionNative{}, err
	}
	if native.Attention, err = parseID[ids.Attention](`attention_native.attention_id`, *attentionText); err != nil {
		return AttentionNative{}, err
	}
	if classText != nil {
		native.ActionClass = identity.ActionClass(*classText)
	}
	native.Tenant, native.Provider, native.NativeRef = tenant, provider, nativeRef
	return native, nil
}

// GetAttentionNativeForAttention reads the correlation of one attention.
func GetAttentionNativeForAttention(ctx context.Context, q Querier, tenant ids.TenantID, attention ids.AttentionID) (AttentionNative, error) {
	var native AttentionNative
	var providerText, classText *string
	err := q.QueryRow(ctx, `
		select provider_id::text, native_ref, action_class, created_at
		from attention_native where tenant_id = $1 and attention_id = $2`,
		tenant.String(), attention.String(),
	).Scan(&providerText, &native.NativeRef, &classText, &native.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return AttentionNative{}, ErrNotFound
		}
		return AttentionNative{}, err
	}
	if native.Provider, err = parseID[ids.Provider](`attention_native.provider_id`, *providerText); err != nil {
		return AttentionNative{}, err
	}
	if classText != nil {
		native.ActionClass = identity.ActionClass(*classText)
	}
	native.Tenant, native.Attention = tenant, attention
	return native, nil
}

// ListOpenAttentionByAttempt returns the attempt's unconsumed attention
// requests, oldest first. Consumed, expired, cancelled and superseded
// requests are closed waits.
func ListOpenAttentionByAttempt(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) ([]lifecycle.AttentionRequest, error) {
	var texts []string
	rows, err := q.Query(ctx, `
		select attention_id::text from attention_request
		where tenant_id = $1 and attempt_id = $2 and state = any($3)
		order by created_at, attention_id`,
		tenant.String(), attempt.String(), stringsAny(lifecycle.UnconsumedAttentionStates()),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	requests := make([]lifecycle.AttentionRequest, 0, len(texts))
	for _, text := range texts {
		id, err := ids.Parse[ids.Attention](text)
		if err != nil {
			return nil, err
		}
		request, err := GetAttention(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

// ListGrantsByAttempt returns the attempt's grants that still authorize:
// neither revoked nor retired, oldest first.
func ListGrantsByAttempt(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) ([]identity.AuthorityGrant, error) {
	var texts []string
	rows, err := q.Query(ctx, `
		select grant_id::text from authority_grant
		where tenant_id = $1 and attempt_id = $2 and revoked_at is null and retired_at is null
		order by issued_at, grant_id`,
		tenant.String(), attempt.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	grants := make([]identity.AuthorityGrant, 0, len(texts))
	for _, text := range texts {
		id, err := ids.Parse[ids.Grant](text)
		if err != nil {
			return nil, err
		}
		grant, err := GetGrant(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, nil
}

// GetAttemptByTask loads the task's newest attempt. One Task delegates one
// Attempt; an attempt that never dispatched has none.
func GetAttemptByTask(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID) (lifecycle.Attempt, error) {
	var text string
	row := q.QueryRow(ctx, `
		select attempt_id::text from attempt
		where tenant_id = $1 and task_id = $2
		order by number desc, created_at desc limit 1`,
		tenant.String(), task.String(),
	)
	if err := row.Scan(&text); err != nil {
		if isNoRows(err) {
			return lifecycle.Attempt{}, ErrNotFound
		}
		return lifecycle.Attempt{}, err
	}
	id, err := ids.Parse[ids.Attempt](text)
	if err != nil {
		return lifecycle.Attempt{}, err
	}
	return GetAttempt(ctx, q, tenant, id)
}

// GetOperationByAttempt loads the newest runtime operation for the attempt,
// excluding governed actions. An attempt that never dispatched has none.
func GetOperationByAttempt(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) (lifecycle.ProviderOperation, error) {
	row := q.QueryRow(ctx, `
		select operation_id::text from provider_operation p
		where tenant_id = $1 and attempt_id = $2
		and not exists (select 1 from governed_action g where g.tenant_id=p.tenant_id and g.operation_id=p.operation_id)
		and not exists (select 1 from artifact_stage_operation s where s.tenant_id=p.tenant_id and s.operation_id=p.operation_id)
		and not exists (select 1 from migration_item m where m.tenant_id=p.tenant_id and p.operation_id in (m.quiesce_operation_id,m.restart_operation_id,m.cleanup_operation_id))
		order by created_at desc, version desc limit 1`,
		tenant.String(), attempt.String(),
	)
	var text string
	if err := row.Scan(&text); err != nil {
		if isNoRows(err) {
			return lifecycle.ProviderOperation{}, ErrNotFound
		}
		return lifecycle.ProviderOperation{}, err
	}
	id, err := ids.Parse[ids.Operation](text)
	if err != nil {
		return lifecycle.ProviderOperation{}, err
	}
	return GetOperation(ctx, q, tenant, id)
}

// HasEnvironmentBinding reports whether the attempt ever bound an
// environment resource, which give-up reconciliation needs: a dispatch that
// never reached the provider cannot have created one, while a possibly
// delivered dispatch must wait for real observations on either stop.
func HasEnvironmentBinding(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `
		select exists (
			select 1 from binding
			where tenant_id = $1 and attempt_id = $2 and kind = $3
		)`,
		tenant.String(), attempt.String(), string(lifecycle.ResourceEnvironmentResource),
	).Scan(&found)
	return found, err
}

// EnvironmentStopped reports whether every environment resource bound to the
// Attempt has a latest stored observation that is definitely stopped. No
// binding is not stop evidence: a start may still be in flight.
func EnvironmentStopped(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) (bool, error) {
	var stopped bool
	err := q.QueryRow(ctx, `
		select count(*) > 0
		   and count(*) = count(o.resource_ref)
		   and coalesce(bool_and(o.state = any($3)), false)
		from binding b
		left join environment_observation o
		  on o.tenant_id = b.tenant_id
		 and o.attempt_id = b.attempt_id
		 and o.provider_id = b.provider_id
		 and o.resource_ref = b.native_ref
		where b.tenant_id = $1 and b.attempt_id = $2 and b.kind = $4`,
		tenant.String(), attempt.String(), stringsAny([]lifecycle.RunState{lifecycle.RunAbsent, lifecycle.RunHibernated}),
		string(lifecycle.ResourceEnvironmentResource),
	).Scan(&stopped)
	return stopped, err
}

// stringsAny adapts typed state strings to a postgres any() argument.
func stringsAny[S ~string](states []S) []any {
	out := make([]any, len(states))
	for i, state := range states {
		out[i] = string(state)
	}
	return out
}
