//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/platform/migrations"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	testpostgres "github.com/dac-plane/dac-plane/internal/testsupport/postgres"
)

type connectorFixture struct {
	agentFixture
	connector ids.ProviderID
}

func seedConnectorTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) connectorFixture {
	t.Helper()
	f := connectorFixture{agentFixture: seedAgentTenant(t, ctx, pool), connector: mustID[ids.Provider](t)}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{
			Tenant: f.tenant, Provider: f.connector, Role: contracts.RoleConnector, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("PutProviderRegistration(connector) error = %v", err)
		}
	})
	return f
}

func (f connectorFixture) transition(expected uint64) store.Transition {
	return store.Transition{ExpectedVersion: expected, At: time.Now().UTC().Truncate(time.Microsecond), Actor: f.actor, AuditID: ids.AuditID{}}
}

func createActiveConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f connectorFixture) lifecycle.Connection {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	c, err := lifecycle.NewConnection(mustID[ids.Connection](t), f.tenant, f.connector, f.principal, "Team GitHub", at)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.InsertConnection(ctx, uow, c); err != nil {
			t.Fatalf("InsertConnection() error = %v", err)
		}
		tr := f.transition(1)
		tr.AuditID = newAuditID(t)
		if c, err = store.CompleteConnection(ctx, uow, f.tenant, c.ID, lifecycle.ConnectionAccount{Account: "octocat", Permissions: []string{"issues:write"}}, tr); err != nil {
			t.Fatalf("CompleteConnection() error = %v", err)
		}
	})
	return c
}

func TestConnectionLifecyclePersists(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedConnectorTenant(t, ctx, pool)
	c := createActiveConnection(t, ctx, pool, f)

	stored, err := store.GetConnection(ctx, pool, f.tenant, c.ID)
	if err != nil || stored.State != lifecycle.ConnectionActive || stored.Account.Account != "octocat" || stored.Account.Permissions[0] != "issues:write" || stored.Version != 2 {
		t.Fatalf("GetConnection() = %+v, %v", stored, err)
	}
	stranger := f.transition(2)
	stranger.Actor = lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: mustID[ids.Principal](t)}
	stranger.AuditID = newAuditID(t)
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (lifecycle.Connection, error) {
		return store.RemoveConnection(ctx, uow, f.tenant, c.ID, stranger)
	}); !rejectedTransition(err, lifecycle.RejectUnauthorizedActor) {
		t.Fatalf("RemoveConnection() by a stranger error = %v", err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		tr := f.transition(2)
		tr.AuditID = newAuditID(t)
		removing, err := store.RemoveConnection(ctx, uow, f.tenant, c.ID, tr)
		if err != nil || removing.State != lifecycle.ConnectionRemoving {
			t.Fatalf("RemoveConnection() = %+v, %v", removing, err)
		}
		tr = f.transition(3)
		tr.AuditID = newAuditID(t)
		removed, err := store.FinishConnectionRemoval(ctx, uow, f.tenant, c.ID, tr)
		if err != nil || removed.State != lifecycle.ConnectionRemoved {
			t.Fatalf("FinishConnectionRemoval() = %+v, %v", removed, err)
		}
	})
	list, err := store.ListConnections(ctx, pool, f.tenant)
	if err != nil || len(list) != 1 || list[0].State != lifecycle.ConnectionRemoved {
		t.Fatalf("ListConnections() = %+v, %v", list, err)
	}
	var audited int
	// The stranger's refusal rolled back with its transaction.
	if err := pool.QueryRow(ctx, `select count(*) from audit_event where tenant_id = $1 and record_kind = 'connection'`, f.tenant.String()).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("connection audit rows = %d, %v; want completion, removal and its end", audited, err)
	}
}

func TestConnectionColumnsRefuseWhatTheRecordForbids(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedConnectorTenant(t, ctx, pool)
	c := createActiveConnection(t, ctx, pool, f)
	for name, stmt := range map[string]string{
		"active without account": `update connection set account = null, permissions = null where connection_id = $1`,
		"account without list":   `update connection set permissions = null where connection_id = $1`,
		"two line account":       `update connection set account = E'a\nb' where connection_id = $1`,
		"unknown state":          `update connection set state = 'expired' where connection_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, stmt, c.ID.String()); err == nil {
				t.Fatal("database accepted it")
			}
		})
	}
	other := mustID[ids.Provider](t)
	bad, _ := lifecycle.NewConnection(mustID[ids.Connection](t), f.tenant, other, f.principal, "Unregistered", time.Now().UTC())
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		return true, store.InsertConnection(ctx, uow, bad)
	}); err == nil {
		t.Fatal("connection to a provider without a connector registration was stored")
	}
	runtimeConnection, _ := lifecycle.NewConnection(mustID[ids.Connection](t), f.tenant, f.runtime, f.principal, "Runtime", time.Now().UTC())
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		return true, store.InsertConnection(ctx, uow, runtimeConnection)
	}); err == nil {
		t.Fatal("connection to a runtime registration was stored")
	}
}

func TestConnectionGrantsListByWorkAndRevoke(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedConnectorTenant(t, ctx, pool)
	c := createActiveConnection(t, ctx, pool, f)
	agent := createAgent(t, ctx, pool, f.agentFixture, "Triager")
	at := time.Now().UTC().Truncate(time.Microsecond)
	task := createTaskInTx(t, ctx, pool, f.tenantFixture, at)
	agentGrant, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, f.principal, lifecycle.ConnectionGrantTerms{
		Agent: agent.ID, Tools: []string{"get_issue"}, Permission: identity.PermissionAlways, CallLimit: 5, ExpiresAt: at.Add(time.Hour),
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	taskGrant, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, f.principal, lifecycle.ConnectionGrantTerms{
		Task: task.ID, Tools: []string{"create_comment", "get_issue"}, Permission: identity.PermissionAsk, CallLimit: 1, ExpiresAt: at.Add(time.Hour),
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if _, err := store.GetConnectionForShare(ctx, uow, f.tenant, c.ID); err != nil {
			t.Fatal(err)
		}
		for _, g := range []lifecycle.ConnectionGrant{agentGrant, taskGrant} {
			if err := store.InsertConnectionGrant(ctx, uow, g); err != nil {
				t.Fatalf("InsertConnectionGrant() error = %v", err)
			}
		}
	})
	work, err := store.ListWorkGrants(ctx, pool, f.tenant, task.ID, agent.ID)
	if err != nil || len(work) != 2 {
		t.Fatalf("ListWorkGrants(task, agent) = %+v, %v", work, err)
	}
	if work, err := store.ListWorkGrants(ctx, pool, f.tenant, task.ID, ids.AgentID{}); err != nil || len(work) != 1 || work[0].ID != taskGrant.ID {
		t.Fatalf("ListWorkGrants(task) = %+v, %v", work, err)
	}
	stored, err := store.GetConnectionGrant(ctx, pool, f.tenant, taskGrant.ID)
	if err != nil || len(stored.Tools) != 2 || stored.Permission != identity.PermissionAsk || stored.Task != task.ID || !stored.Agent.IsZero() {
		t.Fatalf("GetConnectionGrant() = %+v, %v", stored, err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		tr := f.transition(1)
		tr.AuditID = newAuditID(t)
		revoked, err := store.RevokeConnectionGrant(ctx, uow, f.tenant, agentGrant.ID, tr)
		if err != nil || revoked.State != lifecycle.ConnectionGrantRevoked {
			t.Fatalf("RevokeConnectionGrant() = %+v, %v", revoked, err)
		}
	})
	if work, err := store.ListWorkGrants(ctx, pool, f.tenant, task.ID, agent.ID); err != nil || len(work) != 1 {
		t.Fatalf("ListWorkGrants() after revoking = %+v, %v", work, err)
	}
	if all, err := store.ListConnectionGrants(ctx, pool, f.tenant, c.ID); err != nil || len(all) != 2 {
		t.Fatalf("ListConnectionGrants() = %+v, %v", all, err)
	}
	if _, err := pool.Exec(ctx, `update connection_grant set task_id = $2 where grant_id = $1`, agentGrant.ID.String(), task.ID.String()); err == nil {
		t.Fatal("database accepted a grant naming both an agent and a task")
	}
}

func TestConnectorCallsKeepTheirExactBytes(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedConnectorTenant(t, ctx, pool)
	c := createActiveConnection(t, ctx, pool, f)
	at := time.Now().UTC().Truncate(time.Microsecond)
	task := createTaskInTx(t, ctx, pool, f.tenantFixture, at)
	var attempt lifecycle.Attempt
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) { attempt = createAttempt(t, ctx, uow, f.tenantFixture, task, 1, at) })
	grant, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, f.principal, lifecycle.ConnectionGrantTerms{
		Task: task.ID, Tools: []string{"get_issue"}, Permission: identity.PermissionAsk, CallLimit: 2, ExpiresAt: at.Add(time.Hour),
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.InsertConnectionGrant(ctx, uow, grant); err != nil {
			t.Fatal(err)
		}
	})
	// Key order and escaping that jsonb would rewrite.
	input, err := contracts.CanonicalJSON(json.RawMessage(`{"z": 1, "a": "<b>", "number": 137}`))
	if err != nil {
		t.Fatal(err)
	}
	call := store.ConnectorCall{
		Tenant: f.tenant, Operation: mustID[ids.Operation](t), Attempt: attempt.ID, Task: task.ID, Connection: c.ID,
		ConnectionGrant: grant.ID, Provider: f.connector, Tool: "get_issue", InputDigest: identity.DigestOf(input),
		State: store.CallAwaitingApproval, CreatedAt: at,
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.InsertConnectorCall(ctx, uow, call); err != nil {
			t.Fatalf("InsertConnectorCall() error = %v", err)
		}
	})
	if found, err := store.FindConnectorCall(ctx, pool, f.tenant, attempt.ID, c.ID, "get_issue", identity.DigestOf(input)); err != nil || found.Operation != call.Operation || found.State != store.CallAwaitingApproval {
		t.Fatalf("FindConnectorCall() = %+v, %v", found, err)
	}
	execution := contracts.ConnectorExecution{
		Request: identity.ActionRequest{Tenant: f.tenant, Attempt: attempt.ID, Provider: f.connector, Operation: call.Operation,
			Action: lifecycle.ConnectorAction("get_issue"), Scope: contracts.ConnectionScope(c.ID), InputDigest: identity.DigestOf(input)},
		Grant: mustID[ids.Grant](t), Decision: mustID[ids.Decision](t), Source: identity.SourceOneTimeApproval,
		ConnectionGrant: grant.ID, Connection: c.ID, Tool: "get_issue", Input: input,
	}
	for name, mutate := range map[string]func(*contracts.ConnectorExecution){
		"operation": func(e *contracts.ConnectorExecution) { e.Request.Operation = mustID[ids.Operation](t) },
		"attempt":   func(e *contracts.ConnectorExecution) { e.Request.Attempt = mustID[ids.Attempt](t) },
		"input": func(e *contracts.ConnectorExecution) {
			e.Input = json.RawMessage(`{"number":9}`)
			e.Request.InputDigest = identity.DigestOf(e.Input)
		},
		"connection grant": func(e *contracts.ConnectorExecution) { e.ConnectionGrant = mustID[ids.ConnGrant](t) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := execution
			mutate(&bad)
			if _, err := tryTx(t, ctx, pool, func(u *store.UnitOfWork) (bool, error) {
				return true, store.DecideConnectorCall(ctx, u, f.tenant, call.Operation, &bad, at)
			}); !errors.Is(err, store.ErrPayloadMismatch) {
				t.Fatalf("mismatched execution: %v", err)
			}
		})
	}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.DecideConnectorCall(ctx, uow, f.tenant, call.Operation, &execution, at); err != nil {
			t.Fatalf("DecideConnectorCall() error = %v", err)
		}
	})
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		return true, store.DecideConnectorCall(ctx, uow, f.tenant, call.Operation, nil, at)
	}); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("second DecideConnectorCall() error = %v", err)
	}
	output, err := contracts.CanonicalJSON(json.RawMessage(`{"title":"Govern connectors","labels":["a","b"],"html":"<p>"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := contracts.ConnectorResult{Operation: call.Operation, Output: output, OutputDigest: identity.DigestOf(output), CompletedAt: at}
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if replayed, err := store.RecordConnectorResult(ctx, uow, f.tenant, result); err != nil || replayed {
			t.Fatalf("RecordConnectorResult() = %v, %v", replayed, err)
		}
		if replayed, err := store.RecordConnectorResult(ctx, uow, f.tenant, result); err != nil || !replayed {
			t.Fatalf("replayed RecordConnectorResult() = %v, %v", replayed, err)
		}
		other := json.RawMessage(`{"title":"other"}`)
		if _, err := store.RecordConnectorResult(ctx, uow, f.tenant, contracts.ConnectorResult{Operation: call.Operation, Output: other, OutputDigest: identity.DigestOf(other), CompletedAt: at}); !errors.Is(err, store.ErrPayloadMismatch) {
			t.Fatalf("different RecordConnectorResult() error = %v", err)
		}
	})
	stored, err := store.GetConnectorCall(ctx, pool, f.tenant, call.Operation)
	if err != nil || stored.State != store.CallAuthorized || stored.Execution == nil || stored.Result == nil {
		t.Fatalf("GetConnectorCall() = %+v, %v", stored, err)
	}
	if string(stored.Execution.Input) != string(input) || string(stored.Result.Output) != string(output) {
		t.Fatalf("bytes changed: input %s, output %s", stored.Execution.Input, stored.Result.Output)
	}
	if err := stored.Result.Validate(); err != nil {
		t.Fatalf("stored result no longer matches its digest: %v", err)
	}
	if n, err := store.CountAuthorizedCalls(ctx, pool, f.tenant, task.ID, grant.ID); err != nil || n != 1 {
		t.Fatalf("CountAuthorizedCalls() = %d, %v", n, err)
	}
}

// TestConnectorsAreUnavailableOnSchema17 is the rollout state ADR-0011
// requires a new binary to serve.
func TestConnectorsAreUnavailableOnSchema17(t *testing.T) {
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
	if _, err := provider.DownTo(ctx, 17); err != nil {
		t.Fatalf("DownTo(17) error = %v", err)
	}
	pool := openPool(t, ctx, databaseURL)
	f := seedAgentTenant(t, ctx, pool)
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		return true, store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{Tenant: f.tenant, Provider: mustID[ids.Provider](t), Role: contracts.RoleConnector, UpdatedAt: time.Now().UTC()})
	}); err == nil {
		t.Fatal("schema 17 registered a connector")
	}
	c, _ := lifecycle.NewConnection(mustID[ids.Connection](t), f.tenant, f.runtime, f.principal, "Team GitHub", time.Now().UTC())
	if _, err := tryTx(t, ctx, pool, func(uow *store.UnitOfWork) (bool, error) {
		return true, store.InsertConnection(ctx, uow, c)
	}); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Fatalf("InsertConnection() on schema 17 error = %v, want ErrSchemaUnavailable", err)
	}
	if _, err := store.GetConnection(ctx, pool, f.tenant, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetConnection() on schema 17 error = %v, want ErrNotFound", err)
	}
	if list, err := store.ListConnections(ctx, pool, f.tenant); err != nil || len(list) != 0 {
		t.Fatalf("ListConnections() on schema 17 = %v, %v", list, err)
	}
	if work, err := store.ListWorkGrants(ctx, pool, f.tenant, mustID[ids.Task](t), ids.AgentID{}); err != nil || len(work) != 0 {
		t.Fatalf("ListWorkGrants() on schema 17 = %v, %v", work, err)
	}
	if available, err := store.ConnectorSchemaAvailable(ctx, pool); err != nil || available {
		t.Fatalf("ConnectorSchemaAvailable() on schema 17 = %v, %v", available, err)
	}
}

func TestConnectorsRefuseDowngrade(t *testing.T) {
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
	seedConnectorTenant(t, ctx, pool)
	if _, err := provider.DownTo(ctx, 17); err == nil || !strings.Contains(err.Error(), "cannot downgrade while version 18 connectors exist") {
		t.Fatalf("DownTo(17) with a connector registered error = %v, want the refusal", err)
	}
}

func rejectedTransition(err error, code lifecycle.RejectionCode) bool {
	var refusal *lifecycle.TransitionError
	return errors.As(err, &refusal) && refusal.Code == code
}
