package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// connectorSchemaVersion is the first schema with connections (ADR-0022). A
// binary serving an earlier schema during a rollout refuses connector
// operations and serves everything else (ADR-0011).
const connectorSchemaVersion = 18

func requireConnectorSchema(ctx context.Context, q Querier) error {
	version, err := SchemaVersion(ctx, q)
	if err != nil {
		return err
	}
	if version < connectorSchemaVersion {
		return ErrSchemaUnavailable
	}
	return nil
}

// ConnectorSchemaAvailable reports whether the database has connections.
func ConnectorSchemaAvailable(ctx context.Context, q Querier) (bool, error) {
	err := requireConnectorSchema(ctx, q)
	if errors.Is(err, ErrSchemaUnavailable) {
		return false, nil
	}
	return err == nil, err
}

const connectionColumns = `connection_id::text, tenant_id::text, provider_id::text, owner_id::text, name, state,
	account, permissions, version, created_at, updated_at`

// InsertConnection persists a new connection awaiting its first consent.
// Creation is recorded by the command record that made it, as a task's is.
func InsertConnection(ctx context.Context, u *UnitOfWork, c lifecycle.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.State != lifecycle.ConnectionAuthorizing || c.Version != 1 || c.Account != nil {
		return lifecycle.ErrInvalid
	}
	if err := requireConnectorSchema(ctx, u); err != nil {
		return err
	}
	_, err := u.Exec(ctx, `
		insert into connection (tenant_id, connection_id, provider_id, owner_id, name, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.Tenant.String(), c.ID.String(), c.Provider.String(), c.Owner.String(), c.Name, string(c.State),
		int64(c.Version), c.CreatedAt, c.UpdatedAt)
	return err
}

// GetConnection loads a connection. A database before the connector schema
// has none.
func GetConnection(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ConnectionID) (lifecycle.Connection, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return lifecycle.Connection{}, ErrNotFound
		}
		return lifecycle.Connection{}, err
	}
	return scanConnection(q.QueryRow(ctx, `select `+connectionColumns+` from connection where tenant_id = $1 and connection_id = $2`,
		tenant.String(), id.String()))
}

// GetConnectionForShare loads a connection and holds a share lock on it until
// the transaction ends, so a grant or call decided under it cannot pass a
// concurrent removal.
func GetConnectionForShare(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionID) (lifecycle.Connection, error) {
	if err := requireConnectorSchema(ctx, u); err != nil {
		return lifecycle.Connection{}, err
	}
	return scanConnection(u.QueryRow(ctx, `select `+connectionColumns+` from connection where tenant_id = $1 and connection_id = $2 for share`,
		tenant.String(), id.String()))
}

// ListConnections lists a tenant's connections, removed ones included,
// oldest first.
func ListConnections(ctx context.Context, q Querier, tenant ids.TenantID) ([]lifecycle.Connection, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return []lifecycle.Connection{}, nil
		}
		return nil, err
	}
	rows, err := q.Query(ctx, `select `+connectionColumns+` from connection where tenant_id = $1 order by created_at, connection_id`, tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []lifecycle.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CompleteConnection records the account a consent produced, at the expected
// version, with audit and durable event rows. A lifecycle refusal is audited.
func CompleteConnection(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionID, account lifecycle.ConnectionAccount, tr Transition) (lifecycle.Connection, error) {
	return transitionConnection(ctx, u, tenant, id, tr, func(c lifecycle.Connection) (lifecycle.Connection, lifecycle.Change, error) {
		return c.Complete(tr.Actor.Principal, account, tr.ExpectedVersion, tr.At)
	})
}

// RemoveConnection starts a connection's removal at the expected version.
func RemoveConnection(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionID, tr Transition) (lifecycle.Connection, error) {
	return transitionConnection(ctx, u, tenant, id, tr, func(c lifecycle.Connection) (lifecycle.Connection, lifecycle.Change, error) {
		return c.Remove(tr.Actor.Principal, tr.ExpectedVersion, tr.At)
	})
}

// FinishConnectionRemoval records that the connector revoked and destroyed
// the credential.
func FinishConnectionRemoval(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionID, tr Transition) (lifecycle.Connection, error) {
	return transitionConnection(ctx, u, tenant, id, tr, func(c lifecycle.Connection) (lifecycle.Connection, lifecycle.Change, error) {
		return c.FinishRemoval(tr.ExpectedVersion, tr.At)
	})
}

func transitionConnection(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionID, tr Transition, apply func(lifecycle.Connection) (lifecycle.Connection, lifecycle.Change, error)) (lifecycle.Connection, error) {
	if err := requireConnectorSchema(ctx, u); err != nil {
		return lifecycle.Connection{}, err
	}
	c, err := scanConnection(u.QueryRow(ctx, `select `+connectionColumns+` from connection where tenant_id = $1 and connection_id = $2 for update`,
		tenant.String(), id.String()))
	if err != nil {
		return lifecycle.Connection{}, err
	}
	next, change, err := apply(c)
	if err != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(err, &refusal) {
			if histErr := appendRefusal(ctx, u, refusal.Audit(tr.AuditID, tr.Actor, tr.At)); histErr != nil {
				return lifecycle.Connection{}, histErr
			}
		}
		return c, err
	}
	account, permissions, err := accountColumns(next.Account)
	if err != nil {
		return lifecycle.Connection{}, err
	}
	if err := execVersionedUpdate(ctx, u, `
		update connection set state = $3, account = $4, permissions = $5::jsonb, version = $6, updated_at = $7
		where tenant_id = $1 and connection_id = $2 and version = $8`,
		tenant.String(), id.String(), string(next.State), account, permissions, int64(next.Version), next.UpdatedAt, int64(c.Version),
	); err != nil {
		return lifecycle.Connection{}, err
	}
	if err := appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox); err != nil {
		return lifecycle.Connection{}, err
	}
	return next, nil
}

func accountColumns(a *lifecycle.ConnectionAccount) (any, any, error) {
	if a == nil {
		return nil, nil, nil
	}
	permissions := a.Permissions
	if permissions == nil {
		permissions = []string{}
	}
	raw, err := json.Marshal(permissions)
	if err != nil {
		return nil, nil, err
	}
	return a.Account, string(raw), nil
}

func scanConnection(row interface{ Scan(...any) error }) (lifecycle.Connection, error) {
	var c lifecycle.Connection
	var idText, tenantText, providerText, ownerText, state string
	var account *string
	var permissions []byte
	err := row.Scan(&idText, &tenantText, &providerText, &ownerText, &c.Name, &state, &account, &permissions, &c.Version, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Connection{}, ErrNotFound
		}
		return lifecycle.Connection{}, err
	}
	if c.ID, err = parseID[ids.Connection]("connection.connection_id", idText); err != nil {
		return lifecycle.Connection{}, err
	}
	if c.Tenant, err = parseID[ids.Tenant]("connection.tenant_id", tenantText); err != nil {
		return lifecycle.Connection{}, err
	}
	if c.Provider, err = parseID[ids.Provider]("connection.provider_id", providerText); err != nil {
		return lifecycle.Connection{}, err
	}
	if c.Owner, err = parseID[ids.Principal]("connection.owner_id", ownerText); err != nil {
		return lifecycle.Connection{}, err
	}
	c.State = lifecycle.ConnectionState(state)
	if account != nil {
		c.Account = &lifecycle.ConnectionAccount{Account: *account}
		if err := json.Unmarshal(permissions, &c.Account.Permissions); err != nil {
			return lifecycle.Connection{}, corruptRow("connection", err)
		}
	}
	if err := c.Validate(); err != nil {
		return lifecycle.Connection{}, corruptRow("connection", err)
	}
	return c, nil
}

const connectionGrantColumns = `grant_id::text, tenant_id::text, connection_id::text, agent_id::text, task_id::text, tools,
	permission, call_limit, state, granted_by::text, version, expires_at, created_at, updated_at`

// InsertConnectionGrant persists a new grant. The connection must be held
// for share by the caller's transaction.
func InsertConnectionGrant(ctx context.Context, u *UnitOfWork, g lifecycle.ConnectionGrant) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if g.State != lifecycle.ConnectionGrantActive || g.Version != 1 {
		return lifecycle.ErrInvalid
	}
	if err := requireConnectorSchema(ctx, u); err != nil {
		return err
	}
	tools, err := json.Marshal(g.Tools)
	if err != nil {
		return err
	}
	_, err = u.Exec(ctx, `
		insert into connection_grant (tenant_id, grant_id, connection_id, agent_id, task_id, tools, permission, call_limit,
		                              state, granted_by, version, expires_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13, $14)`,
		g.Tenant.String(), g.ID.String(), g.Connection.String(), idOrNil(g.Agent), idOrNil(g.Task), string(tools),
		string(g.Permission), int64(g.CallLimit), string(g.State), g.GrantedBy.String(), int64(g.Version),
		g.ExpiresAt, g.CreatedAt, g.UpdatedAt)
	return err
}

func idOrNil[K any](id ids.ID[K]) any {
	if id.IsZero() {
		return nil
	}
	return id.String()
}

// GetConnectionGrant loads a grant.
func GetConnectionGrant(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ConnectionGrantID) (lifecycle.ConnectionGrant, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return lifecycle.ConnectionGrant{}, ErrNotFound
		}
		return lifecycle.ConnectionGrant{}, err
	}
	return scanConnectionGrant(q.QueryRow(ctx, `select `+connectionGrantColumns+` from connection_grant where tenant_id = $1 and grant_id = $2`,
		tenant.String(), id.String()))
}

// ListConnectionGrants lists a connection's grants, revoked ones included,
// oldest first.
func ListConnectionGrants(ctx context.Context, q Querier, tenant ids.TenantID, connection ids.ConnectionID) ([]lifecycle.ConnectionGrant, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return []lifecycle.ConnectionGrant{}, nil
		}
		return nil, err
	}
	return queryConnectionGrants(ctx, q, `select `+connectionGrantColumns+` from connection_grant
		where tenant_id = $1 and connection_id = $2 order by created_at, grant_id`, tenant.String(), connection.String())
}

// ListWorkGrants lists the active grants naming task or agent, oldest first:
// the grants a task's attempt may call under, before expiry, tool and
// ownership are checked. A zero agent matches no agent grant.
func ListWorkGrants(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID, agent ids.AgentID) ([]lifecycle.ConnectionGrant, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return []lifecycle.ConnectionGrant{}, nil
		}
		return nil, err
	}
	return queryConnectionGrants(ctx, q, `select `+connectionGrantColumns+` from connection_grant
		where tenant_id = $1 and state = 'active' and (task_id = $2 or agent_id = $3)
		order by created_at, grant_id`, tenant.String(), task.String(), idOrNil(agent))
}

func queryConnectionGrants(ctx context.Context, q Querier, stmt string, args ...any) ([]lifecycle.ConnectionGrant, error) {
	rows, err := q.Query(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []lifecycle.ConnectionGrant{}
	for rows.Next() {
		g, err := scanConnectionGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeConnectionGrant revokes a grant at the expected version, with audit
// and durable event rows. A lifecycle refusal is audited.
func RevokeConnectionGrant(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionGrantID, tr Transition) (lifecycle.ConnectionGrant, error) {
	if err := requireConnectorSchema(ctx, u); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	g, err := scanConnectionGrant(u.QueryRow(ctx, `select `+connectionGrantColumns+` from connection_grant where tenant_id = $1 and grant_id = $2 for update`,
		tenant.String(), id.String()))
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	next, change, err := g.Revoke(tr.Actor.Principal, tr.ExpectedVersion, tr.At)
	if err != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(err, &refusal) {
			if histErr := appendRefusal(ctx, u, refusal.Audit(tr.AuditID, tr.Actor, tr.At)); histErr != nil {
				return lifecycle.ConnectionGrant{}, histErr
			}
		}
		return g, err
	}
	if err := execVersionedUpdate(ctx, u, `
		update connection_grant set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and grant_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(g.Version),
	); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if err := appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	return next, nil
}

func scanConnectionGrant(row interface{ Scan(...any) error }) (lifecycle.ConnectionGrant, error) {
	var g lifecycle.ConnectionGrant
	var idText, tenantText, connectionText, granterText, permission, state string
	var agentText, taskText *string
	var tools []byte
	var limit int64
	err := row.Scan(&idText, &tenantText, &connectionText, &agentText, &taskText, &tools, &permission, &limit, &state,
		&granterText, &g.Version, &g.ExpiresAt, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ConnectionGrant{}, ErrNotFound
		}
		return lifecycle.ConnectionGrant{}, err
	}
	if g.ID, err = parseID[ids.ConnGrant]("connection_grant.grant_id", idText); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if g.Tenant, err = parseID[ids.Tenant]("connection_grant.tenant_id", tenantText); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if g.Connection, err = parseID[ids.Connection]("connection_grant.connection_id", connectionText); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if g.GrantedBy, err = parseID[ids.Principal]("connection_grant.granted_by", granterText); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if agentText != nil {
		if g.Agent, err = parseID[ids.Agent]("connection_grant.agent_id", *agentText); err != nil {
			return lifecycle.ConnectionGrant{}, err
		}
	}
	if taskText != nil {
		if g.Task, err = parseID[ids.Task]("connection_grant.task_id", *taskText); err != nil {
			return lifecycle.ConnectionGrant{}, err
		}
	}
	if err := json.Unmarshal(tools, &g.Tools); err != nil {
		return lifecycle.ConnectionGrant{}, corruptRow("connection_grant", err)
	}
	g.Permission = identity.Permission(permission)
	g.CallLimit = uint32(limit)
	g.State = lifecycle.ConnectionGrantState(state)
	if err := g.Validate(); err != nil {
		return lifecycle.ConnectionGrant{}, corruptRow("connection_grant", err)
	}
	return g, nil
}

// ConnectorCallState is where the gateway's decision on one call stands.
type ConnectorCallState string

const (
	CallAwaitingApproval ConnectorCallState = "awaiting_approval"
	CallDenied           ConnectorCallState = "denied"
	CallAuthorized       ConnectorCallState = "authorized"
)

// ConnectorCall is the gateway's record of one tool call. Operation is the
// call's stable identity: the attention request of an approval names it, and
// an authorized call becomes a provider operation of the connector under it.
// Execution, with the exact input, is set once the call is authorized; Result
// once the connector answers.
type ConnectorCall struct {
	Tenant          ids.TenantID
	Operation       ids.OperationID
	Attempt         ids.AttemptID
	Task            ids.TaskID
	Connection      ids.ConnectionID
	ConnectionGrant ids.ConnectionGrantID
	Provider        ids.ProviderID
	Tool            string
	InputDigest     identity.Digest
	State           ConnectorCallState
	Execution       *contracts.ConnectorExecution
	Result          *contracts.ConnectorResult
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const connectorCallColumns = `tenant_id::text, operation_id::text, attempt_id::text, task_id::text, connection_id::text,
	connection_grant_id::text, provider_id::text, tool, input_digest, state, execution, output, output_digest, is_error,
	completed_at, created_at, updated_at`

// InsertConnectorCall records the gateway's decision on a new call.
func InsertConnectorCall(ctx context.Context, u *UnitOfWork, c ConnectorCall) error {
	if err := requireConnectorSchema(ctx, u); err != nil {
		return err
	}
	if (c.State == CallAuthorized) != (c.Execution != nil) || c.Result != nil {
		return lifecycle.ErrInvalid
	}
	if err := c.matchesExecution(c.Execution); err != nil {
		return err
	}
	execution, err := executionColumn(c.Execution)
	if err != nil {
		return err
	}
	_, err = u.Exec(ctx, `
		insert into connector_call (tenant_id, operation_id, attempt_id, task_id, connection_id, connection_grant_id,
		                            provider_id, tool, input_digest, state, execution, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		c.Tenant.String(), c.Operation.String(), c.Attempt.String(), c.Task.String(), c.Connection.String(),
		c.ConnectionGrant.String(), c.Provider.String(), c.Tool, string(c.InputDigest), string(c.State), execution, c.CreatedAt)
	return err
}

// matchesExecution keeps the recorded decision bound to the exact execution.
func (c ConnectorCall) matchesExecution(e *contracts.ConnectorExecution) error {
	if e == nil {
		return nil
	}
	r := e.Request
	if r.Tenant != c.Tenant || r.Operation != c.Operation || r.Attempt != c.Attempt || r.Provider != c.Provider ||
		e.Connection != c.Connection || e.ConnectionGrant != c.ConnectionGrant || e.Tool != c.Tool || r.InputDigest != c.InputDigest {
		return ErrPayloadMismatch
	}
	return nil
}

func executionColumn(e *contracts.ConnectorExecution) (any, error) {
	if e == nil {
		return nil, nil
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// DecideConnectorCall moves a call awaiting approval to authorized, with its
// execution, or to denied.
func DecideConnectorCall(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, operation ids.OperationID, execution *contracts.ConnectorExecution, at time.Time) error {
	state := CallDenied
	if execution != nil {
		state = CallAuthorized
	}
	c, err := GetConnectorCall(ctx, u, tenant, operation)
	if err != nil {
		return err
	}
	if err := c.matchesExecution(execution); err != nil {
		return err
	}
	column, err := executionColumn(execution)
	if err != nil {
		return err
	}
	tag, err := u.Exec(ctx, `
		update connector_call set state = $3, execution = $4, updated_at = $5
		where tenant_id = $1 and operation_id = $2 and state = 'awaiting_approval'`,
		tenant.String(), operation.String(), string(state), column, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleVersion
	}
	return nil
}

// RecordConnectorResult stores the connector's answer to an authorized call,
// once. A second, identical answer is a replay; a different one mismatches.
func RecordConnectorResult(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, result contracts.ConnectorResult) (bool, error) {
	if err := result.Validate(); err != nil {
		return false, err
	}
	tag, err := u.Exec(ctx, `
		update connector_call set output = $3, output_digest = $4, is_error = $5, completed_at = $6, updated_at = $6
		where tenant_id = $1 and operation_id = $2 and state = 'authorized' and output is null`,
		tenant.String(), result.Operation.String(), string(result.Output), string(result.OutputDigest), result.IsError, result.CompletedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return false, nil
	}
	stored, err := GetConnectorCall(ctx, u, tenant, result.Operation)
	if err != nil {
		return false, err
	}
	if stored.Result != nil && stored.Result.OutputDigest == result.OutputDigest && stored.Result.IsError == result.IsError {
		return true, nil
	}
	return false, ErrPayloadMismatch
}

// GetConnectorCall loads a call by its operation.
func GetConnectorCall(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID) (ConnectorCall, error) {
	if err := requireConnectorSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return ConnectorCall{}, ErrNotFound
		}
		return ConnectorCall{}, err
	}
	return scanConnectorCall(q.QueryRow(ctx, `select `+connectorCallColumns+` from connector_call where tenant_id = $1 and operation_id = $2`,
		tenant.String(), operation.String()))
}

// FindConnectorCall loads the latest call of an attempt with this exact
// tool, connection and input: the record a retried call resumes.
func FindConnectorCall(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID, connection ids.ConnectionID, tool string, digest identity.Digest) (ConnectorCall, error) {
	return scanConnectorCall(q.QueryRow(ctx, `select `+connectorCallColumns+` from connector_call
		where tenant_id = $1 and attempt_id = $2 and connection_id = $3 and tool = $4 and input_digest = $5
		order by created_at desc, operation_id desc limit 1`,
		tenant.String(), attempt.String(), connection.String(), tool, string(digest)))
}

// CountAuthorizedCalls counts a task's authorized calls under one grant.
func CountAuthorizedCalls(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID, grant ids.ConnectionGrantID) (uint32, error) {
	var n int64
	err := q.QueryRow(ctx, `select count(*) from connector_call where tenant_id = $1 and task_id = $2 and connection_grant_id = $3 and state = 'authorized'`,
		tenant.String(), task.String(), grant.String()).Scan(&n)
	return uint32(n), err
}

func scanConnectorCall(row interface{ Scan(...any) error }) (ConnectorCall, error) {
	var c ConnectorCall
	var tenantText, operationText, attemptText, taskText, connectionText, grantText, providerText, digest, state string
	var execution, output *string
	var outputDigest *string
	var isError *bool
	var completedAt *time.Time
	err := row.Scan(&tenantText, &operationText, &attemptText, &taskText, &connectionText, &grantText, &providerText,
		&c.Tool, &digest, &state, &execution, &output, &outputDigest, &isError, &completedAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return ConnectorCall{}, ErrNotFound
		}
		return ConnectorCall{}, err
	}
	if c.Tenant, err = parseID[ids.Tenant]("connector_call.tenant_id", tenantText); err != nil {
		return ConnectorCall{}, err
	}
	if c.Operation, err = parseID[ids.Operation]("connector_call.operation_id", operationText); err != nil {
		return ConnectorCall{}, err
	}
	if c.Attempt, err = parseID[ids.Attempt]("connector_call.attempt_id", attemptText); err != nil {
		return ConnectorCall{}, err
	}
	if c.Task, err = parseID[ids.Task]("connector_call.task_id", taskText); err != nil {
		return ConnectorCall{}, err
	}
	if c.Connection, err = parseID[ids.Connection]("connector_call.connection_id", connectionText); err != nil {
		return ConnectorCall{}, err
	}
	if c.ConnectionGrant, err = parseID[ids.ConnGrant]("connector_call.connection_grant_id", grantText); err != nil {
		return ConnectorCall{}, err
	}
	if c.Provider, err = parseID[ids.Provider]("connector_call.provider_id", providerText); err != nil {
		return ConnectorCall{}, err
	}
	c.InputDigest = identity.Digest(digest)
	c.State = ConnectorCallState(state)
	if execution != nil {
		var e contracts.ConnectorExecution
		if err := json.Unmarshal([]byte(*execution), &e); err != nil || e.Validate() != nil {
			return ConnectorCall{}, corruptRow("connector_call", errors.Join(err, e.Validate()))
		}
		c.Execution = &e
	}
	if output != nil {
		r := contracts.ConnectorResult{Operation: c.Operation, Output: json.RawMessage(*output), IsError: *isError, CompletedAt: *completedAt}
		r.OutputDigest = identity.Digest(*outputDigest)
		c.Result = &r
	}
	return c, nil
}

// GetConnectionGrantForUpdate serializes the call ceiling with revocation.
func GetConnectionGrantForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ConnectionGrantID) (lifecycle.ConnectionGrant, error) {
	if err := lockRow(ctx, u, "connection_grant", "grant_id", tenant.String(), id.String()); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	return GetConnectionGrant(ctx, u, tenant, id)
}

// RecordConnectorUse records only safe authority evidence; raw call bytes stay
// in connector_call and are never copied to audit or event payloads.
func RecordConnectorUse(ctx context.Context, u *UnitOfWork, use identity.AuthorityUse, tr Transition) error {
	if _, _, err := InsertUse(ctx, u, use); err != nil {
		return err
	}
	change := lifecycle.Change{Tenant: use.Tenant, Record: lifecycle.RecordAuthorityUse, RecordID: use.Operation.Untyped(), Event: "authorize", From: "requested", To: string(use.Outcome), Version: 1}
	return appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox)
}
