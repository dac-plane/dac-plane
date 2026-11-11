package postgres

import (
	"context"
	"errors"

	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// agentSchemaVersion is the first schema with agents (ADR-0020). A binary
// serving an earlier schema during a rollout refuses agent operations and
// delegation to an agent, and serves everything else (ADR-0011).
const agentSchemaVersion = 17

var (
	// ErrSchemaUnavailable reports an operation the database's schema does
	// not support yet. Nothing is written; it succeeds once the migration
	// that adds it has run.
	ErrSchemaUnavailable = errors.New("not available until the database is migrated")
	// ErrAgentNameTaken reports a name another active agent of the tenant
	// already holds.
	ErrAgentNameTaken = errors.New("an active agent of this tenant already has this name")
)

const agentActiveNameIndex = "agent_active_name"

const agentColumns = `agent_id::text, tenant_id::text, created_by::text, name, role, coalesce(instructions, ''),
	provider_id::text, environment_provider_id::text, revision, state, version, created_at, updated_at`

func requireAgentSchema(ctx context.Context, q Querier) error {
	version, err := SchemaVersion(ctx, q)
	if err != nil {
		return err
	}
	if version < agentSchemaVersion {
		return ErrSchemaUnavailable
	}
	return nil
}

// InsertAgent persists a new agent with its first revision. A primary-key
// conflict loads the stored agent: an identical one replays, anything else
// mismatches. A name another active agent holds is ErrAgentNameTaken.
func InsertAgent(ctx context.Context, u *UnitOfWork, agent lifecycle.Agent, revision lifecycle.AgentRevision) (lifecycle.Agent, bool, error) {
	if err := agent.Validate(); err != nil {
		return lifecycle.Agent{}, false, err
	}
	if err := revision.Validate(); err != nil {
		return lifecycle.Agent{}, false, err
	}
	if agent.Revision != 1 || agent.Version != 1 || agent.State != lifecycle.AgentActive ||
		revision.Tenant != agent.Tenant || revision.Agent != agent.ID || revision.Number != 1 ||
		revision.AgentProfile != agent.AgentProfile || revision.CreatedBy != agent.CreatedBy {
		return lifecycle.Agent{}, false, lifecycle.ErrInvalid
	}
	if err := requireAgentSchema(ctx, u); err != nil {
		return lifecycle.Agent{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into agent (tenant_id, agent_id, created_by, name, role, instructions, provider_id, environment_provider_id,
		                   revision, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		agent.Tenant.String(), agent.ID.String(), agent.CreatedBy.String(), agent.Name, agent.Role, textOrNil(agent.Instructions),
		agent.Provider.String(), agent.EnvironmentProvider.String(),
		int64(agent.Revision), string(agent.State), int64(agent.Version), agent.CreatedAt, agent.UpdatedAt,
	)
	if err != nil {
		return lifecycle.Agent{}, false, err
	}
	if !inserted {
		stored, loadErr := GetAgent(ctx, u, agent.Tenant, agent.ID)
		if isNotFound(loadErr) {
			// Only the active-name index can refuse a new identifier.
			return lifecycle.Agent{}, false, ErrAgentNameTaken
		}
		// Timestamps are compared by the command record, not here: the
		// database stores them at microsecond precision.
		equal := stored.Tenant == agent.Tenant && stored.CreatedBy == agent.CreatedBy &&
			stored.AgentProfile == agent.AgentProfile && stored.Revision == agent.Revision &&
			stored.State == agent.State && stored.Version == agent.Version
		return replayResult(lifecycle.Agent{}, stored, loadErr, equal)
	}
	if err := insertAgentRevision(ctx, u, revision); err != nil {
		return lifecycle.Agent{}, false, err
	}
	return agent, false, nil
}

// GetAgent loads an agent by tenant and id. A database before the agent
// schema has none, so it reports ErrNotFound.
func GetAgent(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AgentID) (lifecycle.Agent, error) {
	if err := requireAgentSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return lifecycle.Agent{}, ErrNotFound
		}
		return lifecycle.Agent{}, err
	}
	return scanAgent(q.QueryRow(ctx, `select `+agentColumns+` from agent where tenant_id = $1 and agent_id = $2`,
		tenant.String(), id.String()))
}

// GetAgentRevision loads one revision of an agent.
func GetAgentRevision(ctx context.Context, q Querier, tenant ids.TenantID, id ids.AgentID, number uint32) (lifecycle.AgentRevision, error) {
	if err := requireAgentSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return lifecycle.AgentRevision{}, ErrNotFound
		}
		return lifecycle.AgentRevision{}, err
	}
	var revision lifecycle.AgentRevision
	var tenantText, agentText, providerText, environmentText, creatorText string
	err := q.QueryRow(ctx, `
		select tenant_id::text, agent_id::text, revision, name, role, coalesce(instructions, ''),
		       provider_id::text, environment_provider_id::text, created_by::text, created_at
		from agent_revision where tenant_id = $1 and agent_id = $2 and revision = $3`,
		tenant.String(), id.String(), int64(number),
	).Scan(&tenantText, &agentText, &revision.Number, &revision.Name, &revision.Role, &revision.Instructions,
		&providerText, &environmentText, &creatorText, &revision.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.AgentRevision{}, ErrNotFound
		}
		return lifecycle.AgentRevision{}, err
	}
	if revision.Tenant, err = parseID[ids.Tenant]("agent_revision.tenant_id", tenantText); err != nil {
		return lifecycle.AgentRevision{}, err
	}
	if revision.Agent, err = parseID[ids.Agent]("agent_revision.agent_id", agentText); err != nil {
		return lifecycle.AgentRevision{}, err
	}
	if revision.Provider, err = parseID[ids.Provider]("agent_revision.provider_id", providerText); err != nil {
		return lifecycle.AgentRevision{}, err
	}
	if revision.EnvironmentProvider, err = parseID[ids.Provider]("agent_revision.environment_provider_id", environmentText); err != nil {
		return lifecycle.AgentRevision{}, err
	}
	if revision.CreatedBy, err = parseID[ids.Principal]("agent_revision.created_by", creatorText); err != nil {
		return lifecycle.AgentRevision{}, err
	}
	if err := revision.Validate(); err != nil {
		return lifecycle.AgentRevision{}, corruptRow("agent_revision", err)
	}
	return revision, nil
}

// ReviseAgent replaces an agent's profile at the expected version, appends
// the next revision and records audit and durable event rows. The actor must
// be the principal that created the agent. A refusal by the lifecycle is
// audited and returned; the row is unchanged.
func ReviseAgent(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AgentID, profile lifecycle.AgentProfile, tr Transition) (lifecycle.Agent, bool, error) {
	agent, replayed, err := lockAgentForTransition(ctx, u, tenant, id, lifecycle.AgentRevise, tr)
	if err != nil || replayed {
		return agent, replayed, err
	}
	next, revision, change, err := agent.Revise(tr.Actor.Principal, profile, tr.ExpectedVersion, tr.At)
	if err != nil {
		return refuseAgent(ctx, u, agent, err, tr)
	}
	// Checked before the update, because a unique violation would abort the
	// transaction. The index still decides between concurrent writers.
	if next.Name != agent.Name {
		var taken bool
		if err := u.QueryRow(ctx, `
			select exists (select 1 from agent where tenant_id = $1 and name = $2 and state = 'active' and agent_id <> $3)`,
			tenant.String(), next.Name, id.String()).Scan(&taken); err != nil {
			return lifecycle.Agent{}, false, err
		}
		if taken {
			return lifecycle.Agent{}, false, ErrAgentNameTaken
		}
	}
	if err := execVersionedUpdate(ctx, u, `
		update agent set name = $3, role = $4, instructions = $5, provider_id = $6, environment_provider_id = $7,
		                 revision = $8, version = $9, updated_at = $10
		where tenant_id = $1 and agent_id = $2 and version = $11`,
		tenant.String(), id.String(), next.Name, next.Role, textOrNil(next.Instructions),
		next.Provider.String(), next.EnvironmentProvider.String(),
		int64(next.Revision), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	); err != nil {
		if isUniqueViolation(err, agentActiveNameIndex) {
			return lifecycle.Agent{}, false, ErrAgentNameTaken
		}
		return lifecycle.Agent{}, false, err
	}
	if err := insertAgentRevision(ctx, u, revision); err != nil {
		return lifecycle.Agent{}, false, err
	}
	if err := appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox); err != nil {
		return lifecycle.Agent{}, false, err
	}
	return next, false, nil
}

// RetireAgent stops new delegation to an agent at the expected version and
// records audit and durable event rows. Its revisions and tasks are kept.
func RetireAgent(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AgentID, tr Transition) (lifecycle.Agent, bool, error) {
	agent, replayed, err := lockAgentForTransition(ctx, u, tenant, id, lifecycle.AgentRetire, tr)
	if err != nil || replayed {
		return agent, replayed, err
	}
	next, change, err := agent.Retire(tr.Actor.Principal, tr.ExpectedVersion, tr.At)
	if err != nil {
		return refuseAgent(ctx, u, agent, err, tr)
	}
	if err := execVersionedUpdate(ctx, u, `
		update agent set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and agent_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(tr.ExpectedVersion),
	); err != nil {
		return lifecycle.Agent{}, false, err
	}
	if err := appendHistory(ctx, u, change, change.Audit(tr.AuditID, tr.Actor, tr.At), tr.Outbox); err != nil {
		return lifecycle.Agent{}, false, err
	}
	return next, false, nil
}

// lockAgentForTransition locks and loads the agent. When the stored version
// is one past the expected one and that version carries event, the command
// already committed and the stored agent replays.
func lockAgentForTransition(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AgentID, event lifecycle.AgentEvent, tr Transition) (lifecycle.Agent, bool, error) {
	if err := requireAgentSchema(ctx, u); err != nil {
		return lifecycle.Agent{}, false, err
	}
	agent, err := scanAgent(u.QueryRow(ctx, `select `+agentColumns+` from agent where tenant_id = $1 and agent_id = $2 for update`,
		tenant.String(), id.String()))
	if err != nil {
		return lifecycle.Agent{}, false, err
	}
	if agent.Version == tr.ExpectedVersion {
		return agent, false, nil
	}
	if agent.Version == tr.ExpectedVersion+1 {
		replayed, replayErr := replayIfCommitted(ctx, u, ReplayCheck{
			Tenant: tenant, Record: lifecycle.RecordAgent, RecordID: agent.ID.Untyped(), Committed: agent.Version, Event: string(event),
		})
		if replayErr != nil {
			return lifecycle.Agent{}, false, replayErr
		}
		if replayed {
			return agent, true, nil
		}
	}
	return lifecycle.Agent{}, false, ErrStaleVersion
}

// refuseAgent audits a lifecycle refusal and returns it with the unchanged
// agent. Invalid input is returned without an audit row.
func refuseAgent(ctx context.Context, u *UnitOfWork, agent lifecycle.Agent, err error, tr Transition) (lifecycle.Agent, bool, error) {
	var refusal *lifecycle.TransitionError
	if errors.As(err, &refusal) {
		if histErr := appendRefusal(ctx, u, refusal.Audit(tr.AuditID, tr.Actor, tr.At)); histErr != nil {
			return lifecycle.Agent{}, false, histErr
		}
	}
	return agent, false, err
}

func insertAgentRevision(ctx context.Context, u *UnitOfWork, revision lifecycle.AgentRevision) error {
	_, err := u.Exec(ctx, `
		insert into agent_revision (tenant_id, agent_id, revision, name, role, instructions, provider_id, environment_provider_id,
		                            created_by, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		revision.Tenant.String(), revision.Agent.String(), int64(revision.Number), revision.Name, revision.Role,
		textOrNil(revision.Instructions), revision.Provider.String(), revision.EnvironmentProvider.String(),
		revision.CreatedBy.String(), revision.CreatedAt,
	)
	return err
}

func scanAgent(row interface{ Scan(...any) error }) (lifecycle.Agent, error) {
	var agent lifecycle.Agent
	var idText, tenantText, creatorText, providerText, environmentText, state string
	err := row.Scan(&idText, &tenantText, &creatorText, &agent.Name, &agent.Role, &agent.Instructions,
		&providerText, &environmentText, &agent.Revision, &state, &agent.Version, &agent.CreatedAt, &agent.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Agent{}, ErrNotFound
		}
		return lifecycle.Agent{}, err
	}
	if agent.ID, err = parseID[ids.Agent]("agent.agent_id", idText); err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.Tenant, err = parseID[ids.Tenant]("agent.tenant_id", tenantText); err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.CreatedBy, err = parseID[ids.Principal]("agent.created_by", creatorText); err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.Provider, err = parseID[ids.Provider]("agent.provider_id", providerText); err != nil {
		return lifecycle.Agent{}, err
	}
	if agent.EnvironmentProvider, err = parseID[ids.Provider]("agent.environment_provider_id", environmentText); err != nil {
		return lifecycle.Agent{}, err
	}
	agent.State = lifecycle.AgentState(state)
	if err := agent.Validate(); err != nil {
		return lifecycle.Agent{}, corruptRow("agent", err)
	}
	return agent, nil
}
