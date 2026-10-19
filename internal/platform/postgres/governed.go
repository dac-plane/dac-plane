package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// GovernedAction is the digest-only result of one requested action. The
// execution is present even on denial but is executable only with an allowed use.
type GovernedAction struct {
	Digest    identity.Digest
	Execution contracts.ActionExecution
	Use       identity.AuthorityUse
	Hook      contracts.HookEvidence
	Receipt   *contracts.EnforcementReceipt
}

// RecordAuthorityChange retains issuance, retirement and revocation history
// alongside the grant change in the same transaction. Grants do not dispatch.
func RecordAuthorityChange(ctx context.Context, u *UnitOfWork, g identity.AuthorityGrant, event string, tr Transition) error {
	from, to, version := "requested", "issued", uint64(1)
	if event == "retire" {
		from, to, version = "issued", "retired", 2
	}
	if event == "revoke" {
		from, to, version = "issued", "revoked", 2
	}
	change := lifecycle.Change{Tenant: g.Tenant, Record: lifecycle.RecordAuthorityGrant, RecordID: g.ID.Untyped(), Event: event, From: from, To: to, Version: version}
	return appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), nil)
}

func GetAttemptForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AttemptID) (lifecycle.Attempt, error) {
	if err := lockRow(ctx, u, "attempt", "attempt_id", tenant.String(), id.String()); err != nil {
		return lifecycle.Attempt{}, err
	}
	return GetAttempt(ctx, u, tenant, id)
}

// LockGovernedAction serializes replay and first authorization for an operation.
func LockGovernedAction(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, operation ids.OperationID) error {
	_, err := u.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1,0))`, "action:"+tenant.String()+":"+operation.String())
	return err
}

// GetPolicyForUpdate fences authorization against concurrent policy changes.
func GetPolicyForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID) (identity.Policy, error) {
	var id string
	if err := u.QueryRow(ctx, `select tenant_id::text from policy where tenant_id=$1 for update`, tenant.String()).Scan(&id); err != nil {
		return identity.Policy{}, err
	}
	return GetPolicy(ctx, u, tenant)
}

// CheckGovernedDispatch fences queued actions against policy changes and
// revocation before their send marker. Retirement is never reversed in storage.
func CheckGovernedDispatch(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, operation ids.OperationID) (identity.DenialReason, error) {
	a, err := GetGovernedAction(ctx, u, tenant, operation)
	if err != nil {
		return "", err
	}
	policy, err := GetPolicyForUpdate(ctx, u, tenant)
	if err != nil {
		return "", err
	}
	if _, err = GetAttemptForUpdate(ctx, u, tenant, a.Use.Attempt); err != nil {
		return "", err
	}
	g, err := GetGrantForUpdate(ctx, u, tenant, a.Use.Grant)
	if err != nil {
		return "", err
	}
	// The send marker takes this lock too. Acquire it before the final time
	// check so a blocked operation write cannot outlive valid authority.
	if err := lockRow(ctx, u, "provider_operation", "operation_id", tenant.String(), operation.String()); err != nil {
		return "", err
	}
	if g.Mode == identity.GrantAllowOnce && g.Operation == operation && a.Use.Outcome == identity.UseAllowed {
		g.RetiredAt = time.Time{}
	}
	// Lock acquisition can outlast a grant. Evaluate time only after all
	// authorization locks are held, never against the batch or claim time.
	use, _, err := identity.Authorize(a.Execution.Request, &g, policy, time.Now().UTC())
	return use.Reason, err
}

func GetGovernedAction(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID) (GovernedAction, error) {
	var a GovernedAction
	var raw []byte
	var atText string
	err := q.QueryRow(ctx, `select request_digest, execution from governed_action where tenant_id=$1 and operation_id=$2`, tenant.String(), operation.String()).Scan(&atText, &raw)
	if isNoRows(err) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Digest = identity.Digest(atText)
	if err = json.Unmarshal(raw, &a.Execution); err != nil {
		return a, err
	}
	var decidedAt time.Time
	var action identity.ActionClass
	err = q.QueryRow(ctx, `select decided_at,action from authority_use where tenant_id=$1 and operation_id=$2 order by decided_at limit 1`, tenant.String(), operation.String()).Scan(&decidedAt, &action)
	if err != nil {
		return a, err
	}
	a.Use, err = GetUse(ctx, q, tenant, operation, decidedAt, action)
	if err != nil {
		return a, err
	}
	a.Hook, err = GetHookEvidence(ctx, q, tenant, operation)
	if err != nil {
		return a, err
	}
	var receiptRaw []byte
	err = q.QueryRow(ctx, `select receipt from enforcement_receipt where tenant_id=$1 and operation_id=$2`, tenant.String(), operation.String()).Scan(&receiptRaw)
	if isNoRows(err) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	var receipt contracts.EnforcementReceipt
	if err = json.Unmarshal(receiptRaw, &receipt); err != nil {
		return a, err
	}
	a.Receipt = &receipt
	return a, nil
}

// InsertGovernedAction commits denial evidence or an allowed dispatch atomically
// with retirement in the caller's transaction. It never persists raw input.
func InsertGovernedAction(ctx context.Context, u *UnitOfWork, a GovernedAction, tr Transition) error {
	raw, err := json.Marshal(a.Execution)
	if err != nil {
		return err
	}
	if _, err = u.Exec(ctx, `insert into governed_action(tenant_id,operation_id,attempt_id,request_digest,execution) values($1,$2,$3,$4,$5::jsonb)`, a.Use.Tenant.String(), a.Use.Operation.String(), a.Use.Attempt.String(), string(a.Digest), string(raw)); err != nil {
		return err
	}
	if _, _, err = InsertUse(ctx, u, a.Use); err != nil {
		return err
	}
	if _, _, err = InsertHookEvidence(ctx, u, a.Use.Tenant, a.Hook); err != nil {
		return err
	}
	change := lifecycle.Change{Tenant: a.Use.Tenant, Record: lifecycle.RecordAuthorityUse, RecordID: a.Use.Operation.Untyped(), Event: "authorize", From: "requested", To: string(a.Use.Outcome), Version: 1}
	return appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox)
}

func InsertEnforcementReceipt(ctx context.Context, u *UnitOfWork, r contracts.EnforcementReceipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	action, err := GetGovernedAction(ctx, u, r.Execution.Request.Tenant, r.Execution.Request.Operation)
	if err != nil {
		return err
	}
	expected, err := json.Marshal(action.Execution)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(r.Execution)
	if err != nil {
		return err
	}
	if action.Use.Outcome != identity.UseAllowed || string(expected) != string(actual) {
		return ErrPayloadMismatch
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	inserted, err := insertReturning(ctx, u, `insert into enforcement_receipt(tenant_id,operation_id,receipt) values($1,$2,$3::jsonb)`, r.Execution.Request.Tenant.String(), r.Execution.Request.Operation.String(), string(raw))
	if err != nil {
		return err
	}
	if inserted {
		return nil
	}
	stored, err := GetGovernedAction(ctx, u, r.Execution.Request.Tenant, r.Execution.Request.Operation)
	if err != nil {
		return err
	}
	if stored.Receipt == nil {
		return ErrPayloadMismatch
	}
	other, _ := json.Marshal(stored.Receipt)
	if string(other) != string(raw) {
		return ErrPayloadMismatch
	}
	return nil
}
