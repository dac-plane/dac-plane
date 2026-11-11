//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/platform/migrations"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	testpostgres "github.com/dac-plane/dac-plane/internal/testsupport/postgres"
)

type agentFixture struct {
	tenantFixture
	runtime     ids.ProviderID
	environment ids.ProviderID
}

func seedAgentTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) agentFixture {
	t.Helper()
	f := agentFixture{tenantFixture: seedTenant(t, ctx, pool), runtime: mustID[ids.Provider](t), environment: mustID[ids.Provider](t)}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		for provider, role := range map[ids.ProviderID]contracts.Role{f.runtime: contracts.RoleRuntime, f.environment: contracts.RoleEnvironment} {
			if err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{
				Tenant: f.tenant, Provider: provider, Role: role, UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("PutProviderRegistration(%s) error = %v", role, err)
			}
		}
	})
	return f
}

func (f agentFixture) profile(name string) lifecycle.AgentProfile {
	return lifecycle.AgentProfile{
		Name: name, Role: "Reviews changes for correctness", Instructions: "Read the whole change first.",
		Provider: f.runtime, EnvironmentProvider: f.environment,
	}
}

func inTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, work func(*store.UnitOfWork)) {
	t.Helper()
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = uow.Rollback(ctx) }()
	work(uow)
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

// tryTx runs work in its own transaction and commits only when it succeeds,
// so a refused write leaves nothing behind.
func tryTx[T any](t *testing.T, ctx context.Context, pool *pgxpool.Pool, work func(*store.UnitOfWork) (T, error)) (T, error) {
	t.Helper()
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = uow.Rollback(ctx) }()
	value, err := work(uow)
	if err != nil {
		return value, err
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return value, nil
}

func createAgent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f agentFixture, name string) lifecycle.Agent {
	t.Helper()
	agent, revision, err := lifecycle.NewAgent(mustID[ids.Agent](t), f.tenant, f.principal, f.profile(name), time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if _, replayed, err := store.InsertAgent(ctx, uow, agent, revision); err != nil || replayed {
			t.Fatalf("InsertAgent() = %v, %v", replayed, err)
		}
	})
	return agent
}

func countQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAgentInsertReadAndReplay(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)
	agent := createAgent(t, ctx, pool, f, "Code reviewer")

	stored, err := store.GetAgent(ctx, pool, f.tenant, agent.ID)
	if err != nil {
		t.Fatalf("GetAgent() error = %v", err)
	}
	if stored.AgentProfile != agent.AgentProfile || stored.Revision != 1 || stored.Version != 1 || stored.State != lifecycle.AgentActive || stored.CreatedBy != f.principal {
		t.Fatalf("GetAgent() = %+v, want %+v", stored, agent)
	}
	first, err := store.GetAgentRevision(ctx, pool, f.tenant, agent.ID, 1)
	if err != nil || first.AgentProfile != agent.AgentProfile || first.CreatedBy != f.principal {
		t.Fatalf("GetAgentRevision(1) = %+v, %v", first, err)
	}
	if _, err := store.GetAgentRevision(ctx, pool, f.tenant, agent.ID, 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgentRevision(2) error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetAgent(ctx, pool, mustID[ids.Tenant](t), agent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgent() from another tenant error = %v, want ErrNotFound", err)
	}

	revision := lifecycle.AgentRevision{Tenant: f.tenant, Agent: agent.ID, Number: 1, AgentProfile: agent.AgentProfile, CreatedBy: f.principal, CreatedAt: agent.CreatedAt}
	replayed, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertAgent(ctx, uow, agent, revision)
		return replayed, err
	})
	if err != nil || !replayed {
		t.Fatalf("InsertAgent() again = %v, %v, want a replay", replayed, err)
	}
	changed := agent
	changed.Role = "Something else"
	changedRevision := revision
	changedRevision.AgentProfile = changed.AgentProfile
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertAgent(ctx, uow, changed, changedRevision)
		return replayed, err
	}); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("InsertAgent() with another profile error = %v, want ErrPayloadMismatch", err)
	}
	if n := countQuery(t, ctx, pool, `select count(*) from agent_revision where agent_id = $1`, agent.ID.String()); n != 1 {
		t.Fatalf("revisions after replays = %d, want 1", n)
	}
}

func TestAgentNameIsUniqueAmongActiveAgents(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)
	first := createAgent(t, ctx, pool, f, "Code reviewer")

	duplicate, revision, err := lifecycle.NewAgent(mustID[ids.Agent](t), f.tenant, f.principal, f.profile("Code reviewer"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertAgent(ctx, uow, duplicate, revision)
		return replayed, err
	}); !errors.Is(err, store.ErrAgentNameTaken) {
		t.Fatalf("InsertAgent() with a taken name error = %v, want ErrAgentNameTaken", err)
	}

	other := createAgent(t, ctx, pool, f, "Tester")
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (lifecycle.Agent, error) {
		next, _, err := store.ReviseAgent(ctx, uow, f.tenant, other.ID, f.profile("Code reviewer"), store.Transition{
			ExpectedVersion: other.Version, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return next, err
	}); !errors.Is(err, store.ErrAgentNameTaken) {
		t.Fatalf("ReviseAgent() to a taken name error = %v, want ErrAgentNameTaken", err)
	}

	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (lifecycle.Agent, error) {
		next, _, err := store.RetireAgent(ctx, uow, f.tenant, first.ID, store.Transition{
			ExpectedVersion: first.Version, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return next, err
	}); err != nil {
		t.Fatalf("RetireAgent() error = %v", err)
	}
	createAgent(t, ctx, pool, f, "Code reviewer")

	elsewhere := seedAgentTenant(t, ctx, pool)
	createAgent(t, ctx, pool, elsewhere, "Tester")
}

func TestAgentReviseAppendsARevisionAndAudits(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)
	agent := createAgent(t, ctx, pool, f, "Code reviewer")
	profile := agent.AgentProfile
	profile.Instructions = ""
	tr := store.Transition{ExpectedVersion: agent.Version, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t)}

	revised, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (lifecycle.Agent, error) {
		next, _, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, profile, tr)
		return next, err
	})
	if err != nil {
		t.Fatalf("ReviseAgent() error = %v", err)
	}
	if revised.Revision != 2 || revised.Version != 2 || revised.AgentProfile != profile {
		t.Fatalf("ReviseAgent() = %+v", revised)
	}
	stored, err := store.GetAgent(ctx, pool, f.tenant, agent.ID)
	if err != nil || stored.AgentProfile != profile || stored.Revision != 2 {
		t.Fatalf("GetAgent() after revise = %+v, %v", stored, err)
	}
	first, err := store.GetAgentRevision(ctx, pool, f.tenant, agent.ID, 1)
	if err != nil || first.AgentProfile != agent.AgentProfile {
		t.Fatalf("revision 1 after revise = %+v, %v", first, err)
	}
	second, err := store.GetAgentRevision(ctx, pool, f.tenant, agent.ID, 2)
	if err != nil || second.AgentProfile != profile {
		t.Fatalf("revision 2 = %+v, %v", second, err)
	}
	if n := countQuery(t, ctx, pool, `select count(*) from audit_event where record_kind = 'agent' and record_id = $1 and event = 'revise' and outcome = 'applied'`, agent.ID.String()); n != 1 {
		t.Fatalf("applied revise audits = %d, want 1", n)
	}
	if n := countQuery(t, ctx, pool, `select count(*) from durable_event where record_kind = 'agent' and record_id = $1 and version = 2`, agent.ID.String()); n != 1 {
		t.Fatalf("revise durable events = %d, want 1", n)
	}
	if n := countQuery(t, ctx, pool, `select count(*) from event_position p join durable_event e using (tenant_id, event_id) where e.record_id = $1 and p.task_id is null`, agent.ID.String()); n != 1 {
		t.Fatalf("positioned agent events outside any task = %d, want 1", n)
	}

	replayed, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, profile, tr)
		return replayed, err
	})
	if err != nil || !replayed {
		t.Fatalf("ReviseAgent() replay = %v, %v", replayed, err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, f.profile("Late"), store.Transition{
			ExpectedVersion: 0, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return replayed, err
	}); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("ReviseAgent() at a stale version error = %v, want ErrStaleVersion", err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.ReviseAgent(ctx, uow, mustID[ids.Tenant](t), agent.ID, f.profile("Other"), tr)
		return replayed, err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ReviseAgent() from another tenant error = %v, want ErrNotFound", err)
	}
}

func TestAgentRefusalsAreAuditedAndChangeNothing(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)
	agent := createAgent(t, ctx, pool, f, "Code reviewer")
	stranger := lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: mustID[ids.Principal](t)}

	// The refusal's audit row is committed, as the command boundary does.
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		_, _, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, f.profile("Taken over"), store.Transition{
			ExpectedVersion: agent.Version, At: time.Now().UTC(), Actor: stranger, AuditID: newAuditID(t),
		})
		if lifecycle.RejectionOf(err) != lifecycle.RejectUnauthorizedActor {
			t.Fatalf("ReviseAgent() by another principal error = %v, want unauthorized_actor", err)
		}
	})
	if n := countQuery(t, ctx, pool, `select count(*) from audit_event where record_id = $1 and outcome = 'rejected' and reason = 'unauthorized_actor'`, agent.ID.String()); n != 1 {
		t.Fatalf("refusal audits = %d, want 1", n)
	}

	_, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, _, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, agent.AgentProfile, store.Transition{
			ExpectedVersion: agent.Version, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return false, err
	})
	if !errors.Is(err, lifecycle.ErrInvalid) {
		t.Fatalf("ReviseAgent() with the same profile error = %v, want ErrInvalid", err)
	}
	stored, err := store.GetAgent(ctx, pool, f.tenant, agent.ID)
	if err != nil || stored.Version != 1 || stored.Revision != 1 || stored.Name != agent.Name {
		t.Fatalf("agent after refusals = %+v, %v", stored, err)
	}

	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (lifecycle.Agent, error) {
		next, _, err := store.RetireAgent(ctx, uow, f.tenant, agent.ID, store.Transition{
			ExpectedVersion: agent.Version, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return next, err
	}); err != nil {
		t.Fatalf("RetireAgent() error = %v", err)
	}
	_, err = tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, _, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, f.profile("Revived"), store.Transition{
			ExpectedVersion: agent.Version + 1, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return false, err
	})
	if lifecycle.RejectionOf(err) != lifecycle.RejectTerminalState {
		t.Fatalf("ReviseAgent() after retiring error = %v, want terminal_state", err)
	}
}

func TestAgentRequiresRegisteredProvidersAndKeepsRevisions(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)

	profile := f.profile("Unregistered")
	profile.Provider = f.environment // registered, but not as a runtime
	agent, revision, err := lifecycle.NewAgent(mustID[ids.Agent](t), f.tenant, f.principal, profile, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertAgent(ctx, uow, agent, revision)
		return replayed, err
	}); err == nil {
		t.Fatal("InsertAgent() accepted an environment provider as the default runtime")
	}

	kept := createAgent(t, ctx, pool, f, "Code reviewer")
	if _, err := pool.Exec(ctx, `update agent_revision set role = 'rewritten' where agent_id = $1`, kept.ID.String()); err == nil {
		t.Fatal("an agent revision was updated")
	}
	if _, err := pool.Exec(ctx, `delete from agent_revision where agent_id = $1`, kept.ID.String()); err == nil {
		t.Fatal("an agent revision was deleted")
	}
}

func TestTaskKeepsItsAgentReference(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedAgentTenant(t, ctx, pool)
	agent := createAgent(t, ctx, pool, f, "Code reviewer")
	at := time.Now().UTC().Truncate(time.Microsecond)

	task, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, at)
	if err != nil {
		t.Fatal(err)
	}
	reference := agent.Reference()
	task.Agent = &reference
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if _, replayed, err := store.InsertTask(ctx, uow, task); err != nil || replayed {
			t.Fatalf("InsertTask() = %v, %v", replayed, err)
		}
	})
	stored, err := store.GetTask(ctx, pool, f.tenant, task.ID)
	if err != nil || stored.Agent == nil || *stored.Agent != reference {
		t.Fatalf("GetTask() agent = %+v, %v, want %+v", stored.Agent, err, reference)
	}

	other := task
	other.Agent = &lifecycle.AgentReference{Agent: agent.ID, Revision: 1}
	without := task
	without.Agent = nil
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertTask(ctx, uow, without)
		return replayed, err
	}); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("InsertTask() replay without the agent error = %v, want ErrPayloadMismatch", err)
	}
	if replayed, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertTask(ctx, uow, other)
		return replayed, err
	}); err != nil || !replayed {
		t.Fatalf("InsertTask() replay with the same agent = %v, %v", replayed, err)
	}

	missing, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, at)
	if err != nil {
		t.Fatal(err)
	}
	missing.Agent = &lifecycle.AgentReference{Agent: agent.ID, Revision: 9}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertTask(ctx, uow, missing)
		return replayed, err
	}); err == nil {
		t.Fatal("InsertTask() accepted a reference to a revision that does not exist")
	}

	plain := createTaskInTx(t, ctx, pool, f.tenantFixture, at)
	if plain.Agent != nil {
		t.Fatalf("task without an agent read back %+v", plain.Agent)
	}
}

func createTaskInTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f tenantFixture, at time.Time) lifecycle.Task {
	t.Helper()
	var task lifecycle.Task
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) { task = createTask(t, ctx, uow, f, at) })
	stored, err := store.GetTask(ctx, pool, f.tenant, task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	return stored
}

// TestAgentsAreUnavailableOnSchema16 is the rollout state ADR-0011 requires
// a new binary to serve: the database is still at the previous schema.
func TestAgentsAreUnavailableOnSchema16(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("DownTo(16) error = %v", err)
	}
	pool := openPool(t, ctx, databaseURL)
	f := seedAgentTenant(t, ctx, pool)

	agent, revision, err := lifecycle.NewAgent(mustID[ids.Agent](t), f.tenant, f.principal, f.profile("Code reviewer"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertAgent(ctx, uow, agent, revision)
		return replayed, err
	}); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Fatalf("InsertAgent() on schema 16 error = %v, want ErrSchemaUnavailable", err)
	}
	if _, err := store.GetAgent(ctx, pool, f.tenant, agent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgent() on schema 16 error = %v, want ErrNotFound", err)
	}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.ReviseAgent(ctx, uow, f.tenant, agent.ID, f.profile("Other"), store.Transition{
			ExpectedVersion: 1, At: time.Now().UTC(), Actor: f.actor, AuditID: newAuditID(t),
		})
		return replayed, err
	}); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Fatalf("ReviseAgent() on schema 16 error = %v, want ErrSchemaUnavailable", err)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	delegated, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, at)
	if err != nil {
		t.Fatal(err)
	}
	delegated.Agent = &lifecycle.AgentReference{Agent: agent.ID, Revision: 1}
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		_, replayed, err := store.InsertTask(ctx, uow, delegated)
		return replayed, err
	}); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Fatalf("InsertTask() with an agent on schema 16 error = %v, want ErrSchemaUnavailable", err)
	}
	if plain := createTaskInTx(t, ctx, pool, f.tenantFixture, at); plain.Agent != nil {
		t.Fatalf("task on schema 16 read back agent %+v", plain.Agent)
	}
}

func TestAgentsRefuseDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}

	createAgent(t, ctx, pool, seedAgentTenant(t, ctx, pool), "Code reviewer")
	if _, err := provider.DownTo(ctx, 16); err == nil || !strings.Contains(err.Error(), "cannot downgrade while version 17 agents exist") {
		t.Fatalf("DownTo(16) with an agent error = %v, want the refusal", err)
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != migrations.CurrentVersion {
		t.Fatalf("version after the refused downgrade = %d, %v", version, err)
	}
}
