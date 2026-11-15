package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

// Agents (ADR-0020) are tenant records: every principal of the tenant reads
// them, and the principal that created one revises or retires it.

const (
	commandCreateAgent = "create_agent"
	commandReviseAgent = "revise_agent"
	commandRetireAgent = "retire_agent"
)

var (
	// ErrNoAgent reports a submission naming an agent that does not exist in
	// the tenant or is retired.
	ErrNoAgent = errors.New("no active agent with the selected id")
	// ErrNoRuntime reports a default runtime provider that is not registered
	// for this tenant.
	ErrNoRuntime = errors.New("no registered runtime provider for the selected id")
	// ErrAgentNameTaken reports a name another active agent of the tenant
	// holds.
	ErrAgentNameTaken = store.ErrAgentNameTaken
	// ErrSchemaUnavailable reports an operation the database's schema does not
	// support yet, during a rollout that has not migrated (ADR-0011).
	ErrSchemaUnavailable = store.ErrSchemaUnavailable
)

// CreateAgentCommand creates an active agent at revision 1.
type CreateAgentCommand struct {
	Command
	At      time.Time
	Profile lifecycle.AgentProfile
}

// ReviseAgentCommand replaces an agent's profile. ExpectedVersion is the agent
// version the change was based on.
type ReviseAgentCommand struct {
	Command
	Agent   ids.AgentID
	At      time.Time
	Profile lifecycle.AgentProfile
}

// RetireAgentCommand stops new delegation to an agent. ExpectedVersion is the agent
// version the retirement was based on.
type RetireAgentCommand struct {
	Command
	Agent ids.AgentID
	At    time.Time
}

type agentPayload struct {
	Agent           string                 `json:"agent,omitempty"`
	ExpectedVersion uint64                 `json:"expected_version,omitempty"`
	Profile         lifecycle.AgentProfile `json:"profile"`
}

type retireAgentPayload struct {
	Agent           string `json:"agent"`
	ExpectedVersion uint64 `json:"expected_version"`
}

// CreateAgent runs the command. A replay with the same nonce and payload returns
// the agent as it was created.
func CreateAgent(ctx context.Context, pool *pgxpool.Pool, cmd CreateAgentCommand) (lifecycle.Agent, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(true); err != nil {
		return lifecycle.Agent{}, err
	}
	if cmd.At.IsZero() {
		return lifecycle.Agent{}, fmt.Errorf("%w: a command time is required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandCreateAgent, agentPayload{Profile: cmd.Profile})
	if err != nil {
		return lifecycle.Agent{}, err
	}
	return agentResult(execute(ctx, pool, cmd.Command, commandCreateAgent, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		principal := cmd.Actor.Principal()
		if err := requireAgentProviders(ctx, u.uow, principal.Tenant, cmd.Profile); err != nil {
			return nil, err
		}
		id, err := ids.New[ids.Agent]()
		if err != nil {
			return nil, err
		}
		agent, revision, err := lifecycle.NewAgent(id, principal.Tenant, principal.ID, cmd.Profile, cmd.At)
		if err != nil {
			return nil, err
		}
		if _, _, err := store.InsertAgent(ctx, u.uow, agent, revision); err != nil {
			return nil, err
		}
		return json.Marshal(agent)
	}))
}

// ReviseAgent runs the command. A replay with the same nonce and payload returns
// the agent as that revision left it.
func ReviseAgent(ctx context.Context, pool *pgxpool.Pool, cmd ReviseAgentCommand) (lifecycle.Agent, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(false); err != nil {
		return lifecycle.Agent{}, err
	}
	if cmd.At.IsZero() || cmd.Agent.IsZero() {
		return lifecycle.Agent{}, fmt.Errorf("%w: an agent and a command time are required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandReviseAgent, agentPayload{
		Agent: cmd.Agent.String(), ExpectedVersion: cmd.ExpectedVersion, Profile: cmd.Profile,
	})
	if err != nil {
		return lifecycle.Agent{}, err
	}
	return agentResult(execute(ctx, pool, cmd.Command, commandReviseAgent, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		tenant := cmd.Actor.Principal().Tenant
		if err := requireAgentProviders(ctx, u.uow, tenant, cmd.Profile); err != nil {
			return nil, err
		}
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		agent, replayed, err := store.ReviseAgent(ctx, u.uow, tenant, cmd.Agent, cmd.Profile, tr)
		if replayed {
			err = store.ErrStaleVersion
		}
		if err != nil {
			return nil, agentRefusal(ctx, u, tenant, cmd.Agent, lifecycle.AgentRevise, tr.ExpectedVersion, err)
		}
		return json.Marshal(agent)
	}))
}

// RetireAgent runs the command. A replay with the same nonce and payload returns
// the agent as the retirement left it.
func RetireAgent(ctx context.Context, pool *pgxpool.Pool, cmd RetireAgentCommand) (lifecycle.Agent, error) {
	cmd.At = cmd.At.UTC().Truncate(time.Microsecond)
	if err := cmd.Command.validate(false); err != nil {
		return lifecycle.Agent{}, err
	}
	if cmd.At.IsZero() || cmd.Agent.IsZero() {
		return lifecycle.Agent{}, fmt.Errorf("%w: an agent and a command time are required", ErrInvalidCommand)
	}
	digest, err := fingerprint(commandRetireAgent, retireAgentPayload{Agent: cmd.Agent.String(), ExpectedVersion: cmd.ExpectedVersion})
	if err != nil {
		return lifecycle.Agent{}, err
	}
	return agentResult(execute(ctx, pool, cmd.Command, commandRetireAgent, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		tenant := cmd.Actor.Principal().Tenant
		agent, replayed, err := store.RetireAgent(ctx, u.uow, tenant, cmd.Agent, tr)
		if replayed {
			err = store.ErrStaleVersion
		}
		if err != nil {
			return nil, agentRefusal(ctx, u, tenant, cmd.Agent, lifecycle.AgentRetire, tr.ExpectedVersion, err)
		}
		return json.Marshal(agent)
	}))
}

// requireAgentProviders checks the profile's default providers are
// registered in their roles, which the database also enforces, so the
// refusal names the provider instead of a constraint.
func requireAgentProviders(ctx context.Context, q store.Querier, tenant ids.TenantID, profile lifecycle.AgentProfile) error {
	if profile.Provider.IsZero() || profile.EnvironmentProvider.IsZero() {
		return fmt.Errorf("%w: an agent requires a default runtime provider and environment provider", ErrInvalidCommand)
	}
	if _, err := store.GetProviderRegistration(ctx, q, tenant, profile.Provider, contracts.RoleRuntime); errors.Is(err, store.ErrNotFound) {
		return ErrNoRuntime
	} else if err != nil {
		return err
	}
	if _, err := store.GetProviderRegistration(ctx, q, tenant, profile.EnvironmentProvider, contracts.RoleEnvironment); errors.Is(err, store.ErrNotFound) {
		return ErrNoEnvironment
	} else if err != nil {
		return err
	}
	return nil
}

func agentResult(raw json.RawMessage, err error) (lifecycle.Agent, error) {
	if err != nil {
		return lifecycle.Agent{}, err
	}
	var agent lifecycle.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return lifecycle.Agent{}, fmt.Errorf("agent record: %w", err)
	}
	return agent, nil
}

// GetAgent reads one of the tenant's agents.
func GetAgent(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.AgentID) (lifecycle.Agent, error) {
	var agent lifecycle.Agent
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		agent, err = store.GetAgent(ctx, r.uow, r.principal.Tenant, id)
		return err
	})
	return agent, err
}

// ListAgents reads the tenant's agents, retired ones included, oldest first.
func ListAgents(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext) ([]lifecycle.Agent, error) {
	var agents []lifecycle.Agent
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		agents, err = store.ListAgents(ctx, r.uow, r.principal.Tenant)
		return err
	})
	return agents, err
}

// GetAgentRevision reads one revision of one of the tenant's agents, such as
// the one a task was delegated to.
func GetAgentRevision(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.AgentID, number uint32) (lifecycle.AgentRevision, error) {
	var revision lifecycle.AgentRevision
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		revision, err = store.GetAgentRevision(ctx, r.uow, r.principal.Tenant, id, number)
		return err
	})
	return revision, err
}

// agentRefusal turns the store's stale version into the lifecycle's version
// conflict, so it is audited and answered as every other stale command is.
// A store replay counts as stale here: execute already answered a retry of
// this command from its nonce, so a replay reached inside a command is another
// command decided on a version that has moved, as in CancelTask.
// Anything else is returned unchanged.
func agentRefusal(ctx context.Context, u *unit, tenant ids.TenantID, id ids.AgentID, event lifecycle.AgentEvent, expected uint64, err error) error {
	if !errors.Is(err, store.ErrStaleVersion) {
		return err
	}
	agent, loadErr := store.GetAgent(ctx, u.uow, tenant, id)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	return staleRefusal(tenant, lifecycle.RecordAgent, id.Untyped(), string(event), string(agent.State), expected)
}
