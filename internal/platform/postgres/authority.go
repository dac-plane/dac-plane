// Package postgres Authority repositories persist policies, grants, uses and hook evidence.
// Generation fencing for grants lives in both layers: the pure
// identity.IssueGrant check and the check_grant_generation trigger, so a
// stale generation can never be persisted even if validation is bypassed.
package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// InsertTenant persists a tenant. Tenants are the root of every foreign key;
// creation is idempotent by id.
func InsertTenant(ctx context.Context, u *UnitOfWork, tenant ids.TenantID) error {
	if _, err := insertReturning(ctx, u, `insert into tenant (id) values ($1)`, tenant.String()); err != nil {
		return err
	}
	return nil
}

// ListTenants returns every tenant identity, oldest first. The dispatch
// loop ticks tenants sequentially; cross-tenant fairness is out of scope.
func ListTenants(ctx context.Context, q Querier) ([]ids.TenantID, error) {
	rows, err := q.Query(ctx, `select id::text from tenant order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ids.TenantID
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			return nil, err
		}
		id, err := parseID[ids.Tenant](`tenant.id`, idText)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertPrincipal persists a principal with replay comparison.
func InsertPrincipal(ctx context.Context, u *UnitOfWork, principal identity.Principal) error {
	if err := principal.Validate(); err != nil {
		return err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into principal (tenant_id, principal_id, kind) values ($1, $2, $3)`,
		principal.Tenant.String(), principal.ID.String(), string(principal.Kind),
	)
	if err != nil {
		return err
	}
	if inserted {
		return nil
	}
	stored, loadErr := GetPrincipal(ctx, u, principal.Tenant, principal.ID)
	if isNotFound(loadErr) {
		return ErrPayloadMismatch
	}
	if loadErr != nil {
		return loadErr
	}
	if stored != principal {
		return ErrPayloadMismatch
	}
	return nil
}

// GetPrincipal loads a principal by tenant and id.
func GetPrincipal(ctx context.Context, q Querier, tenant ids.TenantID, id ids.PrincipalID) (identity.Principal, error) {
	var principal identity.Principal
	var kind, idText, tenantText string
	err := q.QueryRow(ctx, `
		select principal_id::text, tenant_id::text, kind from principal where tenant_id = $1 and principal_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &kind)
	if err != nil {
		if isNoRows(err) {
			return identity.Principal{}, ErrNotFound
		}
		return identity.Principal{}, err
	}
	var parseErr error
	if principal.ID, parseErr = parseID[ids.Principal](`principal.principal_id`, idText); parseErr != nil {
		return identity.Principal{}, parseErr
	}
	if principal.Tenant, parseErr = parseID[ids.Tenant](`principal.tenant_id`, tenantText); parseErr != nil {
		return identity.Principal{}, parseErr
	}
	principal.Kind = identity.PrincipalKind(kind)
	if err := principal.Validate(); err != nil {
		return identity.Principal{}, err
	}
	return principal, nil
}

// GetPolicy loads a tenant's authority policy.
func GetPolicy(ctx context.Context, q Querier, tenant ids.TenantID) (identity.Policy, error) {
	var policy identity.Policy
	var generation int64
	var settingsText, ceilingText, deniesText *string
	err := q.QueryRow(ctx, `
		select generation, settings::text, ceiling::text, denies::text
		from policy where tenant_id = $1`,
		tenant.String(),
	).Scan(&generation, &settingsText, &ceilingText, &deniesText)
	if err != nil {
		if isNoRows(err) {
			return identity.Policy{}, ErrNotFound
		}
		return identity.Policy{}, err
	}
	policy.Tenant = tenant
	policy.Generation = uint64(generation)
	if err := unmarshalPolicyPart(`policy.settings`, settingsText, &policy.Settings); err != nil {
		return identity.Policy{}, err
	}
	if err := unmarshalPolicyPart(`policy.ceiling`, ceilingText, &policy.Ceiling); err != nil {
		return identity.Policy{}, err
	}
	if err := unmarshalPolicyPart(`policy.denies`, deniesText, &policy.Denies); err != nil {
		return identity.Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return identity.Policy{}, err
	}
	return policy, nil
}

func unmarshalPolicyPart[T any](column string, value *string, target *T) error {
	if value == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(*value), target); err != nil {
		return corruptRow(column, err)
	}
	return nil
}

// InsertPolicy persists a tenant's initial policy. Generation changes go
// through UpdatePolicyGeneration so concurrent writers serialize.
func InsertPolicy(ctx context.Context, u *UnitOfWork, policy identity.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	settings, err := marshalPolicyPart(policy.Settings)
	if err != nil {
		return err
	}
	ceiling, err := marshalPolicyPart(policy.Ceiling)
	if err != nil {
		return err
	}
	denies, err := marshalPolicyPart(policy.Denies)
	if err != nil {
		return err
	}
	_, err = u.Exec(ctx, `
		insert into policy (tenant_id, generation, settings, ceiling, denies)
		values ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb)`,
		policy.Tenant.String(), int64(policy.Generation), settings, ceiling, denies,
	)
	return err
}

func marshalPolicyPart(value any) (*string, error) {
	switch v := value.(type) {
	case map[identity.ActionClass]identity.Permission:
		if len(v) == 0 {
			return nil, nil
		}
	case []identity.DenyPolicy:
		if len(v) == 0 {
			return nil, nil
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	return &text, nil
}

// PolicyUpdate advances a tenant's policy, invalidating authority issued
// under earlier generations.
type PolicyUpdate struct {
	Tenant             ids.TenantID
	ExpectedGeneration uint64
	NextGeneration     uint64
	Settings           map[identity.ActionClass]identity.Permission
	Ceiling            map[identity.ActionClass]identity.Permission
	Denies             []identity.DenyPolicy
	At                 time.Time
}

// UpdatePolicyGeneration advances a tenant's policy, invalidating authority
// issued under earlier generations. Zero updated rows mean stale.
func UpdatePolicyGeneration(ctx context.Context, u *UnitOfWork, update PolicyUpdate) (identity.Policy, error) {
	settingsJSON, err := marshalPolicyPart(update.Settings)
	if err != nil {
		return identity.Policy{}, err
	}
	ceilingJSON, err := marshalPolicyPart(update.Ceiling)
	if err != nil {
		return identity.Policy{}, err
	}
	deniesJSON, err := marshalPolicyPart(update.Denies)
	if err != nil {
		return identity.Policy{}, err
	}
	if err := execVersionedUpdate(ctx, u, `
		update policy set generation = $2, settings = $3::jsonb, ceiling = $4::jsonb, denies = $5::jsonb, updated_at = $6
		where tenant_id = $1 and generation = $7`,
		update.Tenant.String(), int64(update.NextGeneration), settingsJSON, ceilingJSON, deniesJSON, update.At, int64(update.ExpectedGeneration),
	); err != nil {
		return identity.Policy{}, err
	}
	return GetPolicy(ctx, u, update.Tenant)
}

// InsertGrant persists an authority grant. The generation trigger refuses
// stale generations; callers still run identity.IssueGrant first so refusals
// carry typed denial reasons instead of database errors.
func InsertGrant(ctx context.Context, u *UnitOfWork, grant identity.AuthorityGrant) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into authority_grant (tenant_id, grant_id, attempt_id, provider_id, action, mode, operation_id, input_digest, scope_class, scope_target, granted_by, policy_generation, issued_at, expires_at, revoked_at, retired_at, decision_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		grant.Tenant.String(), grant.ID.String(), grant.Attempt.String(), grant.Provider.String(),
		string(grant.Action), string(grant.Mode), uuidOrNil(grant.Operation), textOrNil(string(grant.InputDigest)),
		textOrNil(string(grant.Scope.Class)), textOrNil(grant.Scope.Target), grant.GrantedBy.String(),
		int64(grant.PolicyGeneration), grant.IssuedAt, grant.ExpiresAt,
		timeOrNil(grant.RevokedAt), timeOrNil(grant.RetiredAt), uuidOrNil(grant.Decision),
	)
	if err != nil {
		return err
	}
	if inserted {
		return nil
	}
	stored, loadErr := GetGrant(ctx, u, grant.Tenant, grant.ID)
	if loadErr != nil {
		if isNotFound(loadErr) {
			return ErrPayloadMismatch
		}
		return loadErr
	}
	if !grantsEqualIssuance(stored, grant) {
		return ErrPayloadMismatch
	}
	return nil
}

// grantsEqualIssuance compares the issuance payload of two grants.
// Revocation and retirement are post-issuance mutations, not payload.
func grantsEqualIssuance(a, b identity.AuthorityGrant) bool {
	return a.Decision == b.Decision && a.ID == b.ID && a.Tenant == b.Tenant && a.Attempt == b.Attempt && a.Provider == b.Provider &&
		a.Action == b.Action && a.Mode == b.Mode && a.Operation == b.Operation &&
		a.InputDigest == b.InputDigest && a.Scope == b.Scope && a.GrantedBy == b.GrantedBy &&
		a.PolicyGeneration == b.PolicyGeneration && sameInstant(a.IssuedAt, b.IssuedAt) && sameInstant(a.ExpiresAt, b.ExpiresAt)
}

// GetGrant loads a grant by tenant and id.
func GetGrant(ctx context.Context, q Querier, tenant ids.TenantID, id ids.GrantID) (identity.AuthorityGrant, error) {
	var grant identity.AuthorityGrant
	var mode, action string
	var idText, tenantText, attemptText, providerText, grantedByText string
	var operationText, digestText, scopeClassText, scopeTargetText *string
	var generation int64
	var revoked, retired *time.Time
	err := q.QueryRow(ctx, `
		select grant_id::text, tenant_id::text, attempt_id::text, provider_id::text, action, mode,
			operation_id::text, input_digest, scope_class, scope_target, granted_by::text,
			policy_generation, issued_at, expires_at, revoked_at, retired_at
		from authority_grant where tenant_id = $1 and grant_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &attemptText, &providerText, &action, &mode,
		&operationText, &digestText, &scopeClassText, &scopeTargetText, &grantedByText,
		&generation, &grant.IssuedAt, &grant.ExpiresAt, &revoked, &retired)
	if err != nil {
		if isNoRows(err) {
			return identity.AuthorityGrant{}, ErrNotFound
		}
		return identity.AuthorityGrant{}, err
	}
	var parseErr error
	if grant.ID, parseErr = parseID[ids.Grant](`authority_grant.grant_id`, idText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if grant.Tenant, parseErr = parseID[ids.Tenant](`authority_grant.tenant_id`, tenantText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if grant.Attempt, parseErr = parseID[ids.Attempt](`authority_grant.attempt_id`, attemptText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if grant.Provider, parseErr = parseID[ids.Provider](`authority_grant.provider_id`, providerText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if grant.GrantedBy, parseErr = parseID[ids.Principal](`authority_grant.granted_by`, grantedByText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if grant.Operation, parseErr = parseOptionalID[ids.Operation](`authority_grant.operation_id`, operationText); parseErr != nil {
		return identity.AuthorityGrant{}, parseErr
	}
	if digestText != nil {
		grant.InputDigest = identity.Digest(*digestText)
	}
	if scopeClassText != nil {
		grant.Scope.Class = identity.ScopeClass(*scopeClassText)
	}
	if scopeTargetText != nil {
		grant.Scope.Target = *scopeTargetText
	}
	grant.Action, grant.Mode = identity.ActionClass(action), identity.GrantMode(mode)
	grant.PolicyGeneration = uint64(generation)
	if revoked != nil {
		grant.RevokedAt = *revoked
	}
	if retired != nil {
		grant.RetiredAt = *retired
	}
	var decision *string
	if err := q.QueryRow(ctx, `select decision_id::text from authority_grant where tenant_id=$1 and grant_id=$2`, tenant.String(), id.String()).Scan(&decision); err != nil {
		return grant, err
	}
	if grant.Decision, parseErr = parseOptionalID[ids.Decision]("authority_grant.decision_id", decision); parseErr != nil {
		return grant, parseErr
	}
	if err := grant.Validate(); err != nil {
		return identity.AuthorityGrant{}, err
	}
	return grant, nil
}

// RevokeGrant marks a standing grant revoked. An already-revoked grant
// replays instead of failing, so revocation retries are safe.
func RevokeGrant(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.GrantID, at time.Time) (identity.AuthorityGrant, bool, error) {
	return setGrantTime(ctx, u, tenant, id, at, "revoked_at")
}

// RetireGrant marks an allow-once grant retired after its first use. The
// retired row persists in the same transaction as the allowing AuthorityUse
// so the grant can never authorize a second action.
func RetireGrant(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.GrantID, at time.Time) (identity.AuthorityGrant, bool, error) {
	return setGrantTime(ctx, u, tenant, id, at, "retired_at")
}

func setGrantTime(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.GrantID, at time.Time, column string) (identity.AuthorityGrant, bool, error) {
	grant, err := GetGrantForUpdate(ctx, u, tenant, id)
	if err != nil {
		return identity.AuthorityGrant{}, false, err
	}
	current := grant.RevokedAt
	if column == "retired_at" {
		current = grant.RetiredAt
	}
	if !current.IsZero() {
		return grant, true, nil
	}
	var res interface{ RowsAffected() int64 }
	var execErr error
	if column == "revoked_at" {
		res, execErr = u.Exec(ctx, `update authority_grant set revoked_at = $3 where tenant_id = $1 and grant_id = $2 and revoked_at is null`,
			tenant.String(), id.String(), at)
	} else {
		res, execErr = u.Exec(ctx, `update authority_grant set retired_at = $3 where tenant_id = $1 and grant_id = $2 and retired_at is null`,
			tenant.String(), id.String(), at)
	}
	if execErr != nil {
		return identity.AuthorityGrant{}, false, execErr
	}
	if res.RowsAffected() == 0 {
		return identity.AuthorityGrant{}, false, ErrStaleVersion
	}
	grant, err = GetGrant(ctx, u, tenant, id)
	return grant, false, err
}

// GetGrantForUpdate locks a grant row for the caller's read-modify-write.
func GetGrantForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.GrantID) (identity.AuthorityGrant, error) {
	if err := lockRow(ctx, u, "authority_grant", "grant_id", tenant.String(), id.String()); err != nil {
		return identity.AuthorityGrant{}, err
	}
	return GetGrant(ctx, u, tenant, id)
}

// InsertUse persists one authorization decision. Uses are immutable evidence;
// a primary-key conflict compares the stored row for replay.
func InsertUse(ctx context.Context, u *UnitOfWork, use identity.AuthorityUse) (identity.AuthorityUse, bool, error) {
	if err := use.Validate(); err != nil {
		return identity.AuthorityUse{}, false, err
	}
	scope, err := json.Marshal(use.Scope)
	if err != nil {
		return use, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into authority_use (tenant_id, operation_id, attempt_id, provider_id, action, input_digest, grant_id, source, granted_by, outcome, reason, policy_generation, decided_at, scope, decision_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb, $15)`,
		use.Tenant.String(), use.Operation.String(), use.Attempt.String(), use.Provider.String(),
		string(use.Action), string(use.InputDigest), uuidOrNil(use.Grant), textOrNil(string(use.Source)),
		uuidOrNil(use.GrantedBy), string(use.Outcome), textOrNil(string(use.Reason)),
		int64(use.PolicyGeneration), use.DecidedAt, string(scope), uuidOrNil(use.Decision),
	)
	if err != nil {
		return identity.AuthorityUse{}, false, err
	}
	if inserted {
		return use, false, nil
	}
	stored, loadErr := GetUse(ctx, u, use.Tenant, use.Operation, use.DecidedAt, use.Action)
	if isNotFound(loadErr) {
		return identity.AuthorityUse{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return identity.AuthorityUse{}, false, loadErr
	}
	if !usesEqual(stored, use) {
		return identity.AuthorityUse{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetUse loads one authorization decision by its natural key. The timestamp
// is truncated to storage precision so a replayed lookup matches the row a
// previous call wrote.
func GetUse(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID, decidedAt time.Time, action identity.ActionClass) (identity.AuthorityUse, error) {
	var use identity.AuthorityUse
	var actionText, idText, tenantText, attemptText, providerText, digestText string
	var grantText, sourceText, grantedByText, reasonText *string
	var outcomeText string
	var generation int64
	err := q.QueryRow(ctx, `
		select operation_id::text, tenant_id::text, attempt_id::text, provider_id::text, action, input_digest,
			grant_id::text, source, granted_by::text, outcome, reason, policy_generation, decided_at
		from authority_use where tenant_id = $1 and operation_id = $2 and decided_at = $3 and action = $4`,
		tenant.String(), operation.String(), decidedAt.Truncate(time.Microsecond), string(action),
	).Scan(&idText, &tenantText, &attemptText, &providerText, &actionText, &digestText,
		&grantText, &sourceText, &grantedByText, &outcomeText, &reasonText, &generation, &use.DecidedAt)
	if err != nil {
		if isNoRows(err) {
			return identity.AuthorityUse{}, ErrNotFound
		}
		return identity.AuthorityUse{}, err
	}
	var parseErr error
	if use.Operation, parseErr = parseID[ids.Operation](`authority_use.operation_id`, idText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	if use.Tenant, parseErr = parseID[ids.Tenant](`authority_use.tenant_id`, tenantText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	if use.Attempt, parseErr = parseID[ids.Attempt](`authority_use.attempt_id`, attemptText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	if use.Provider, parseErr = parseID[ids.Provider](`authority_use.provider_id`, providerText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	if use.Grant, parseErr = parseOptionalID[ids.Grant](`authority_use.grant_id`, grantText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	if use.GrantedBy, parseErr = parseOptionalID[ids.Principal](`authority_use.granted_by`, grantedByText); parseErr != nil {
		return identity.AuthorityUse{}, parseErr
	}
	use.Action = identity.ActionClass(actionText)
	use.InputDigest = identity.Digest(digestText)
	if sourceText != nil {
		use.Source = identity.AuthoritySource(*sourceText)
	}
	use.Outcome = identity.UseOutcome(outcomeText)
	if reasonText != nil {
		use.Reason = identity.DenialReason(*reasonText)
	}
	use.PolicyGeneration = uint64(generation)
	var scope []byte
	var decision *string
	if err := q.QueryRow(ctx, `select scope, decision_id::text from authority_use where tenant_id=$1 and operation_id=$2 and decided_at=$3 and action=$4`, tenant.String(), operation.String(), decidedAt.Truncate(time.Microsecond), string(action)).Scan(&scope, &decision); err != nil {
		return use, err
	}
	if len(scope) > 0 {
		if err := json.Unmarshal(scope, &use.Scope); err != nil {
			return use, err
		}
	}
	if use.Decision, parseErr = parseOptionalID[ids.Decision]("authority_use.decision_id", decision); parseErr != nil {
		return use, parseErr
	}
	if err := use.Validate(); err != nil {
		return identity.AuthorityUse{}, err
	}
	return use, nil
}

func usesEqual(a, b identity.AuthorityUse) bool {
	return a.Scope == b.Scope && a.Decision == b.Decision && a.Tenant == b.Tenant && a.Operation == b.Operation && a.Attempt == b.Attempt &&
		a.Provider == b.Provider && a.Action == b.Action && a.InputDigest == b.InputDigest &&
		a.Grant == b.Grant && a.Source == b.Source && a.GrantedBy == b.GrantedBy &&
		a.Outcome == b.Outcome && a.Reason == b.Reason &&
		a.PolicyGeneration == b.PolicyGeneration && sameInstant(a.DecidedAt, b.DecidedAt)
}

// InsertHookEvidence persists pre-action hook evidence digests under the
// tenant that owns the operation. Evidence is immutable; a natural-key
// conflict compares for replay.
func InsertHookEvidence(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, evidence contracts.HookEvidence) (contracts.HookEvidence, bool, error) {
	if err := evidence.Validate(); err != nil {
		return contracts.HookEvidence{}, false, err
	}
	usage, err := json.Marshal(evidence.Usage)
	if err != nil {
		return contracts.HookEvidence{}, false, err
	}
	if len(evidence.Usage) == 0 {
		usage = []byte("[]")
	}
	inserted, err := insertReturning(ctx, u, `
		insert into hook_evidence (tenant_id, operation_id, outcome, original_digest, effective_digest, rewritten, duration_ns, response_digest, user_message_digest, agent_message_digest, context_digest, usage)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb)`,
		tenant.String(), evidence.Operation.String(), string(evidence.Outcome),
		string(evidence.OriginalDigest), textOrNil(string(evidence.EffectiveDigest)), evidence.Rewritten, durationOrNil(evidence.Duration), textOrNil(string(evidence.ResponseDigest)), textOrNil(string(evidence.UserMessageDigest)), textOrNil(string(evidence.AgentMessageDigest)), textOrNil(string(evidence.ContextDigest)), string(usage),
	)
	if err != nil {
		return contracts.HookEvidence{}, false, err
	}
	if inserted {
		return evidence, false, nil
	}
	stored, loadErr := GetHookEvidence(ctx, u, tenant, evidence.Operation)
	if isNotFound(loadErr) {
		return contracts.HookEvidence{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return contracts.HookEvidence{}, false, loadErr
	}
	storedJSON, _ := json.Marshal(stored)
	evidenceJSON, _ := json.Marshal(evidence)
	if string(storedJSON) != string(evidenceJSON) {
		return contracts.HookEvidence{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetHookEvidence loads hook evidence for one operation.
func GetHookEvidence(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID) (contracts.HookEvidence, error) {
	var evidence contracts.HookEvidence
	var outcome, original string
	var effective, response, user, agent, contextDigest *string
	var duration *int64
	var usage []byte
	err := q.QueryRow(ctx, `
		select outcome, original_digest, effective_digest, rewritten, duration_ns, response_digest, user_message_digest, agent_message_digest, context_digest, usage
		from hook_evidence where tenant_id = $1 and operation_id = $2`,
		tenant.String(), operation.String(),
	).Scan(&outcome, &original, &effective, &evidence.Rewritten, &duration, &response, &user, &agent, &contextDigest, &usage)
	if err != nil {
		if isNoRows(err) {
			return contracts.HookEvidence{}, ErrNotFound
		}
		return contracts.HookEvidence{}, err
	}
	evidence.Operation = operation
	evidence.Outcome = contracts.HookOutcome(outcome)
	evidence.OriginalDigest = identity.Digest(original)
	if effective != nil {
		evidence.EffectiveDigest = identity.Digest(*effective)
	}
	if duration != nil {
		evidence.Duration = time.Duration(*duration)
	}
	if response != nil {
		evidence.ResponseDigest = identity.Digest(*response)
	}
	if user != nil {
		evidence.UserMessageDigest = identity.Digest(*user)
	}
	if agent != nil {
		evidence.AgentMessageDigest = identity.Digest(*agent)
	}
	if contextDigest != nil {
		evidence.ContextDigest = identity.Digest(*contextDigest)
	}
	if len(usage) > 0 {
		if err := json.Unmarshal(usage, &evidence.Usage); err != nil {
			return contracts.HookEvidence{}, corruptRow("hook_evidence.usage", err)
		}
	}
	if err := evidence.Validate(); err != nil {
		return contracts.HookEvidence{}, err
	}
	return evidence, nil
}

func durationOrNil(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return int64(d)
}

// InsertHookStageEvidence persists immutable post-action or action-failure
// evidence. Its natural key is the parent operation plus hook kind, so replay
// is stable while distinct stages remain independent.
func InsertHookStageEvidence(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, evidence contracts.HookStageEvidence) (contracts.HookStageEvidence, bool, error) {
	if tenant.IsZero() || evidence.Validate() != nil {
		return contracts.HookStageEvidence{}, false, ErrPayloadMismatch
	}
	usage, err := json.Marshal(evidence.Usage)
	if err != nil {
		return contracts.HookStageEvidence{}, false, err
	}
	if len(evidence.Usage) == 0 {
		usage = []byte("[]")
	}
	inserted, err := insertReturning(ctx, u, `
		insert into hook_stage_evidence (tenant_id,operation_id,kind,input_digest,output_digest,failure_code,duration_ns,response_digest,user_message_digest,agent_message_digest,context_digest,usage)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`,
		tenant.String(), evidence.Operation.String(), string(evidence.Kind), string(evidence.InputDigest), textOrNil(string(evidence.OutputDigest)), textOrNil(string(evidence.FailureCode)), int64(evidence.Duration), string(evidence.ResponseDigest), textOrNil(string(evidence.UserMessageDigest)), textOrNil(string(evidence.AgentMessageDigest)), textOrNil(string(evidence.ContextDigest)), string(usage))
	if err != nil {
		return contracts.HookStageEvidence{}, false, err
	}
	if inserted {
		return evidence, false, nil
	}
	stored, err := GetHookStageEvidence(ctx, u, tenant, evidence.Operation, evidence.Kind)
	if isNotFound(err) {
		return contracts.HookStageEvidence{}, false, ErrPayloadMismatch
	}
	if err != nil {
		return contracts.HookStageEvidence{}, false, err
	}
	left, _ := json.Marshal(stored)
	right, _ := json.Marshal(evidence)
	if string(left) != string(right) {
		return contracts.HookStageEvidence{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetHookStageEvidence loads one immutable follow-up hook stage.
func GetHookStageEvidence(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID, kind contracts.HookKind) (contracts.HookStageEvidence, error) {
	var e contracts.HookStageEvidence
	var input, response string
	var output, failure, user, agent, contextDigest *string
	var duration int64
	var usage []byte
	err := q.QueryRow(ctx, `select input_digest,output_digest,failure_code,duration_ns,response_digest,user_message_digest,agent_message_digest,context_digest,usage from hook_stage_evidence where tenant_id=$1 and operation_id=$2 and kind=$3`, tenant.String(), operation.String(), string(kind)).Scan(&input, &output, &failure, &duration, &response, &user, &agent, &contextDigest, &usage)
	if err != nil {
		if isNoRows(err) {
			return contracts.HookStageEvidence{}, ErrNotFound
		}
		return contracts.HookStageEvidence{}, err
	}
	e.Operation, e.Kind = operation, kind
	e.InputDigest, e.ResponseDigest, e.Duration = identity.Digest(input), identity.Digest(response), time.Duration(duration)
	if output != nil {
		e.OutputDigest = identity.Digest(*output)
	}
	if failure != nil {
		e.FailureCode = contracts.FailureCode(*failure)
	}
	if user != nil {
		e.UserMessageDigest = identity.Digest(*user)
	}
	if agent != nil {
		e.AgentMessageDigest = identity.Digest(*agent)
	}
	if contextDigest != nil {
		e.ContextDigest = identity.Digest(*contextDigest)
	}
	if err := json.Unmarshal(usage, &e.Usage); err != nil {
		return contracts.HookStageEvidence{}, corruptRow("hook_stage_evidence.usage", err)
	}
	if err := e.Validate(); err != nil {
		return contracts.HookStageEvidence{}, err
	}
	return e, nil
}
