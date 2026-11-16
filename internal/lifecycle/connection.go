package lifecycle

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
)

const (
	MaxConnectionNameCharacters    = 100
	MaxConnectionAccountCharacters = 200
	MaxConnectionPermissions       = 64
	MaxConnectionPermissionLength  = 100
	MaxGrantTools                  = 64
	MaxGrantCallLimit              = 10_000
)

// toolNamePattern is a connector tool's name: one segment of an action class,
// so "connector." + name is a valid identity.ActionClass.
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// ValidateToolName reports whether name can name a connector tool.
func ValidateToolName(name string) error {
	if !toolNamePattern.MatchString(name) {
		return fmt.Errorf("%w: tool name must be a lowercase identifier of at most 48 characters", ErrInvalid)
	}
	return nil
}

// ConnectorAction is the governed action class of a connector tool.
func ConnectorAction(tool string) identity.ActionClass {
	return identity.ActionClass("connector." + tool)
}

// ConnectionState is the product lifecycle state of a Connection.
type ConnectionState string

const (
	ConnectionAuthorizing ConnectionState = "authorizing"
	ConnectionActive      ConnectionState = "active"
	ConnectionRemoving    ConnectionState = "removing"
	ConnectionRemoved     ConnectionState = "removed"
)

// ConnectionStates lists every ConnectionState.
func ConnectionStates() []ConnectionState {
	return []ConnectionState{ConnectionAuthorizing, ConnectionActive, ConnectionRemoving, ConnectionRemoved}
}

// ConnectionEvent is a command or observation that may change a Connection.
type ConnectionEvent string

const (
	ConnectionComplete ConnectionEvent = "complete"
	ConnectionRemove   ConnectionEvent = "remove"
	ConnectionFinish   ConnectionEvent = "finish_removal"
)

// ConnectionEvents lists every ConnectionEvent.
func ConnectionEvents() []ConnectionEvent {
	return []ConnectionEvent{ConnectionComplete, ConnectionRemove, ConnectionFinish}
}

// connectionMachine follows ADR-0022. Completing an active connection again
// records a renewed consent; removal revokes the credential at the connector
// before the record is final.
var connectionMachine = newMachine(ConnectionStates(), ConnectionEvents(),
	[]ConnectionState{ConnectionRemoved},
	map[ConnectionState]map[ConnectionEvent][]ConnectionState{
		ConnectionAuthorizing: {
			ConnectionComplete: {ConnectionActive},
			ConnectionRemove:   {ConnectionRemoving},
		},
		ConnectionActive: {
			ConnectionComplete: {ConnectionActive},
			ConnectionRemove:   {ConnectionRemoving},
		},
		ConnectionRemoving: {
			ConnectionFinish: {ConnectionRemoved},
		},
	},
)

// ConnectionAccount is what the external application reported about the
// account a connection acts as: a display label and the permissions it
// granted. It holds no credential.
type ConnectionAccount struct {
	Account     string   `json:"account"`
	Permissions []string `json:"permissions"`
}

// Validate reports whether the account report is within its bounds.
func (a ConnectionAccount) Validate() error {
	if !utf8.ValidString(a.Account) || strings.TrimSpace(a.Account) == "" || strings.ContainsAny(a.Account, "\n\r") ||
		utf8.RuneCountInString(a.Account) > MaxConnectionAccountCharacters {
		return fmt.Errorf("%w: connection account must be one non-blank line of at most %d characters", ErrInvalid, MaxConnectionAccountCharacters)
	}
	if a.Permissions == nil || len(a.Permissions) > MaxConnectionPermissions {
		return fmt.Errorf("%w: connection reports at most %d permissions", ErrInvalid, MaxConnectionPermissions)
	}
	for _, p := range a.Permissions {
		if !utf8.ValidString(p) || strings.TrimSpace(p) == "" || strings.ContainsAny(p, "\n\r") || len(p) > MaxConnectionPermissionLength {
			return fmt.Errorf("%w: connection permission must be one non-blank line of at most %d bytes", ErrInvalid, MaxConnectionPermissionLength)
		}
	}
	return nil
}

// Connection is a tenant's link to one account at an external application,
// served by a connector provider. It refers to a credential the connector
// adapter holds and holds no secret itself (ADR-0022). Only its owner, the
// principal that registered it, completes, grants or removes it.
type Connection struct {
	ID       ids.ConnectionID `json:"connection_id"`
	Tenant   ids.TenantID     `json:"tenant_id"`
	Provider ids.ProviderID   `json:"provider_id"`
	Owner    ids.PrincipalID  `json:"owner"`
	Name     string           `json:"name"`
	State    ConnectionState  `json:"state"`
	// Account is set once consent completes and kept through removal.
	Account   *ConnectionAccount `json:"account,omitempty"`
	Version   uint64             `json:"version"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// NewConnection returns a Connection awaiting its first consent.
func NewConnection(id ids.ConnectionID, tenant ids.TenantID, provider ids.ProviderID, owner ids.PrincipalID, name string, at time.Time) (Connection, error) {
	c := Connection{
		ID: id, Tenant: tenant, Provider: provider, Owner: owner, Name: name,
		State: ConnectionAuthorizing, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	return c, c.Validate()
}

// ValidateConnectionName reports whether name can name a connection.
func ValidateConnectionName(name string) error {
	if !utf8.ValidString(name) || name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\n\r") ||
		utf8.RuneCountInString(name) > MaxConnectionNameCharacters {
		return fmt.Errorf("%w: connection name must be one line of 1 to %d characters with no surrounding space", ErrInvalid, MaxConnectionNameCharacters)
	}
	return nil
}

// Validate reports whether the record is complete and consistent.
func (c Connection) Validate() error {
	if c.ID.IsZero() || c.Tenant.IsZero() || c.Provider.IsZero() || c.Owner.IsZero() || c.Version == 0 {
		return fmt.Errorf("%w: connection requires identifiers and a positive version", ErrInvalid)
	}
	if err := ValidateConnectionName(c.Name); err != nil {
		return err
	}
	if !slices.Contains(ConnectionStates(), c.State) {
		return fmt.Errorf("%w: unknown connection state", ErrInvalid)
	}
	if c.State == ConnectionActive && c.Account == nil {
		return fmt.Errorf("%w: an active connection reports its account", ErrInvalid)
	}
	if c.Account != nil {
		if err := c.Account.Validate(); err != nil {
			return err
		}
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) {
		return fmt.Errorf("%w: connection timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Complete records the account a consent produced, if actor owns the
// connection and it is still at expectedVersion.
func (c Connection) Complete(actor ids.PrincipalID, account ConnectionAccount, expectedVersion uint64, at time.Time) (Connection, Change, error) {
	if err := account.Validate(); err != nil {
		return c, c.change(ConnectionComplete), err
	}
	next, change, err := c.apply(ConnectionComplete, actor, expectedVersion, at)
	if err != nil {
		return c, change, err
	}
	next.Account = &account
	return next, change, nil
}

// Remove starts removal if actor owns the connection. Its grants stop
// authorizing at once; the connector then revokes and destroys the credential.
func (c Connection) Remove(actor ids.PrincipalID, expectedVersion uint64, at time.Time) (Connection, Change, error) {
	return c.apply(ConnectionRemove, actor, expectedVersion, at)
}

// FinishRemoval records that the connector revoked and destroyed the
// credential. It is the connector's observation, so no actor is checked.
func (c Connection) FinishRemoval(expectedVersion uint64, at time.Time) (Connection, Change, error) {
	return c.apply(ConnectionFinish, c.Owner, expectedVersion, at)
}

func (c Connection) change(event ConnectionEvent) Change {
	return Change{
		Tenant: c.Tenant, Record: RecordConnection, RecordID: c.ID.Untyped(),
		Event: string(event), From: string(c.State), Version: c.Version,
	}
}

func (c Connection) apply(event ConnectionEvent, actor ids.PrincipalID, expectedVersion uint64, at time.Time) (Connection, Change, error) {
	change := c.change(event)
	if actor.IsZero() || actor != c.Owner {
		return c, change, reject(change, RejectUnauthorizedActor)
	}
	next, code := connectionMachine.next(c.State, c.Version, expectedVersion, event, "")
	if code != "" {
		return c, change, reject(change, code)
	}
	c.State = next
	c.Version++
	c.UpdatedAt = at
	change.To = string(next)
	change.Version = c.Version
	return c, change, nil
}

// ConnectionGrantState is the product lifecycle state of a ConnectionGrant.
// Expiry is not a state: a grant past ExpiresAt simply stops authorizing.
type ConnectionGrantState string

const (
	ConnectionGrantActive  ConnectionGrantState = "active"
	ConnectionGrantRevoked ConnectionGrantState = "revoked"
)

// ConnectionGrantStates lists every ConnectionGrantState.
func ConnectionGrantStates() []ConnectionGrantState {
	return []ConnectionGrantState{ConnectionGrantActive, ConnectionGrantRevoked}
}

// ConnectionGrantEvent is a command that may change a ConnectionGrant.
type ConnectionGrantEvent string

const ConnectionGrantRevoke ConnectionGrantEvent = "revoke"

// ConnectionGrantEvents lists every ConnectionGrantEvent.
func ConnectionGrantEvents() []ConnectionGrantEvent {
	return []ConnectionGrantEvent{ConnectionGrantRevoke}
}

var connectionGrantMachine = newMachine(ConnectionGrantStates(), ConnectionGrantEvents(),
	[]ConnectionGrantState{ConnectionGrantRevoked},
	map[ConnectionGrantState]map[ConnectionGrantEvent][]ConnectionGrantState{
		ConnectionGrantActive: {ConnectionGrantRevoke: {ConnectionGrantRevoked}},
	},
)

// ConnectionGrant lets the work of one agent, or of one task, use some tools
// of a connection (ADR-0022). It names exactly one of Agent and Task. A grant
// is used only by a task its connection's owner owns. Permission is ask or
// always: always is the owner's standing approval of every call it covers,
// ask makes each call wait for a person's decision. CallLimit bounds the
// calls one task may make under the grant.
type ConnectionGrant struct {
	ID         ids.ConnectionGrantID `json:"grant_id"`
	Tenant     ids.TenantID          `json:"tenant_id"`
	Connection ids.ConnectionID      `json:"connection_id"`
	Agent      ids.AgentID           `json:"agent_id,omitzero"`
	Task       ids.TaskID            `json:"task_id,omitzero"`
	Tools      []string              `json:"tools"`
	Permission identity.Permission   `json:"permission"`
	CallLimit  uint32                `json:"call_limit"`
	State      ConnectionGrantState  `json:"state"`
	GrantedBy  ids.PrincipalID       `json:"granted_by"`
	Version    uint64                `json:"version"`
	ExpiresAt  time.Time             `json:"expires_at"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
}

// ConnectionGrantTerms is what a grant's owner chooses.
type ConnectionGrantTerms struct {
	Agent      ids.AgentID
	Task       ids.TaskID
	Tools      []string
	Permission identity.Permission
	CallLimit  uint32
	ExpiresAt  time.Time
}

// NewConnectionGrant returns an active grant of connection by its owner. The
// connection must be active.
func NewConnectionGrant(id ids.ConnectionGrantID, connection Connection, actor ids.PrincipalID, terms ConnectionGrantTerms, at time.Time) (ConnectionGrant, error) {
	change := Change{Tenant: connection.Tenant, Record: RecordConnection, RecordID: connection.ID.Untyped(), Event: "grant", From: string(connection.State), Version: connection.Version}
	if actor.IsZero() || actor != connection.Owner {
		return ConnectionGrant{}, reject(change, RejectUnauthorizedActor)
	}
	if connection.State != ConnectionActive {
		return ConnectionGrant{}, reject(change, RejectInvalidTransition)
	}
	tools := slices.Clone(terms.Tools)
	slices.Sort(tools)
	g := ConnectionGrant{
		ID: id, Tenant: connection.Tenant, Connection: connection.ID, Agent: terms.Agent, Task: terms.Task,
		Tools: tools, Permission: terms.Permission, CallLimit: terms.CallLimit, State: ConnectionGrantActive,
		GrantedBy: actor, Version: 1, ExpiresAt: terms.ExpiresAt, CreatedAt: at, UpdatedAt: at,
	}
	return g, g.Validate()
}

// Validate reports whether the record is complete and consistent.
func (g ConnectionGrant) Validate() error {
	if g.ID.IsZero() || g.Tenant.IsZero() || g.Connection.IsZero() || g.GrantedBy.IsZero() || g.Version == 0 {
		return fmt.Errorf("%w: connection grant requires identifiers and a positive version", ErrInvalid)
	}
	if g.Agent.IsZero() == g.Task.IsZero() {
		return fmt.Errorf("%w: connection grant names exactly one agent or task", ErrInvalid)
	}
	if len(g.Tools) == 0 || len(g.Tools) > MaxGrantTools {
		return fmt.Errorf("%w: connection grant names 1 to %d tools", ErrInvalid, MaxGrantTools)
	}
	for i, tool := range g.Tools {
		if err := ValidateToolName(tool); err != nil {
			return err
		}
		if i > 0 && g.Tools[i-1] >= tool {
			return fmt.Errorf("%w: connection grant tools are sorted and distinct", ErrInvalid)
		}
	}
	if g.Permission != identity.PermissionAsk && g.Permission != identity.PermissionAlways {
		return fmt.Errorf("%w: connection grant permission is ask or always", ErrInvalid)
	}
	if g.CallLimit == 0 || g.CallLimit > MaxGrantCallLimit {
		return fmt.Errorf("%w: connection grant call limit is 1 to %d", ErrInvalid, MaxGrantCallLimit)
	}
	if !slices.Contains(ConnectionGrantStates(), g.State) {
		return fmt.Errorf("%w: unknown connection grant state", ErrInvalid)
	}
	if g.CreatedAt.IsZero() || g.UpdatedAt.Before(g.CreatedAt) || !g.ExpiresAt.After(g.CreatedAt) {
		return fmt.Errorf("%w: connection grant timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Covers reports whether the grant authorizes tool for work delegated to
// agent as task at time at.
func (g ConnectionGrant) Covers(task ids.TaskID, agent ids.AgentID, tool string, at time.Time) bool {
	if g.State != ConnectionGrantActive || !at.Before(g.ExpiresAt) {
		return false
	}
	if g.Task.IsZero() && (agent.IsZero() || g.Agent != agent) {
		return false
	}
	if !g.Task.IsZero() && g.Task != task {
		return false
	}
	_, found := slices.BinarySearch(g.Tools, tool)
	return found
}

// Revoke stops the grant authorizing, if actor granted it.
func (g ConnectionGrant) Revoke(actor ids.PrincipalID, expectedVersion uint64, at time.Time) (ConnectionGrant, Change, error) {
	change := Change{Tenant: g.Tenant, Record: RecordConnectionGrant, RecordID: g.ID.Untyped(), Event: string(ConnectionGrantRevoke), From: string(g.State), Version: g.Version}
	if actor.IsZero() || actor != g.GrantedBy {
		return g, change, reject(change, RejectUnauthorizedActor)
	}
	next, code := connectionGrantMachine.next(g.State, g.Version, expectedVersion, ConnectionGrantRevoke, "")
	if code != "" {
		return g, change, reject(change, code)
	}
	g.State = next
	g.Version++
	g.UpdatedAt = at
	change.To = string(next)
	change.Version = g.Version
	return g, change, nil
}
