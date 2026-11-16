package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

// Connections (ADR-0022) are tenant records: every principal of the tenant
// reads them, and only a connection's owner completes, grants or removes it.
// A connection's requests to its connector stand outside any attempt and are
// sent while the command runs; each is idempotent by connection, so a command
// that cannot reach the connector is repeated, never reconciled.

const (
	commandCreateConnection    = "create_connection"
	commandAuthorizeConnection = "authorize_connection"
	commandCompleteConnection  = "complete_connection"
	commandRemoveConnection    = "remove_connection"
	commandGrantConnection     = "grant_connection"
	commandRevokeConnGrant     = "revoke_connection_grant"

	// authorizationLifetime bounds a consent URL's state.
	authorizationLifetime = 10 * time.Minute
	// connectorRequestTimeout bounds one connection request to a connector.
	connectorRequestTimeout = 30 * time.Second
)

var (
	// ErrNoConnector reports a provider that is not registered as a connector
	// for this tenant.
	ErrNoConnector = errors.New("no registered connector provider for the selected id")
	// ErrConnectorUnavailable reports a connector this replica cannot reach
	// now. Nothing changed at the connector; the command can be repeated.
	ErrConnectorUnavailable = errors.New("the connector is not reachable")
	// ErrAuthorizationPending reports a consent the owner has not finished.
	ErrAuthorizationPending = errors.New("the connection's consent is not complete")
	// ErrAuthorizationFailed reports a consent the external application
	// refused or that expired; the owner starts it again.
	ErrAuthorizationFailed = errors.New("the connection's consent failed")
	// ErrUnknownTool reports a grant naming a tool its connector does not offer.
	ErrUnknownTool = errors.New("the connector offers no tool of that name")
	// ErrGrantTarget reports a grant naming an agent that is not active or a
	// task the connection's owner does not own.
	ErrGrantTarget = errors.New("a grant names an active agent of the tenant or a task its connection's owner owns")
)

// ConnectorPorts resolves a provider's connector port on this replica, or nil
// when its adapter is not connected here.
type ConnectorPorts interface {
	Port(tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) contracts.Port
}

// ConnectionView is a connection with the consent URL a command obtained, if
// the connector answered.
type ConnectionView struct {
	Connection    lifecycle.Connection
	Authorization *contracts.AuthorizationStarted
}

// CreateConnectionCommand registers a connection to an account at an external
// application, owned by the actor, and starts its consent.
type CreateConnectionCommand struct {
	Command
	At       time.Time
	Provider ids.ProviderID
	Name     string
}

type connectionPayload struct {
	Connection         string          `json:"connection,omitempty"`
	Provider           string          `json:"provider,omitempty"`
	Name               string          `json:"name,omitempty"`
	ExpectedVersion    uint64          `json:"expected_version,omitempty"`
	ConfirmationDigest identity.Digest `json:"confirmation_digest,omitempty"`
}

// CreateConnection runs the command, then asks the connector for a consent
// URL. A replay returns the connection as it was created and a fresh URL.
// The connection is created even when the connector is unreachable; the
// owner then asks for the URL again with AuthorizeConnection.
func CreateConnection(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, cmd CreateConnectionCommand) (ConnectionView, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(true); err != nil {
		return ConnectionView{}, err
	}
	if cmd.At.IsZero() || cmd.Provider.IsZero() {
		return ConnectionView{}, fmt.Errorf("%w: a connector provider and a command time are required", ErrInvalidCommand)
	}
	if err := lifecycle.ValidateConnectionName(cmd.Name); err != nil {
		return ConnectionView{}, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}
	digest, err := fingerprint(commandCreateConnection, connectionPayload{Provider: cmd.Provider.String(), Name: cmd.Name})
	if err != nil {
		return ConnectionView{}, err
	}
	connection, err := connectionResult(execute(ctx, pool, cmd.Command, commandCreateConnection, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		principal := cmd.Actor.Principal()
		if available, err := store.ConnectorSchemaAvailable(ctx, u.uow); err != nil {
			return nil, err
		} else if !available {
			return nil, ErrSchemaUnavailable
		}
		if _, err := store.GetProviderRegistration(ctx, u.uow, principal.Tenant, cmd.Provider, contracts.RoleConnector); errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoConnector
		} else if err != nil {
			return nil, err
		}
		id, err := ids.New[ids.Connection]()
		if err != nil {
			return nil, err
		}
		connection, err := lifecycle.NewConnection(id, principal.Tenant, cmd.Provider, principal.ID, cmd.Name, cmd.At)
		if err != nil {
			return nil, err
		}
		if err := store.InsertConnection(ctx, u.uow, connection); err != nil {
			return nil, err
		}
		return json.Marshal(connection)
	}))
	if err != nil {
		return ConnectionView{}, err
	}
	view := ConnectionView{Connection: connection}
	if connection.State == lifecycle.ConnectionAuthorizing {
		if started, err := beginAuthorization(ctx, ports, connection, cmd.At); err == nil {
			view.Authorization = &started
		} else if !errors.Is(err, ErrConnectorUnavailable) {
			return ConnectionView{}, err
		}
	}
	return view, nil
}

// AuthorizeConnectionCommand starts or renews consent for an owned connection.
// Its nonce makes a retry return the same consent URL.
type AuthorizeConnectionCommand struct {
	Command
	At         time.Time
	Connection ids.ConnectionID
}

// AuthorizeConnection asks the connector for a consent URL. A new nonce
// starts a new consent; replaying a nonce returns the URL it already issued.
func AuthorizeConnection(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, cmd AuthorizeConnectionCommand) (contracts.AuthorizationStarted, error) {
	if err := cmd.Command.validate(true); err != nil {
		return contracts.AuthorizationStarted{}, err
	}
	if cmd.At.IsZero() || cmd.Connection.IsZero() {
		return contracts.AuthorizationStarted{}, ErrInvalidCommand
	}
	digest, err := fingerprint(commandAuthorizeConnection, connectionPayload{Connection: cmd.Connection.String()})
	if err != nil {
		return contracts.AuthorizationStarted{}, err
	}
	raw, err := execute(ctx, pool, cmd.Command, commandAuthorizeConnection, digest, cmd.At.UTC(), func(u *unit) (json.RawMessage, error) {
		connection, err := store.GetConnectionForShare(ctx, u.uow, cmd.Actor.Tenant(), cmd.Connection)
		if err != nil {
			return nil, err
		}
		if connection.Owner != cmd.Actor.Principal().ID {
			return nil, connectionRefusal(connection, "authorize", lifecycle.RejectUnauthorizedActor)
		}
		if connection.State != lifecycle.ConnectionAuthorizing && connection.State != lifecycle.ConnectionActive {
			return nil, connectionRefusal(connection, "authorize", lifecycle.RejectInvalidTransition)
		}
		started, err := beginAuthorization(ctx, ports, connection, cmd.At.UTC())
		if err != nil {
			return nil, err
		}
		return json.Marshal(started)
	})
	if err != nil {
		return contracts.AuthorizationStarted{}, err
	}
	var started contracts.AuthorizationStarted
	err = json.Unmarshal(raw, &started)
	return started, err
}

// CompleteConnectionCommand records the account a finished consent produced.
// ExpectedVersion is the connection version the owner read.
type CompleteConnectionCommand struct {
	Command
	At           time.Time
	Connection   ids.ConnectionID
	Confirmation string
}

// CompleteConnection asks the connector whether the owner's consent finished
// and, once it has, records the account. A pending consent is
// ErrAuthorizationPending and a failed one ErrAuthorizationFailed; neither
// changes the connection.
func CompleteConnection(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, cmd CompleteConnectionCommand) (lifecycle.Connection, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(false); err != nil {
		return lifecycle.Connection{}, err
	}
	if cmd.At.IsZero() || cmd.Connection.IsZero() || contracts.ValidateConfirmation(cmd.Confirmation) != nil {
		return lifecycle.Connection{}, fmt.Errorf("%w: a connection and a command time are required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandCompleteConnection, connectionPayload{Connection: cmd.Connection.String(), ExpectedVersion: cmd.ExpectedVersion, ConfirmationDigest: identity.DigestOf([]byte(cmd.Confirmation))})
	if err != nil {
		return lifecycle.Connection{}, err
	}
	if stored, err := storedCommand(ctx, pool, cmd.Command, commandCompleteConnection); err != nil {
		return lifecycle.Connection{}, err
	} else if stored != nil {
		// A replay answers from the record without asking the connector.
		return connectionResult(execute(ctx, pool, cmd.Command, commandCompleteConnection, digest, cmd.At, replayOnly))
	}
	connection, err := ownedConnection(ctx, pool, cmd.Actor, cmd.Connection)
	if err != nil {
		return lifecycle.Connection{}, err
	}
	var result contracts.AuthorizationResult
	if err := connectionRequest(ctx, ports, connection, contracts.MethodCompleteAuthorization, contracts.CompleteAuthorization{Connection: connection.ID, Confirmation: cmd.Confirmation}, cmd.At, &result); err != nil {
		return lifecycle.Connection{}, err
	}
	if err := result.Validate(); err != nil {
		return lifecycle.Connection{}, fmt.Errorf("%w: connector reply: %v", ErrConnectorUnavailable, err)
	}
	switch result.Status {
	case contracts.AuthorizationPending:
		return lifecycle.Connection{}, ErrAuthorizationPending
	case contracts.AuthorizationFailed:
		return lifecycle.Connection{}, ErrAuthorizationFailed
	}
	return connectionResult(execute(ctx, pool, cmd.Command, commandCompleteConnection, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		completed, err := store.CompleteConnection(ctx, u.uow, cmd.Actor.Principal().Tenant, cmd.Connection, *result.Account, tr)
		if err != nil {
			return nil, connectionStale(ctx, u, cmd.Actor.Principal().Tenant, cmd.Connection, lifecycle.ConnectionComplete, cmd.ExpectedVersion, err)
		}
		return json.Marshal(completed)
	}))
}

// RemoveConnectionCommand removes a connection. ExpectedVersion is the
// connection version the owner read.
type RemoveConnectionCommand struct {
	Command
	At         time.Time
	Connection ids.ConnectionID
}

// RemoveConnection stops the connection's use and revokes its credential. In
// one transaction the connection starts removing and every active grant of it
// is revoked, so no call can be authorized under it again. The connector is
// then asked to revoke and destroy the credential; once it has, the
// connection is removed. A connector that cannot be reached leaves the
// connection removing, and removing it again repeats only the revocation.
func RemoveConnection(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, cmd RemoveConnectionCommand) (lifecycle.Connection, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(false); err != nil {
		return lifecycle.Connection{}, err
	}
	if cmd.At.IsZero() || cmd.Connection.IsZero() {
		return lifecycle.Connection{}, fmt.Errorf("%w: a connection and a command time are required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandRemoveConnection, connectionPayload{Connection: cmd.Connection.String(), ExpectedVersion: cmd.ExpectedVersion})
	if err != nil {
		return lifecycle.Connection{}, err
	}
	tenant := cmd.Actor.Principal().Tenant
	connection, err := connectionResult(execute(ctx, pool, cmd.Command, commandRemoveConnection, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		current, err := store.GetConnection(ctx, u.uow, tenant, cmd.Connection)
		if err != nil {
			return nil, err
		}
		if current.State == lifecycle.ConnectionRemoving && current.Owner == cmd.Actor.Principal().ID && current.Version == cmd.ExpectedVersion {
			return json.Marshal(current)
		}
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		removing, err := store.RemoveConnection(ctx, u.uow, tenant, cmd.Connection, tr)
		if err != nil {
			return nil, connectionStale(ctx, u, tenant, cmd.Connection, lifecycle.ConnectionRemove, cmd.ExpectedVersion, err)
		}
		grants, err := store.ListConnectionGrants(ctx, u.uow, tenant, cmd.Connection)
		if err != nil {
			return nil, err
		}
		for _, grant := range grants {
			if grant.State != lifecycle.ConnectionGrantActive {
				continue
			}
			tr, err := u.transition(grant.Version, nil)
			if err != nil {
				return nil, err
			}
			if _, err := store.RevokeConnectionGrant(ctx, u.uow, tenant, grant.ID, tr); err != nil {
				return nil, err
			}
		}
		return json.Marshal(removing)
	}))
	if err != nil || connection.State != lifecycle.ConnectionRemoving {
		return connection, err
	}
	if err := connectionRequest(ctx, ports, connection, contracts.MethodRevokeConnection, contracts.ConnectionReference{Connection: connection.ID}, cmd.At, nil); err != nil {
		if errors.Is(err, ErrConnectorUnavailable) {
			return connection, nil
		}
		return lifecycle.Connection{}, err
	}
	return finishRemoval(ctx, pool, cmd.Command.actor(), connection)
}

// finishRemoval records that the connector revoked and destroyed the
// credential. A concurrent removal that finished first answers with its
// result.
func finishRemoval(ctx context.Context, pool *pgxpool.Pool, actor lifecycle.Actor, connection lifecycle.Connection) (lifecycle.Connection, error) {
	u, err := store.Begin(ctx, pool)
	if err != nil {
		return lifecycle.Connection{}, err
	}
	defer func() { _ = u.Rollback(ctx) }()
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return lifecycle.Connection{}, err
	}
	removed, err := store.FinishConnectionRemoval(ctx, u, connection.Tenant, connection.ID, store.Transition{
		ExpectedVersion: connection.Version, At: time.Now().UTC().Truncate(time.Microsecond),
		Actor: actor, AuditID: auditID,
	})
	if err != nil {
		var refusal *lifecycle.TransitionError
		if errors.Is(err, store.ErrStaleVersion) || errors.As(err, &refusal) {
			_ = u.Rollback(ctx)
			return store.GetConnection(ctx, pool, connection.Tenant, connection.ID)
		}
		return lifecycle.Connection{}, err
	}
	return removed, u.Commit(ctx)
}

// GrantConnectionCommand lets an agent's or a task's work call some tools of
// one of the actor's connections.
type GrantConnectionCommand struct {
	Command
	At         time.Time
	Connection ids.ConnectionID
	Terms      lifecycle.ConnectionGrantTerms
}

type grantPayload struct {
	Connection string              `json:"connection"`
	Agent      string              `json:"agent,omitempty"`
	Task       string              `json:"task,omitempty"`
	Tools      []string            `json:"tools"`
	Permission identity.Permission `json:"permission"`
	CallLimit  uint32              `json:"call_limit"`
	ExpiresAt  time.Time           `json:"expires_at"`
}

// GrantConnection runs the command. The tools must be ones the connector
// offers now, so the connector is asked for its catalogue first.
func GrantConnection(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, cmd GrantConnectionCommand) (lifecycle.ConnectionGrant, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	cmd.Terms.ExpiresAt = cmd.Terms.ExpiresAt.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(true); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if cmd.At.IsZero() || cmd.Connection.IsZero() {
		return lifecycle.ConnectionGrant{}, fmt.Errorf("%w: a connection and a command time are required", ErrInvalidCommand)
	}
	tools := slices.Clone(cmd.Terms.Tools)
	slices.Sort(tools)
	digest, err := fingerprint(commandGrantConnection, grantPayload{
		Connection: cmd.Connection.String(), Agent: idText(cmd.Terms.Agent), Task: idText(cmd.Terms.Task),
		Tools: tools, Permission: cmd.Terms.Permission, CallLimit: cmd.Terms.CallLimit, ExpiresAt: cmd.Terms.ExpiresAt,
	})
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if stored, err := storedCommand(ctx, pool, cmd.Command, commandGrantConnection); err != nil {
		return lifecycle.ConnectionGrant{}, err
	} else if stored != nil {
		return grantResult(execute(ctx, pool, cmd.Command, commandGrantConnection, digest, cmd.At, replayOnly))
	}
	connection, err := ownedConnection(ctx, pool, cmd.Actor, cmd.Connection)
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	catalogue, err := connectorTools(ctx, ports, connection, cmd.At)
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	for _, tool := range cmd.Terms.Tools {
		if _, ok := catalogue.Tool(tool); !ok {
			return lifecycle.ConnectionGrant{}, ErrUnknownTool
		}
	}
	return grantResult(execute(ctx, pool, cmd.Command, commandGrantConnection, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		principal := cmd.Actor.Principal()
		connection, err := store.GetConnectionForShare(ctx, u.uow, principal.Tenant, cmd.Connection)
		if err != nil {
			return nil, err
		}
		switch {
		case !cmd.Terms.Agent.IsZero():
			agent, err := store.GetAgent(ctx, u.uow, principal.Tenant, cmd.Terms.Agent)
			if errors.Is(err, store.ErrNotFound) || (err == nil && agent.State != lifecycle.AgentActive) {
				return nil, ErrGrantTarget
			} else if err != nil {
				return nil, err
			}
		case !cmd.Terms.Task.IsZero():
			task, err := store.GetTask(ctx, u.uow, principal.Tenant, cmd.Terms.Task)
			if errors.Is(err, store.ErrNotFound) || (err == nil && task.Owner != connection.Owner) {
				return nil, ErrGrantTarget
			} else if err != nil {
				return nil, err
			}
		}
		id, err := ids.New[ids.ConnGrant]()
		if err != nil {
			return nil, err
		}
		grant, err := lifecycle.NewConnectionGrant(id, connection, principal.ID, cmd.Terms, cmd.At)
		if err != nil {
			var refusal *lifecycle.TransitionError
			if errors.As(err, &refusal) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
		}
		if err := store.InsertConnectionGrant(ctx, u.uow, grant); err != nil {
			return nil, err
		}
		return json.Marshal(grant)
	}))
}

// RevokeConnectionGrantCommand revokes a grant. ExpectedVersion is the grant
// version the actor read.
type RevokeConnectionGrantCommand struct {
	Command
	At    time.Time
	Grant ids.ConnectionGrantID
}

// RevokeConnectionGrant runs the command. The next call the grant covered is
// refused; a call already authorized under it completes or is cancelled with
// its attempt.
func RevokeConnectionGrant(ctx context.Context, pool *pgxpool.Pool, cmd RevokeConnectionGrantCommand) (lifecycle.ConnectionGrant, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(false); err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	if cmd.At.IsZero() || cmd.Grant.IsZero() {
		return lifecycle.ConnectionGrant{}, fmt.Errorf("%w: a grant and a command time are required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandRevokeConnGrant, struct {
		Grant           string `json:"grant"`
		ExpectedVersion uint64 `json:"expected_version"`
	}{cmd.Grant.String(), cmd.ExpectedVersion})
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	return grantResult(execute(ctx, pool, cmd.Command, commandRevokeConnGrant, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		tenant := cmd.Actor.Principal().Tenant
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		revoked, err := store.RevokeConnectionGrant(ctx, u.uow, tenant, cmd.Grant, tr)
		if errors.Is(err, store.ErrStaleVersion) {
			grant, loadErr := store.GetConnectionGrant(ctx, u.uow, tenant, cmd.Grant)
			if loadErr != nil {
				return nil, errors.Join(err, loadErr)
			}
			return nil, staleRefusal(tenant, lifecycle.RecordConnectionGrant, cmd.Grant.Untyped(), string(lifecycle.ConnectionGrantRevoke), string(grant.State), cmd.ExpectedVersion)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(revoked)
	}))
}

// GetConnection reads one of the tenant's connections.
func GetConnection(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.ConnectionID) (lifecycle.Connection, error) {
	var connection lifecycle.Connection
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		connection, err = store.GetConnection(ctx, r.uow, r.principal.Tenant, id)
		return err
	})
	return connection, err
}

// ListConnections reads the tenant's connections, removed ones included.
func ListConnections(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext) ([]lifecycle.Connection, error) {
	var connections []lifecycle.Connection
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		connections, err = store.ListConnections(ctx, r.uow, r.principal.Tenant)
		return err
	})
	return connections, err
}

// ListConnectionGrants reads a connection's grants, revoked ones included.
func ListConnectionGrants(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.ConnectionID) ([]lifecycle.ConnectionGrant, error) {
	var grants []lifecycle.ConnectionGrant
	err := read(ctx, pool, actor, func(r *reader) error {
		if _, err := store.GetConnection(ctx, r.uow, r.principal.Tenant, id); err != nil {
			return err
		}
		var err error
		grants, err = store.ListConnectionGrants(ctx, r.uow, r.principal.Tenant, id)
		return err
	})
	return grants, err
}

// GetConnectionGrant reads one grant of the tenant's connections.
func GetConnectionGrant(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.ConnectionGrantID) (lifecycle.ConnectionGrant, error) {
	var grant lifecycle.ConnectionGrant
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		grant, err = store.GetConnectionGrant(ctx, r.uow, r.principal.Tenant, id)
		return err
	})
	return grant, err
}

// ListConnectionTools asks a connection's connector for the tools it offers.
func ListConnectionTools(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, actor identity.AuthorizationContext, id ids.ConnectionID, at time.Time) (contracts.ToolCatalogue, error) {
	connection, err := GetConnection(ctx, pool, actor, id)
	if err != nil {
		return contracts.ToolCatalogue{}, err
	}
	return connectorTools(ctx, ports, connection, at.UTC())
}

func connectorTools(ctx context.Context, ports ConnectorPorts, connection lifecycle.Connection, at time.Time) (contracts.ToolCatalogue, error) {
	var catalogue contracts.ToolCatalogue
	if err := connectionRequest(ctx, ports, connection, contracts.MethodListTools, nil, at, &catalogue); err != nil {
		return contracts.ToolCatalogue{}, err
	}
	if err := catalogue.Validate(); err != nil {
		return contracts.ToolCatalogue{}, fmt.Errorf("%w: connector catalogue: %v", ErrConnectorUnavailable, err)
	}
	return catalogue, nil
}

func beginAuthorization(ctx context.Context, ports ConnectorPorts, connection lifecycle.Connection, at time.Time) (contracts.AuthorizationStarted, error) {
	var started contracts.AuthorizationStarted
	request := contracts.BeginAuthorization{Connection: connection.ID, ExpiresAt: at.Add(authorizationLifetime)}
	if err := connectionRequest(ctx, ports, connection, contracts.MethodBeginAuthorization, request, at, &started); err != nil {
		return contracts.AuthorizationStarted{}, err
	}
	if err := started.Validate(); err != nil {
		return contracts.AuthorizationStarted{}, fmt.Errorf("%w: connector reply: %v", ErrConnectorUnavailable, err)
	}
	return started, nil
}

// connectionRequest sends one of a connection's requests to its connector
// and decodes the reply's result into out. Anything short of an accepted
// reply is ErrConnectorUnavailable: these requests are idempotent by
// connection, so the caller repeats them rather than reconciling.
func connectionRequest(ctx context.Context, ports ConnectorPorts, connection lifecycle.Connection, method contracts.Method, arguments any, at time.Time, out any) error {
	if ports == nil {
		return ErrConnectorUnavailable
	}
	port := ports.Port(connection.Tenant, connection.Provider, contracts.RoleConnector)
	if port == nil {
		return ErrConnectorUnavailable
	}
	var raw json.RawMessage
	if arguments != nil {
		var err error
		if raw, err = json.Marshal(arguments); err != nil {
			return err
		}
	}
	operation, err := ids.New[ids.Operation]()
	if err != nil {
		return err
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	request := contracts.Request{
		Operation: operation, Delivery: delivery, Tenant: connection.Tenant, Role: contracts.RoleConnector,
		Method: method, Arguments: raw, Demand: contracts.DemandPerson, Deadline: at.Add(connectorRequestTimeout),
	}
	if err := request.Validate(); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, connectorRequestTimeout)
	defer cancel()
	reply, err := port.Send(sendCtx, request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnectorUnavailable, err)
	}
	if reply.Validate() != nil || reply.Operation != operation || reply.Delivery != delivery || reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		if reply.Failure != nil {
			return fmt.Errorf("%w: %s", ErrConnectorUnavailable, reply.Failure.Code)
		}
		return ErrConnectorUnavailable
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Result, out); err != nil {
		return fmt.Errorf("%w: connector reply: %v", ErrConnectorUnavailable, err)
	}
	return nil
}

// ownedConnection reads a connection the actor owns. Another principal's
// connection is refused as an unauthorized actor, the way its lifecycle
// would refuse the change.
func ownedConnection(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.ConnectionID) (lifecycle.Connection, error) {
	connection, err := GetConnection(ctx, pool, actor, id)
	if err != nil {
		return lifecycle.Connection{}, err
	}
	if connection.Owner != actor.Principal().ID {
		return lifecycle.Connection{}, connectionRefusal(connection, "authorize", lifecycle.RejectUnauthorizedActor)
	}
	return connection, nil
}

func connectionRefusal(c lifecycle.Connection, event string, code lifecycle.RejectionCode) error {
	return &lifecycle.TransitionError{Code: code, Change: lifecycle.Change{
		Tenant: c.Tenant, Record: lifecycle.RecordConnection, RecordID: c.ID.Untyped(),
		Event: event, From: string(c.State), Version: c.Version,
	}}
}

// connectionStale turns the store's stale version into the lifecycle's
// version conflict, as agentRefusal does.
func connectionStale(ctx context.Context, u *unit, tenant ids.TenantID, id ids.ConnectionID, event lifecycle.ConnectionEvent, expected uint64, err error) error {
	if !errors.Is(err, store.ErrStaleVersion) {
		return err
	}
	c, loadErr := store.GetConnection(ctx, u.uow, tenant, id)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	return staleRefusal(tenant, lifecycle.RecordConnection, id.Untyped(), string(event), string(c.State), expected)
}

func connectionResult(raw json.RawMessage, err error) (lifecycle.Connection, error) {
	if err != nil {
		return lifecycle.Connection{}, err
	}
	var c lifecycle.Connection
	if err := json.Unmarshal(raw, &c); err != nil {
		return lifecycle.Connection{}, fmt.Errorf("connection record: %w", err)
	}
	return c, nil
}

func grantResult(raw json.RawMessage, err error) (lifecycle.ConnectionGrant, error) {
	if err != nil {
		return lifecycle.ConnectionGrant{}, err
	}
	var g lifecycle.ConnectionGrant
	if err := json.Unmarshal(raw, &g); err != nil {
		return lifecycle.ConnectionGrant{}, fmt.Errorf("connection grant record: %w", err)
	}
	return g, nil
}

// storedCommand returns the command record of cmd's nonce, if one exists. A
// command that asks a connector before it runs answers a replay from the
// record instead, so a retry never repeats a side request it already made.
func storedCommand(ctx context.Context, pool *pgxpool.Pool, cmd Command, kind string) (*store.CommandRecord, error) {
	principal := cmd.Actor.Principal()
	record, err := store.GetCommandRecord(ctx, pool, principal.Tenant, principal.ID, kind, cmd.Nonce)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// replayOnly is the work of a command already recorded: execute answers it
// from the record and never runs this.
func replayOnly(*unit) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: the command record disappeared", ErrInvalidCommand)
}

func idText[K any](id ids.ID[K]) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}

// FinishConnectionRemovals recovers credential destruction after a process dies
// between recording removing and receiving the connector's confirmation. The
// connector's revocation is idempotent by connection, including after a lost reply.
func FinishConnectionRemovals(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, tenant ids.TenantID) error {
	available, err := store.ConnectorSchemaAvailable(ctx, pool)
	if err != nil || !available {
		return err
	}
	rows, err := pool.Query(ctx, `select connection_id::text from connection where tenant_id=$1 and state='removing' order by updated_at limit 10`, tenant.String())
	if err != nil {
		return err
	}
	var idsToRemove []ids.ConnectionID
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			rows.Close()
			return err
		}
		id, err := ids.Parse[ids.Connection](text)
		if err != nil {
			rows.Close()
			return err
		}
		idsToRemove = append(idsToRemove, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range idsToRemove {
		c, err := store.GetConnection(ctx, pool, tenant, id)
		if err != nil {
			return err
		}
		if c.State != lifecycle.ConnectionRemoving {
			continue
		}
		if err := connectionRequest(ctx, ports, c, contracts.MethodRevokeConnection, contracts.ConnectionReference{Connection: id}, time.Now().UTC(), nil); err != nil {
			if errors.Is(err, ErrConnectorUnavailable) {
				continue
			}
			return err
		}
		if _, err := finishRemoval(ctx, pool, lifecycle.Actor{Kind: lifecycle.ActorSystem}, c); err != nil {
			return err
		}
	}
	return nil
}
