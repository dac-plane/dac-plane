package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/dac-plane/dac-plane/internal/progress"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConnectorDenied          = errors.New("connector call denied")
	ErrConnectorApprovalPending = errors.New("connector approval pending")
)

// ConnectorPolicy applies the connection owner's ceiling together with any
// explicit tenant setting or administrative ceiling. Targeted denies remain.
func ConnectorPolicy(p identity.Policy, action identity.ActionClass, permission identity.Permission) identity.Policy {
	p.Settings = maps.Clone(p.Settings)
	p.Ceiling = maps.Clone(p.Ceiling)
	if p.Settings == nil {
		p.Settings = map[identity.ActionClass]identity.Permission{}
	}
	if p.Ceiling == nil {
		p.Ceiling = map[identity.ActionClass]identity.Permission{}
	}
	for _, m := range []map[identity.ActionClass]identity.Permission{p.Settings, p.Ceiling} {
		v, ok := m[action]
		if !ok || (permission == identity.PermissionAsk && v == identity.PermissionAlways) {
			m[action] = permission
		}
	}
	return p
}

// ConnectorAttempt authenticates a gateway token's subject against current
// product state. A token for a completed or cancelling attempt authorizes nothing.
func ConnectorAttempt(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, id ids.AttemptID) (lifecycle.Attempt, error) {
	a, err := store.GetAttempt(ctx, pool, tenant, id)
	if err != nil {
		return a, err
	}
	op, err := store.GetOperationByAttempt(ctx, pool, tenant, id)
	if err != nil {
		return a, err
	}
	err = checkActionAttempt(ctx, pool, identity.ActionRequest{Tenant: tenant, Attempt: id, Provider: op.Provider})
	return a, err
}

type GrantedConnectorTool struct {
	Connection ids.ConnectionID
	Tool       contracts.ConnectorTool
}

// ConnectorTools exposes only currently usable connections owned by this work's owner.
func ConnectorTools(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, tenant ids.TenantID, attempt ids.AttemptID) ([]GrantedConnectorTool, error) {
	a, err := ConnectorAttempt(ctx, pool, tenant, attempt)
	if err != nil {
		return nil, err
	}
	task, err := store.GetTask(ctx, pool, tenant, a.Task)
	if err != nil {
		return nil, err
	}
	var agent ids.AgentID
	if task.Agent != nil {
		agent = task.Agent.Agent
	}
	grants, err := store.ListWorkGrants(ctx, pool, tenant, task.ID, agent)
	if err != nil {
		return nil, err
	}
	policy, err := store.GetPolicy(ctx, pool, tenant)
	if err != nil {
		return nil, err
	}
	out := []GrantedConnectorTool{}
	seen := map[string]bool{}
	for _, g := range grants {
		c, err := store.GetConnection(ctx, pool, tenant, g.Connection)
		if err != nil {
			return nil, err
		}
		if c.State != lifecycle.ConnectionActive || c.Owner != task.Owner || !time.Now().Before(g.ExpiresAt) {
			continue
		}
		catalogue, err := connectorTools(ctx, ports, c, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		for _, tool := range catalogue.Tools {
			key := c.ID.String() + ":" + tool.Name
			if seen[key] || connectorScopeDenied(policy, lifecycle.ConnectorAction(tool.Name), contracts.ConnectionScope(c.ID)) || !slices.Contains(g.Tools, tool.Name) || ConnectorPolicy(policy, lifecycle.ConnectorAction(tool.Name), g.Permission).Effective(lifecycle.ConnectorAction(tool.Name)) == identity.PermissionNever {
				continue
			}
			seen[key] = true
			out = append(out, GrantedConnectorTool{Connection: c.ID, Tool: tool})
		}
	}
	return out, nil
}

// CallConnector authorizes or resumes one exact-input call. Pending calls use
// their original operation and attention; completed calls can be invoked again.
// All authorization predicates, the call ceiling and the dispatch intent commit
// under policy, attempt, connection and grant locks in that order.
func CallConnector(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, attempt ids.AttemptID, connection ids.ConnectionID, tool string, input json.RawMessage) (store.ConnectorCall, error) {
	return callConnector(ctx, pool, tenant, attempt, connection, tool, input, ids.OperationID{})
}

// ResumeConnectorCall keeps a waiting gateway request bound to its original
// approval, including a denial, even when another covering grant appears.
func ResumeConnectorCall(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, attempt ids.AttemptID, operation ids.OperationID, input json.RawMessage) (store.ConnectorCall, error) {
	call, err := store.GetConnectorCall(ctx, pool, tenant, operation)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	if call.Attempt != attempt {
		return store.ConnectorCall{}, store.ErrNotFound
	}
	return callConnector(ctx, pool, tenant, attempt, call.Connection, call.Tool, input, operation)
}

func callConnector(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, attempt ids.AttemptID, connection ids.ConnectionID, tool string, input json.RawMessage, bound ids.OperationID) (store.ConnectorCall, error) {
	input, err := contracts.CanonicalJSON(input)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	if err := contracts.ValidateToolInput(input); err != nil {
		return store.ConnectorCall{}, err
	}
	if err := lifecycle.ValidateToolName(tool); err != nil {
		return store.ConnectorCall{}, err
	}
	u, err := store.Begin(ctx, pool)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	defer func() { _ = u.Rollback(ctx) }()
	policy, err := store.GetPolicyForUpdate(ctx, u, tenant)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	a, err := store.GetAttemptForUpdate(ctx, u, tenant, attempt)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	runtime, err := store.GetOperationByAttempt(ctx, u, tenant, attempt)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	if err := checkActionAttempt(ctx, u, identity.ActionRequest{Tenant: tenant, Attempt: attempt, Provider: runtime.Provider}); err != nil {
		return store.ConnectorCall{}, err
	}
	task, err := store.GetTask(ctx, u, tenant, a.Task)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	c, err := store.GetConnectionForShare(ctx, u, tenant, connection)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	operation, err := ids.New[ids.Operation]()
	if err != nil {
		return store.ConnectorCall{}, err
	}
	request := identity.ActionRequest{Tenant: tenant, Attempt: attempt, Provider: c.Provider, Operation: operation, Action: lifecycle.ConnectorAction(tool), Scope: contracts.ConnectionScope(connection), InputDigest: identity.DigestOf(input)}
	tr, err := connectorTransition(now)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	deny := func(reason ...identity.DenialReason) (store.ConnectorCall, error) {
		use, _, err := identity.Authorize(request, nil, policy, now)
		if err != nil {
			return store.ConnectorCall{}, err
		}
		if len(reason) > 0 {
			use.Reason = reason[0]
		}
		if err = store.RecordConnectorUse(ctx, u, use, tr); err != nil {
			return store.ConnectorCall{}, err
		}
		if err = u.Commit(ctx); err != nil {
			return store.ConnectorCall{}, err
		}
		return store.ConnectorCall{}, ErrConnectorDenied
	}
	if c.Owner != task.Owner || c.State != lifecycle.ConnectionActive {
		return deny()
	}
	var agent ids.AgentID
	if task.Agent != nil {
		agent = task.Agent.Agent
	}
	grants, err := store.ListWorkGrants(ctx, u, tenant, task.ID, agent)
	if err != nil {
		return store.ConnectorCall{}, err
	}
	var call store.ConnectorCall
	if bound.IsZero() {
		call, err = store.FindConnectorCall(ctx, u, tenant, attempt, connection, tool, request.InputDigest)
	} else {
		call, err = store.GetConnectorCall(ctx, u, tenant, bound)
		if err == nil && (call.Attempt != attempt || call.Connection != connection || call.Tool != tool || call.InputDigest != request.InputDigest) {
			return store.ConnectorCall{}, ErrConnectorDenied
		}
		if err != nil {
			return store.ConnectorCall{}, err
		}
		if call.State == store.CallDenied {
			return call, ErrConnectorDenied
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.ConnectorCall{}, err
	}
	found := err == nil
	existing := found && (call.Result == nil || !bound.IsZero()) && (call.State == store.CallAwaitingApproval || call.State == store.CallAuthorized)
	var grant *lifecycle.ConnectionGrant
	exhausted := false
	for _, g := range grants {
		if g.Connection != connection || !slices.Contains(g.Tools, tool) || (existing && g.ID != call.ConnectionGrant) || (found && call.State == store.CallDenied && g.ID == call.ConnectionGrant) {
			continue
		}
		locked, err := store.GetConnectionGrantForUpdate(ctx, u, tenant, g.ID)
		if err != nil {
			return store.ConnectorCall{}, err
		}
		now = time.Now().UTC().Truncate(time.Microsecond)
		if locked.State != lifecycle.ConnectionGrantActive || !now.Before(locked.ExpiresAt) {
			continue
		}
		if !existing || call.State != store.CallAuthorized {
			count, err := store.CountAuthorizedCalls(ctx, u, tenant, task.ID, g.ID)
			if err != nil {
				return store.ConnectorCall{}, err
			}
			if count >= locked.CallLimit {
				exhausted = true
				continue
			}
		}
		grant = &locked
		break
	}
	if grant == nil {
		if exhausted {
			return deny(identity.DenialAboveCeiling)
		}
		return deny(identity.DenialNoGrant)
	}
	policy = ConnectorPolicy(policy, request.Action, grant.Permission)
	if policy.Effective(request.Action) == identity.PermissionNever || connectorScopeDenied(policy, request.Action, request.Scope) {
		return deny()
	}
	if existing {
		if call.State == store.CallAuthorized {
			return call, u.Commit(ctx)
		}
		request.Operation = call.Operation
	} else {
		call = store.ConnectorCall{Tenant: tenant, Operation: operation, Attempt: attempt, Task: task.ID, Connection: connection, ConnectionGrant: grant.ID, Provider: c.Provider, Tool: tool, InputDigest: request.InputDigest, CreatedAt: now, UpdatedAt: now}
	}
	var authority *identity.AuthorityGrant
	if policy.Effective(request.Action) == identity.PermissionAlways {
		id, err := ids.New[ids.Grant]()
		if err != nil {
			return call, err
		}
		g := identity.AuthorityGrant{ID: id, Tenant: tenant, Attempt: attempt, Provider: c.Provider, Action: request.Action, Mode: identity.GrantStanding, Scope: request.Scope, GrantedBy: grant.GrantedBy, PolicyGeneration: policy.Generation, IssuedAt: now, ExpiresAt: grant.ExpiresAt}
		if err := identity.IssueGrantForScope(g, request.Scope, policy); err != nil {
			return deny()
		}
		if err := store.InsertGrant(ctx, u, g); err != nil {
			return call, err
		}
		if err := store.RecordAuthorityChange(ctx, u, g, "issue", tr); err != nil {
			return call, err
		}
		authority = &g
	} else if existing {
		issued, err := store.ListGrantsByAttempt(ctx, u, tenant, attempt)
		if err != nil {
			return call, err
		}
		for _, g := range issued {
			if g.Mode == identity.GrantAllowOnce && g.Operation == call.Operation && g.RetiredAt.IsZero() {
				locked, err := store.GetGrantForUpdate(ctx, u, tenant, g.ID)
				if err != nil {
					return call, err
				}
				authority = &locked
				break
			}
		}
		if authority == nil {
			native, err := store.GetAttentionNative(ctx, u, tenant, c.Provider, "connector:"+call.Operation.String())
			if err != nil {
				return call, err
			}
			attention, err := store.GetAttention(ctx, u, tenant, native.Attention)
			if err != nil {
				return call, err
			}
			if attention.State != lifecycle.AttentionPending {
				return deny()
			}
			return call, ErrConnectorApprovalPending
		}
	} else {
		open, err := store.ListOpenAttentionByAttempt(ctx, u, tenant, attempt)
		if err != nil {
			return call, err
		}
		if len(open) > 0 {
			return store.ConnectorCall{}, ErrConnectorApprovalPending
		}
		call.State = store.CallAwaitingApproval
		if err := store.InsertConnectorCall(ctx, u, call); err != nil {
			return call, err
		}
		id, err := ids.New[ids.Attention]()
		if err != nil {
			return call, err
		}
		attention, err := lifecycle.NewAttentionRequest(lifecycle.AttentionRequest{ID: id, Tenant: tenant, Task: task.ID, Attempt: attempt, Kind: lifecycle.AttentionActionApproval, Provider: c.Provider, Operation: call.Operation, Scope: request.Scope, ActionDigest: request.InputDigest, PolicyGeneration: policy.Generation, Responders: []ids.PrincipalID{task.Owner}, ExpiresAt: minTime(grant.ExpiresAt, now.Add(10*time.Minute))}, now)
		if err != nil {
			return call, err
		}
		if _, _, err := store.InsertAttention(ctx, u, attention); err != nil {
			return call, err
		}
		if _, _, err := store.InsertAttentionNative(ctx, u, store.AttentionNative{Tenant: tenant, Provider: c.Provider, NativeRef: "connector:" + call.Operation.String(), Attention: id, ActionClass: request.Action, CreatedAt: now}); err != nil {
			return call, err
		}
		step := progress.Step{UOW: u, Tenant: tenant, Actor: tr.Actor, At: now}
		if _, _, _, err := step.Attempt(ctx, attempt, lifecycle.AttemptAwaitInput, ""); err != nil {
			return call, err
		}
		if _, err := step.Task(ctx, task.ID, lifecycle.TaskAwaitInput); err != nil {
			return call, err
		}
		if err := u.Commit(ctx); err != nil {
			return call, err
		}
		return call, ErrConnectorApprovalPending
	}
	use, retired, err := identity.Authorize(request, authority, policy, now)
	if err != nil {
		return call, err
	}
	if use.Outcome != identity.UseAllowed {
		return deny()
	}
	if retired != nil {
		if _, _, err := store.RetireGrant(ctx, u, tenant, retired.ID, now); err != nil {
			return call, err
		}
		if err := store.RecordAuthorityChange(ctx, u, *retired, "retire", tr); err != nil {
			return call, err
		}
	}
	execution := contracts.ConnectorExecution{Request: request, Grant: authority.ID, Decision: authority.Decision, Source: authority.Mode.Source(), ConnectionGrant: grant.ID, Connection: connection, Tool: tool, Input: input}
	if err := execution.Validate(); err != nil {
		return call, err
	}
	if existing {
		if err := store.DecideConnectorCall(ctx, u, tenant, call.Operation, &execution, now); err != nil {
			return call, err
		}
	} else {
		call.State = store.CallAuthorized
		call.Execution = &execution
		if err := store.InsertConnectorCall(ctx, u, call); err != nil {
			return call, err
		}
	}
	call.State = store.CallAuthorized
	call.Execution = &execution
	op, err := lifecycle.NewProviderOperation(call.Operation, tenant, attempt, c.Provider, now)
	if err != nil {
		return call, err
	}
	if _, _, err := store.InsertOperation(ctx, u, op); err != nil {
		return call, err
	}
	body, err := json.Marshal(struct {
		Operation ids.OperationID  `json:"operation"`
		Role      contracts.Role   `json:"role"`
		Method    contracts.Method `json:"method"`
		Deadline  time.Time        `json:"deadline"`
	}{op.ID, contracts.RoleConnector, contracts.MethodCallTool, minTime(authority.ExpiresAt, minTime(grant.ExpiresAt, now.Add(2*time.Minute)))})
	if err != nil {
		return call, err
	}
	tr, err = connectorTransition(now)
	if err != nil {
		return call, err
	}
	tr.Outbox = &store.OutboxPayload{ID: op.ID.Untyped(), Attempt: attempt, Demand: contracts.DemandPerson, Body: body}
	if err := store.RecordConnectorUse(ctx, u, use, tr); err != nil {
		return call, err
	}

	return call, u.Commit(ctx)
}

func connectorTransition(at time.Time) (store.Transition, error) {
	id, err := ids.New[ids.Audit]()
	return store.Transition{At: at, AuditID: id, Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}}, err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ConnectorCallResult returns an authorized call's current outcome. An unknown
// external effect stays uncertain; the gateway never retries it as a new effect.
func ConnectorCallResult(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, operation ids.OperationID) (store.ConnectorCall, error) {
	c, err := store.GetConnectorCall(ctx, pool, tenant, operation)
	if err != nil {
		return c, err
	}
	if c.Result != nil {
		return c, nil
	}
	op, err := store.GetOperation(ctx, pool, tenant, operation)
	if err != nil {
		return c, err
	}
	if op.State == lifecycle.OperationUncertain {
		return c, fmt.Errorf("connector outcome uncertain")
	}
	if op.State == lifecycle.OperationFailed || op.State == lifecycle.OperationRejected {
		return c, ErrConnectorDenied
	}
	return c, nil
}

// CheckConnectorDispatch rechecks mutable authority before the durable send
// marker. A consumed one-time approval remains usable only by its own queued
// operation; retirement is never undone in storage.
func CheckConnectorDispatch(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, operation ids.OperationID) (identity.DenialReason, error) {
	c, err := store.GetConnectorCall(ctx, u, tenant, operation)
	if err != nil {
		return "", err
	}
	if c.Execution == nil || c.State != store.CallAuthorized {
		return identity.DenialNoGrant, nil
	}
	policy, err := store.GetPolicyForUpdate(ctx, u, tenant)
	if err != nil {
		return "", err
	}
	a, err := store.GetAttemptForUpdate(ctx, u, tenant, c.Attempt)
	if err != nil {
		return "", err
	}
	if a.State != lifecycle.AttemptExecuting && a.State != lifecycle.AttemptWaitingInput {
		return identity.DenialRevoked, nil
	}
	connection, err := store.GetConnectionForShare(ctx, u, tenant, c.Connection)
	if err != nil {
		return "", err
	}
	g, err := store.GetConnectionGrantForUpdate(ctx, u, tenant, c.ConnectionGrant)
	if err != nil {
		return "", err
	}
	if connection.State != lifecycle.ConnectionActive || g.State != lifecycle.ConnectionGrantActive || !time.Now().Before(g.ExpiresAt) {
		return identity.DenialRevoked, nil
	}
	authority, err := store.GetGrantForUpdate(ctx, u, tenant, c.Execution.Grant)
	if err != nil {
		return "", err
	}
	if authority.Mode == identity.GrantAllowOnce && authority.Operation == operation {
		authority.RetiredAt = time.Time{}
	}
	policy = ConnectorPolicy(policy, c.Execution.Request.Action, g.Permission)
	reservationID, err := ids.Parse[ids.Reservation](c.Attempt.String())
	if err != nil {
		return "", err
	}
	reservation, err := store.GetReservation(ctx, u, tenant, reservationID)
	if err != nil {
		return "", err
	}
	budget, err := store.GetBudgetForUpdate(ctx, u, tenant, reservation.Budget)
	if err != nil {
		return "", err
	}
	if reservation.State != lifecycle.ReservationHeld || budget.Consumed+budget.Reserved > budget.Ceiling {
		return identity.DenialAboveCeiling, nil
	}
	if _, err := store.GetOperationForUpdate(ctx, u, tenant, operation); err != nil {
		return "", err
	}
	use, _, err := identity.Authorize(c.Execution.Request, &authority, policy, time.Now().UTC())
	if err != nil {
		return "", err
	}
	if use.Outcome != identity.UseAllowed {
		return use.Reason, nil
	}
	return "", nil
}

func connectorScopeDenied(p identity.Policy, action identity.ActionClass, scope identity.Scope) bool {
	for _, d := range p.Denies {
		if d.Action == action && d.Target == scope.Target {
			return true
		}
	}
	return false
}

// ExpireConnectorApprovals closes waits that can no longer authorize a call.
// Revocation cancels a wait; timeout expires it. Neither creates an effect.
func ExpireConnectorApprovals(ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID) error {
	available, err := store.ConnectorSchemaAvailable(ctx, pool)
	if err != nil || !available {
		return err
	}
	rows, err := pool.Query(ctx, `select n.attention_id::text,c.operation_id::text,c.attempt_id::text from connector_call c join attention_native n on n.tenant_id=c.tenant_id and n.native_ref='connector:'||c.operation_id::text join attention_request a on a.tenant_id=n.tenant_id and a.attention_id=n.attention_id join connection_grant g on g.tenant_id=c.tenant_id and g.grant_id=c.connection_grant_id join connection x on x.tenant_id=c.tenant_id and x.connection_id=c.connection_id where c.tenant_id=$1 and c.state='awaiting_approval' and a.state='pending' and (a.expires_at<=clock_timestamp() or g.expires_at<=clock_timestamp() or g.state<>'active' or x.state<>'active') limit 100`, tenant.String())
	if err != nil {
		return err
	}
	type expired struct {
		attention ids.AttentionID
		operation ids.OperationID
		attempt   ids.AttemptID
	}
	var pending []expired
	for rows.Next() {
		var attention, operation, attempt string
		if err := rows.Scan(&attention, &operation, &attempt); err != nil {
			rows.Close()
			return err
		}
		aid, e := ids.Parse[ids.Attention](attention)
		if e != nil {
			rows.Close()
			return e
		}
		oid, e := ids.Parse[ids.Operation](operation)
		if e != nil {
			rows.Close()
			return e
		}
		at, e := ids.Parse[ids.Attempt](attempt)
		if e != nil {
			rows.Close()
			return e
		}
		pending = append(pending, expired{aid, oid, at})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range pending {
		u, err := store.Begin(ctx, pool)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = u.Rollback(ctx) }()
			attempt, err := store.GetAttemptForUpdate(ctx, u, tenant, p.attempt)
			if err != nil {
				return err
			}
			attention, err := store.GetAttentionForUpdate(ctx, u, tenant, p.attention)
			if err != nil {
				return err
			}
			if attention.State != lifecycle.AttentionPending {
				return nil
			}
			call, err := store.GetConnectorCall(ctx, u, tenant, p.operation)
			if err != nil {
				return err
			}
			connection, err := store.GetConnectionForShare(ctx, u, tenant, call.Connection)
			if err != nil {
				return err
			}
			grant, err := store.GetConnectionGrantForUpdate(ctx, u, tenant, call.ConnectionGrant)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			event := lifecycle.AttentionCancel
			if !now.Before(attention.ExpiresAt) {
				event = lifecycle.AttentionExpire
			} else if connection.State == lifecycle.ConnectionActive && grant.State == lifecycle.ConnectionGrantActive && now.Before(grant.ExpiresAt) {
				return nil
			}
			tr, err := connectorTransition(now)
			if err != nil {
				return err
			}
			tr.ExpectedVersion = attention.Version
			if _, _, err := store.ApplyAttention(ctx, u, tenant, attention.ID, event, tr); err != nil {
				return err
			}
			if err := store.DecideConnectorCall(ctx, u, tenant, p.operation, nil, now); err != nil {
				return err
			}
			if attempt.State == lifecycle.AttemptWaitingInput {
				step := progress.Step{UOW: u, Tenant: tenant, Actor: tr.Actor, At: now}
				if _, _, _, err := step.Attempt(ctx, p.attempt, lifecycle.AttemptResume, ""); err != nil {
					return err
				}
				if _, err := step.Task(ctx, attempt.Task, lifecycle.TaskInputResolved); err != nil {
					return err
				}
			}
			return u.Commit(ctx)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
