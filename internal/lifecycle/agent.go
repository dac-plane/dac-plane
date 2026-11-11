package lifecycle

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dac-plane/dac-plane/internal/ids"
)

const (
	MaxAgentNameCharacters         = 100
	MaxAgentRoleCharacters         = 1_000
	MaxAgentInstructionsCharacters = 20_000
)

// AgentState is the product lifecycle state of an Agent.
type AgentState string

const (
	AgentActive  AgentState = "active"
	AgentRetired AgentState = "retired"
)

// AgentStates lists every AgentState.
func AgentStates() []AgentState {
	return []AgentState{AgentActive, AgentRetired}
}

// AgentEvent is a command that may change an Agent.
type AgentEvent string

const (
	AgentRevise AgentEvent = "revise"
	AgentRetire AgentEvent = "retire"
)

// AgentEvents lists every AgentEvent.
func AgentEvents() []AgentEvent {
	return []AgentEvent{AgentRevise, AgentRetire}
}

// agentMachine follows ADR-0020: an active agent is revised in place, and a
// retired one accepts no further change.
var agentMachine = newMachine(AgentStates(), AgentEvents(),
	[]AgentState{AgentRetired},
	map[AgentState]map[AgentEvent][]AgentState{
		AgentActive: {
			AgentRevise: {AgentActive},
			AgentRetire: {AgentRetired},
		},
	},
)

// AgentProfile is what a revision of an Agent says: who the agent is and the
// providers its work goes to unless a submission names others. It holds no
// model identifier, credential or tool list; those belong to provider
// registration and adapter configuration (ADR-0013, ADR-0020). Text is
// preserved exactly.
type AgentProfile struct {
	Name                string         `json:"name"`
	Role                string         `json:"role"`
	Instructions        string         `json:"instructions,omitempty"`
	Provider            ids.ProviderID `json:"provider_id"`
	EnvironmentProvider ids.ProviderID `json:"environment_provider_id"`
}

func (p AgentProfile) validate() error {
	if !utf8.ValidString(p.Name) || p.Name == "" || p.Name != strings.TrimSpace(p.Name) ||
		strings.ContainsAny(p.Name, "\n\r") || utf8.RuneCountInString(p.Name) > MaxAgentNameCharacters {
		return fmt.Errorf("%w: agent name must be one line of 1 to %d characters with no surrounding space", ErrInvalid, MaxAgentNameCharacters)
	}
	if !utf8.ValidString(p.Role) || strings.TrimSpace(p.Role) == "" || utf8.RuneCountInString(p.Role) > MaxAgentRoleCharacters {
		return fmt.Errorf("%w: agent role must be non-blank and at most %d characters", ErrInvalid, MaxAgentRoleCharacters)
	}
	if !utf8.ValidString(p.Instructions) || utf8.RuneCountInString(p.Instructions) > MaxAgentInstructionsCharacters ||
		(p.Instructions != "" && strings.TrimSpace(p.Instructions) == "") {
		return fmt.Errorf("%w: agent instructions must be absent or non-blank and at most %d characters", ErrInvalid, MaxAgentInstructionsCharacters)
	}
	if p.Provider.IsZero() || p.EnvironmentProvider.IsZero() {
		return fmt.Errorf("%w: agent requires a default runtime provider and environment provider", ErrInvalid)
	}
	return nil
}

// Agent is what a tenant delegates work to: a named role with standing
// instructions. It is not a principal; a task delegated to it is still owned
// and authorised by the principal that submitted it (ADR-0020).
//
// Revision counts changes to the profile and Version every change, so
// retiring advances Version alone.
type Agent struct {
	ID        ids.AgentID     `json:"agent_id"`
	Tenant    ids.TenantID    `json:"tenant_id"`
	CreatedBy ids.PrincipalID `json:"created_by"`
	AgentProfile
	Revision  uint32     `json:"revision"`
	State     AgentState `json:"state"`
	Version   uint64     `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// NewAgent returns an active Agent at revision 1 with that revision's record.
func NewAgent(id ids.AgentID, tenant ids.TenantID, creator ids.PrincipalID, profile AgentProfile, at time.Time) (Agent, AgentRevision, error) {
	agent := Agent{
		ID: id, Tenant: tenant, CreatedBy: creator, AgentProfile: profile,
		Revision: 1, State: AgentActive, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	if err := agent.Validate(); err != nil {
		return Agent{}, AgentRevision{}, err
	}
	return agent, agent.revision(creator, at), nil
}

// Validate reports whether the record is complete.
func (a Agent) Validate() error {
	if a.ID.IsZero() || a.Tenant.IsZero() || a.CreatedBy.IsZero() || a.Version == 0 || a.Revision == 0 {
		return fmt.Errorf("%w: agent requires identifiers, a positive revision and a positive version", ErrInvalid)
	}
	if uint64(a.Revision) > a.Version {
		return fmt.Errorf("%w: agent revision cannot exceed its version", ErrInvalid)
	}
	if !slices.Contains(AgentStates(), a.State) {
		return fmt.Errorf("%w: unknown agent state", ErrInvalid)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return fmt.Errorf("%w: agent timestamps are inconsistent", ErrInvalid)
	}
	return a.AgentProfile.validate()
}

// Reference names the agent at its current revision, as a task delegated now
// records it.
func (a Agent) Reference() AgentReference {
	return AgentReference{Agent: a.ID, Revision: a.Revision}
}

// Revise replaces the profile if actor created the agent and it is still at
// expectedVersion. It returns the next revision's record. A profile equal to
// the current one is refused, so every revision differs from the one before.
func (a Agent) Revise(actor ids.PrincipalID, profile AgentProfile, expectedVersion uint64, at time.Time) (Agent, AgentRevision, Change, error) {
	next, change, err := a.apply(AgentRevise, actor, expectedVersion, at)
	if err != nil {
		return a, AgentRevision{}, change, err
	}
	if err := profile.validate(); err != nil {
		return a, AgentRevision{}, change, err
	}
	if profile == a.AgentProfile {
		return a, AgentRevision{}, change, fmt.Errorf("%w: revision changes nothing", ErrInvalid)
	}
	next.AgentProfile = profile
	next.Revision++
	return next, next.revision(actor, at), change, nil
}

// Retire stops new delegation to the agent if actor created it and it is
// still at expectedVersion. Its tasks and revisions stay readable.
func (a Agent) Retire(actor ids.PrincipalID, expectedVersion uint64, at time.Time) (Agent, Change, error) {
	return a.apply(AgentRetire, actor, expectedVersion, at)
}

func (a Agent) apply(event AgentEvent, actor ids.PrincipalID, expectedVersion uint64, at time.Time) (Agent, Change, error) {
	change := Change{
		Tenant: a.Tenant, Record: RecordAgent, RecordID: a.ID.Untyped(),
		Event: string(event), From: string(a.State), Version: a.Version,
	}
	if actor.IsZero() || actor != a.CreatedBy {
		return a, change, reject(change, RejectUnauthorizedActor)
	}
	next, code := agentMachine.next(a.State, a.Version, expectedVersion, event, "")
	if code != "" {
		return a, change, reject(change, code)
	}
	a.State = next
	a.Version++
	a.UpdatedAt = at
	change.To = string(next)
	change.Version = a.Version
	return a, change, nil
}

func (a Agent) revision(author ids.PrincipalID, at time.Time) AgentRevision {
	return AgentRevision{
		Tenant: a.Tenant, Agent: a.ID, Number: a.Revision, AgentProfile: a.AgentProfile,
		CreatedBy: author, CreatedAt: at,
	}
}

// AgentRevision is the immutable profile an Agent had at one revision. Tasks
// refer to it, so it outlives later revisions and the agent's retirement.
type AgentRevision struct {
	Tenant ids.TenantID `json:"tenant_id"`
	Agent  ids.AgentID  `json:"agent_id"`
	Number uint32       `json:"revision"`
	AgentProfile
	CreatedBy ids.PrincipalID `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

// Validate reports whether the record is complete.
func (r AgentRevision) Validate() error {
	if r.Tenant.IsZero() || r.Agent.IsZero() || r.CreatedBy.IsZero() || r.Number == 0 || r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: agent revision requires identifiers, a positive number and a timestamp", ErrInvalid)
	}
	return r.AgentProfile.validate()
}

// AgentReference pins a task to the agent revision it was delegated to.
type AgentReference struct {
	Agent    ids.AgentID `json:"agent_id"`
	Revision uint32      `json:"revision"`
}

// Validate reports whether the reference names an agent and a revision.
func (r AgentReference) Validate() error {
	if r.Agent.IsZero() || r.Revision == 0 {
		return fmt.Errorf("%w: agent reference requires an agent and a positive revision", ErrInvalid)
	}
	return nil
}
