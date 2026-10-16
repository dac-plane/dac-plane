// Package postgres Lifecycle repositories persist tasks, attempts, attention requests and
// decisions. Every mutation loads its row FOR UPDATE inside the caller's unit
// of work, runs the pure in-memory transition, and commits the returned
// record with its audit event, durable event and outbox entry atomically.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Transition carries the shared arguments of every versioned mutation: the
// expected version for the compare-and-set, when and by whom the change is
// made, the audit identity, and the outbox dispatch for applied changes.
// Refusals record audit evidence only; pass a nil Outbox for them.
// AuditID identifies one mutation attempt: every call, including a retry,
// needs a fresh one, since the audit row persists even when the transition
// is refused.
type Transition struct {
	ExpectedVersion uint64
	At              time.Time
	Actor           lifecycle.Actor
	AuditID         ids.AuditID
	Outbox          *OutboxPayload
}

// replayIfCommitted answers idempotent retries: when a row already sits one
// version past the expected version and its durable record names the same
// event (and target, when the caller names one), the command already
// committed and the stored row is the original outcome.
func replayIfCommitted(ctx context.Context, q Querier, tenant ids.TenantID, record lifecycle.RecordKind, recordID ids.AnyID, committed uint64, event, target string) (bool, error) {
	storedEvent, storedTo, found, err := findDurable(ctx, q, tenant, record, recordID, committed)
	if err != nil {
		return false, err
	}
	if !found || storedEvent != event {
		return false, ErrStaleVersion
	}
	if target != "" && storedTo != target {
		return false, ErrPayloadMismatch
	}
	return true, nil
}

// GetTask loads a task by tenant and id. Other tenants read as not found.
func GetTask(ctx context.Context, q Querier, tenant ids.TenantID, id ids.TaskID) (lifecycle.Task, error) {
	var task lifecycle.Task
	var state string
	var idText, tenantText, ownerText string
	err := q.QueryRow(ctx, `
		select task_id::text, tenant_id::text, owner_id::text, state, version, created_at, updated_at
		from task where tenant_id = $1 and task_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &ownerText, &state, &task.Version, &task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Task{}, ErrNotFound
		}
		return lifecycle.Task{}, err
	}
	if task.ID, err = parseID[ids.Task](`task.task_id`, idText); err != nil {
		return lifecycle.Task{}, err
	}
	if task.Tenant, err = parseID[ids.Tenant](`task.tenant_id`, tenantText); err != nil {
		return lifecycle.Task{}, err
	}
	if task.Owner, err = parseID[ids.Principal](`task.owner_id`, ownerText); err != nil {
		return lifecycle.Task{}, err
	}
	task.State = lifecycle.TaskState(state)
	if err := task.Validate(); err != nil {
		return lifecycle.Task{}, err
	}
	return task, nil
}

// InsertTask persists a new task. A primary-key conflict loads the stored
// row: identical domain payloads replay it, anything else mismatches.
func InsertTask(ctx context.Context, u *UnitOfWork, task lifecycle.Task) (lifecycle.Task, bool, error) {
	if err := task.Validate(); err != nil {
		return lifecycle.Task{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into task (tenant_id, task_id, owner_id, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7)`,
		task.Tenant.String(), task.ID.String(), task.Owner.String(),
		string(task.State), int64(task.Version), task.CreatedAt, task.UpdatedAt,
	)
	if err != nil {
		return lifecycle.Task{}, false, err
	}
	if inserted {
		return task, false, nil
	}
	stored, loadErr := GetTask(ctx, u, task.Tenant, task.ID)
	if isNotFound(loadErr) {
		return lifecycle.Task{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.Task{}, false, loadErr
	}
	if stored.Tenant != task.Tenant || stored.Owner != task.Owner ||
		stored.State != task.State || stored.Version != task.Version {
		return lifecycle.Task{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// ApplyTask performs a task event at the expected version and atomically
// records audit, durable event and outbox rows. Refusals persist refusal
// audit evidence and return the transition error; the row is unchanged.
func ApplyTask(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.TaskID, event lifecycle.TaskEvent, target lifecycle.TaskState, tr Transition) (lifecycle.Task, bool, error) {
	var task lifecycle.Task
	var state string
	var idText, tenantText, ownerText string
	err := u.QueryRow(ctx, `
		select task_id::text, tenant_id::text, owner_id::text, state, version, created_at, updated_at
		from task where tenant_id = $1 and task_id = $2 for update`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &ownerText, &state, &task.Version, &task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Task{}, false, ErrNotFound
		}
		return lifecycle.Task{}, false, err
	}
	var parseErr error
	if task.ID, parseErr = parseID[ids.Task](`task.task_id`, idText); parseErr != nil {
		return lifecycle.Task{}, false, parseErr
	}
	if task.Tenant, parseErr = parseID[ids.Tenant](`task.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Task{}, false, parseErr
	}
	if task.Owner, parseErr = parseID[ids.Principal](`task.owner_id`, ownerText); parseErr != nil {
		return lifecycle.Task{}, false, parseErr
	}
	task.State = lifecycle.TaskState(state)
	if err := task.Validate(); err != nil {
		return lifecycle.Task{}, false, err
	}
	if task.Version != tr.ExpectedVersion {
		if task.Version == tr.ExpectedVersion+1 {
			replayed, replayErr := replayIfCommitted(ctx, u, tenant, lifecycle.RecordTask, task.ID.Untyped(), task.Version, string(event), string(target))
			if replayErr != nil {
				return lifecycle.Task{}, false, replayErr
			}
			if replayed {
				return task, true, nil
			}
		}
		return lifecycle.Task{}, false, ErrStaleVersion
	}
	next, change, applyErr := task.Apply(event, tr.ExpectedVersion, target, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Task{}, false, histErr
			}
		}
		return task, false, applyErr
	}
	res, err := u.Exec(ctx, `
		update task set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and task_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	)
	if err != nil {
		return lifecycle.Task{}, false, err
	}
	if res.RowsAffected() == 0 {
		return lifecycle.Task{}, false, ErrStaleVersion
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.Task{}, false, err
	}
	return next, false, nil
}

// GetTaskForUpdate locks a task row for the caller's read-modify-write.
func GetTaskForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.TaskID) (lifecycle.Task, error) {
	var locked bool
	err := u.QueryRow(ctx, `select true from task where tenant_id = $1 and task_id = $2 for update`,
		tenant.String(), id.String()).Scan(&locked)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Task{}, ErrNotFound
		}
		return lifecycle.Task{}, err
	}
	return GetTask(ctx, u, tenant, id)
}

// GetAttempt loads an attempt by tenant and id.
func GetAttempt(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AttemptID) (lifecycle.Attempt, error) {
	var attempt lifecycle.Attempt
	var state, idText, tenantText, taskText string
	var number int32
	err := q.QueryRow(ctx, `
		select attempt_id::text, tenant_id::text, task_id::text, number, state, version, runtime_stopped, environment_stopped, created_at, updated_at
		from attempt where tenant_id = $1 and attempt_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &number, &state, &attempt.Version,
		&attempt.RuntimeStopped, &attempt.EnvironmentStopped, &attempt.CreatedAt, &attempt.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Attempt{}, ErrNotFound
		}
		return lifecycle.Attempt{}, err
	}
	var parseErr error
	if attempt.ID, parseErr = parseID[ids.Attempt](`attempt.attempt_id`, idText); parseErr != nil {
		return lifecycle.Attempt{}, parseErr
	}
	if attempt.Tenant, parseErr = parseID[ids.Tenant](`attempt.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Attempt{}, parseErr
	}
	if attempt.Task, parseErr = parseID[ids.Task](`attempt.task_id`, taskText); parseErr != nil {
		return lifecycle.Attempt{}, parseErr
	}
	attempt.Number = uint32(number)
	attempt.State = lifecycle.AttemptState(state)
	if err := attempt.Validate(); err != nil {
		return lifecycle.Attempt{}, err
	}
	return attempt, nil
}

// InsertAttempt persists a new attempt with replay comparison on conflict.
func InsertAttempt(ctx context.Context, u *UnitOfWork, attempt lifecycle.Attempt) (lifecycle.Attempt, bool, error) {
	if err := attempt.Validate(); err != nil {
		return lifecycle.Attempt{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into attempt (tenant_id, attempt_id, task_id, number, state, version, runtime_stopped, environment_stopped, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		attempt.Tenant.String(), attempt.ID.String(), attempt.Task.String(), int32(attempt.Number),
		string(attempt.State), int64(attempt.Version),
		attempt.RuntimeStopped, attempt.EnvironmentStopped, attempt.CreatedAt, attempt.UpdatedAt,
	)
	if err != nil {
		return lifecycle.Attempt{}, false, err
	}
	if inserted {
		return attempt, false, nil
	}
	stored, loadErr := GetAttempt(ctx, u, attempt.Tenant, attempt.ID)
	if isNotFound(loadErr) {
		return lifecycle.Attempt{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.Attempt{}, false, loadErr
	}
	if stored.Tenant != attempt.Tenant || stored.Task != attempt.Task || stored.Number != attempt.Number ||
		stored.State != attempt.State || stored.Version != attempt.Version ||
		stored.RuntimeStopped != attempt.RuntimeStopped || stored.EnvironmentStopped != attempt.EnvironmentStopped {
		return lifecycle.Attempt{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// ApplyAttempt performs an attempt event at the expected version with atomic
// history, following the same stale, replay and refusal rules as ApplyTask.
func ApplyAttempt(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AttemptID, event lifecycle.AttemptEvent, target lifecycle.AttemptState, tr Transition) (lifecycle.Attempt, bool, error) {
	var attempt lifecycle.Attempt
	var state, idText, tenantText, taskText string
	var number int32
	err := u.QueryRow(ctx, `
		select attempt_id::text, tenant_id::text, task_id::text, number, state, version, runtime_stopped, environment_stopped, created_at, updated_at
		from attempt where tenant_id = $1 and attempt_id = $2 for update`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &number, &state, &attempt.Version,
		&attempt.RuntimeStopped, &attempt.EnvironmentStopped, &attempt.CreatedAt, &attempt.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Attempt{}, false, ErrNotFound
		}
		return lifecycle.Attempt{}, false, err
	}
	var parseErr error
	if attempt.ID, parseErr = parseID[ids.Attempt](`attempt.attempt_id`, idText); parseErr != nil {
		return lifecycle.Attempt{}, false, parseErr
	}
	if attempt.Tenant, parseErr = parseID[ids.Tenant](`attempt.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Attempt{}, false, parseErr
	}
	if attempt.Task, parseErr = parseID[ids.Task](`attempt.task_id`, taskText); parseErr != nil {
		return lifecycle.Attempt{}, false, parseErr
	}
	attempt.Number = uint32(number)
	attempt.State = lifecycle.AttemptState(state)
	if err := attempt.Validate(); err != nil {
		return lifecycle.Attempt{}, false, err
	}
	if attempt.Version != tr.ExpectedVersion {
		if attempt.Version == tr.ExpectedVersion+1 {
			replayed, replayErr := replayIfCommitted(ctx, u, tenant, lifecycle.RecordAttempt, attempt.ID.Untyped(), attempt.Version, string(event), string(target))
			if replayErr != nil {
				return lifecycle.Attempt{}, false, replayErr
			}
			if replayed {
				return attempt, true, nil
			}
		}
		return lifecycle.Attempt{}, false, ErrStaleVersion
	}
	next, change, applyErr := attempt.Apply(event, tr.ExpectedVersion, target, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Attempt{}, false, histErr
			}
		}
		return attempt, false, applyErr
	}
	res, err := u.Exec(ctx, `
		update attempt set state = $3, version = $4, runtime_stopped = $5, environment_stopped = $6, updated_at = $7
		where tenant_id = $1 and attempt_id = $2 and version = $8`,
		tenant.String(), id.String(), string(next.State), int64(next.Version),
		next.RuntimeStopped, next.EnvironmentStopped, next.UpdatedAt, int64(tr.ExpectedVersion),
	)
	if err != nil {
		return lifecycle.Attempt{}, false, err
	}
	if res.RowsAffected() == 0 {
		return lifecycle.Attempt{}, false, ErrStaleVersion
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.Attempt{}, false, err
	}
	return next, false, nil
}

// formatResponders renders principal ids as a uuid[] literal.
func formatResponders(responders []ids.PrincipalID) string {
	parts := make([]string, 0, len(responders))
	for _, responder := range responders {
		parts = append(parts, responder.String())
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// GetAttention loads an attention request by tenant and id.
func GetAttention(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AttentionID) (lifecycle.AttentionRequest, error) {
	var request lifecycle.AttentionRequest
	var kind, state, idText, tenantText, taskText, attemptText, respondersText string
	var providerText, operationText, digestText, decisionText *string
	err := q.QueryRow(ctx, `
		select attention_id::text, tenant_id::text, task_id::text, attempt_id::text, kind,
			provider_id::text, operation_id::text, action_digest, policy_generation, responders::text,
			state, version, decision_id::text, expires_at, created_at, updated_at
		from attention_request where tenant_id = $1 and attention_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &attemptText, &kind,
		&providerText, &operationText, &digestText, &request.PolicyGeneration, &respondersText,
		&state, &request.Version, &decisionText, &request.ExpiresAt, &request.CreatedAt, &request.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.AttentionRequest{}, ErrNotFound
		}
		return lifecycle.AttentionRequest{}, err
	}
	var parseErr error
	if request.ID, parseErr = parseID[ids.Attention](`attention_request.attention_id`, idText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Tenant, parseErr = parseID[ids.Tenant](`attention_request.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Task, parseErr = parseID[ids.Task](`attention_request.task_id`, taskText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Attempt, parseErr = parseID[ids.Attempt](`attention_request.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Provider, parseErr = parseOptionalID[ids.Provider](`attention_request.provider_id`, providerText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Operation, parseErr = parseOptionalID[ids.Operation](`attention_request.operation_id`, operationText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if digestText != nil {
		request.ActionDigest = identity.Digest(*digestText)
	}
	if request.Decision, parseErr = parseOptionalID[ids.Decision](`attention_request.decision_id`, decisionText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	if request.Responders, parseErr = parseUUIDArray[ids.Principal](`attention_request.responders`, respondersText); parseErr != nil {
		return lifecycle.AttentionRequest{}, parseErr
	}
	request.Kind = lifecycle.AttentionKind(kind)
	request.State = lifecycle.AttentionState(state)
	if err := request.Validate(); err != nil {
		return lifecycle.AttentionRequest{}, err
	}
	return request, nil
}

// InsertAttention persists a new attention request with replay comparison.
func InsertAttention(ctx context.Context, u *UnitOfWork, request lifecycle.AttentionRequest) (lifecycle.AttentionRequest, bool, error) {
	if err := request.Validate(); err != nil {
		return lifecycle.AttentionRequest{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into attention_request (tenant_id, attention_id, task_id, attempt_id, kind, provider_id, operation_id, action_digest, policy_generation, responders, state, version, decision_id, expires_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid[], $11, $12, $13, $14, $15, $16)`,
		request.Tenant.String(), request.ID.String(), request.Task.String(), request.Attempt.String(), string(request.Kind),
		uuidOrNil(request.Provider), uuidOrNil(request.Operation), textOrNil(string(request.ActionDigest)),
		int64(request.PolicyGeneration), formatResponders(request.Responders),
		string(request.State), int64(request.Version), uuidOrNil(request.Decision),
		request.ExpiresAt, request.CreatedAt, request.UpdatedAt,
	)
	if err != nil {
		return lifecycle.AttentionRequest{}, false, err
	}
	if inserted {
		return request, false, nil
	}
	stored, loadErr := GetAttention(ctx, u, request.Tenant, request.ID)
	if isNotFound(loadErr) {
		return lifecycle.AttentionRequest{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.AttentionRequest{}, false, loadErr
	}
	if stored.Task != request.Task || stored.Attempt != request.Attempt || stored.Kind != request.Kind ||
		stored.Provider != request.Provider || stored.Operation != request.Operation ||
		stored.ActionDigest != request.ActionDigest || stored.PolicyGeneration != request.PolicyGeneration ||
		stored.State != request.State || stored.Version != request.Version || stored.Decision != request.Decision ||
		!respondersEqual(stored.Responders, request.Responders) {
		return lifecycle.AttentionRequest{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

func respondersEqual(a, b []ids.PrincipalID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// updateAttentionRow writes a decided or transitioned request with a version
// predicate. Zero rows mean a concurrent writer moved first.
func updateAttentionRow(ctx context.Context, q Querier, request lifecycle.AttentionRequest, expectedVersion uint64) error {
	res, err := q.Exec(ctx, `
		update attention_request set state = $3, version = $4, decision_id = $5, updated_at = $6
		where tenant_id = $1 and attention_id = $2 and version = $7`,
		request.Tenant.String(), request.ID.String(), string(request.State),
		int64(request.Version), uuidOrNil(request.Decision), request.UpdatedAt, int64(expectedVersion),
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrStaleVersion
	}
	return nil
}

// ApplyAttention performs a non-decision attention event with atomic history.
// Decisions go through DecideAttention so the decision row commits together.
func ApplyAttention(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AttentionID, event lifecycle.AttentionEvent, tr Transition) (lifecycle.AttentionRequest, bool, error) {
	if event == lifecycle.AttentionDecide {
		return lifecycle.AttentionRequest{}, false, fmt.Errorf("%w: decisions go through DecideAttention", lifecycle.ErrInvalid)
	}
	request, err := GetAttentionForUpdate(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.AttentionRequest{}, false, err
	}
	if request.Version != tr.ExpectedVersion {
		if request.Version == tr.ExpectedVersion+1 {
			replayed, replayErr := replayIfCommitted(ctx, u, tenant, lifecycle.RecordAttention, request.ID.Untyped(), request.Version, string(event), "")
			if replayErr != nil {
				return lifecycle.AttentionRequest{}, false, replayErr
			}
			if replayed {
				return request, true, nil
			}
		}
		return lifecycle.AttentionRequest{}, false, ErrStaleVersion
	}
	next, change, applyErr := request.Apply(event, tr.ExpectedVersion, tr.At)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.AttentionRequest{}, false, histErr
			}
		}
		return request, false, applyErr
	}
	if err := updateAttentionRow(ctx, u, next, tr.ExpectedVersion); err != nil {
		return lifecycle.AttentionRequest{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.AttentionRequest{}, false, err
	}
	return next, false, nil
}

// GetAttentionForUpdate loads an attention request locked for writing.
func GetAttentionForUpdate(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AttentionID) (lifecycle.AttentionRequest, error) {
	// Lock first so the caller's read-modify-write serializes; the row is
	// then read through the tenant-scoped getter.
	var locked bool
	err := q.QueryRow(ctx, `select true from attention_request where tenant_id = $1 and attention_id = $2 for update`,
		tenant.String(), id.String()).Scan(&locked)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.AttentionRequest{}, ErrNotFound
		}
		return lifecycle.AttentionRequest{}, err
	}
	return GetAttention(ctx, q, tenant, id)
}

// DecideAttention records a decision and moves its request atomically: the
// decision row, the decided request, and the audit, durable-event and outbox
// rows commit together. Replaying a recorded decision returns it instead of
// writing again; a different decision for the same request fails through the
// pure Decide rules. Mutations require a *UnitOfWork so concurrent writers
// serialize on the row lock.
func DecideAttention(ctx context.Context, u *UnitOfWork, decision lifecycle.Decision, currentPolicyGeneration uint64, tr Transition) (lifecycle.AttentionRequest, lifecycle.Decision, bool, error) {
	if err := decision.Validate(); err != nil {
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, err
	}
	request, err := GetAttentionForUpdate(ctx, u, decision.Tenant, decision.Request)
	if err != nil {
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, err
	}
	if request.Decision == decision.ID && !request.Decision.IsZero() {
		stored, loadErr := GetDecision(ctx, u, decision.Tenant, decision.ID)
		if loadErr != nil {
			return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, loadErr
		}
		if !decisionsEqual(stored, decision) {
			return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, ErrPayloadMismatch
		}
		return request, stored, true, nil
	}
	decided, change, decideErr := request.Decide(decision, currentPolicyGeneration)
	if decideErr != nil {
		if terr, ok := decideErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, decision.DecidedAt)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, histErr
			}
		}
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, decideErr
	}
	_, err = u.Exec(ctx, `
		insert into decision (tenant_id, decision_id, attention_id, request_version, principal_id, kind, answer_digest, decided_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		decision.Tenant.String(), decision.ID.String(), decision.Request.String(),
		int64(decision.RequestVersion), decision.Principal.String(), string(decision.Kind),
		textOrNil(string(decision.AnswerDigest)), decision.DecidedAt,
	)
	if err != nil {
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, err
	}
	if err := updateAttentionRow(ctx, u, decided, decision.RequestVersion); err != nil {
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, decision.DecidedAt)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.AttentionRequest{}, lifecycle.Decision{}, false, err
	}
	return decided, decision, false, nil
}

// decisionsEqual compares decisions field by field, using Equal for the
// timestamp so monotonic readings never mismatch a stored row.
func decisionsEqual(a, b lifecycle.Decision) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Request == b.Request &&
		a.RequestVersion == b.RequestVersion && a.Principal == b.Principal &&
		a.Kind == b.Kind && a.AnswerDigest == b.AnswerDigest && sameInstant(a.DecidedAt, b.DecidedAt)
}

// GetDecision loads a decision by tenant and id.
func GetDecision(ctx context.Context, q Querier, tenant ids.TenantID, id ids.DecisionID) (lifecycle.Decision, error) {
	var decision lifecycle.Decision
	var kind, idText, tenantText, requestText, principalText string
	var digestText *string
	err := q.QueryRow(ctx, `
		select decision_id::text, tenant_id::text, attention_id::text, request_version, principal_id::text, kind, answer_digest, decided_at
		from decision where tenant_id = $1 and decision_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &requestText, &decision.RequestVersion, &principalText, &kind, &digestText, &decision.DecidedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Decision{}, ErrNotFound
		}
		return lifecycle.Decision{}, err
	}
	var parseErr error
	if decision.ID, parseErr = parseID[ids.Decision](`decision.decision_id`, idText); parseErr != nil {
		return lifecycle.Decision{}, parseErr
	}
	if decision.Tenant, parseErr = parseID[ids.Tenant](`decision.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Decision{}, parseErr
	}
	if decision.Request, parseErr = parseID[ids.Attention](`decision.attention_id`, requestText); parseErr != nil {
		return lifecycle.Decision{}, parseErr
	}
	if decision.Principal, parseErr = parseID[ids.Principal](`decision.principal_id`, principalText); parseErr != nil {
		return lifecycle.Decision{}, parseErr
	}
	if digestText != nil {
		decision.AnswerDigest = identity.Digest(*digestText)
	}
	decision.Kind = lifecycle.DecisionKind(kind)
	if err := decision.Validate(); err != nil {
		return lifecycle.Decision{}, err
	}
	return decision, nil
}
