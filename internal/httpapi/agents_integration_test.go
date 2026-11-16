//go:build integration

package httpapi_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/platform/migrations"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/dac-plane/dac-plane/internal/simulator"
	"github.com/dac-plane/dac-plane/internal/testsupport/wiretest"
	"github.com/dac-plane/dac-plane/internal/worker"
)

func (s *surface) agentBody(name string) httpapi.AgentRequest {
	return httpapi.AgentRequest{
		Name: name, Role: "Reviews changes for correctness", Instructions: "Read the whole change first.",
		Provider: s.provider, EnvironmentProvider: s.environmentProvider,
	}
}

// registerRuntime registers the surface's harness provider in the runtime
// role, which an agent's default runtime provider must have.
func (s *surface) registerRuntime(t *testing.T) {
	t.Helper()
	s.seed(t, func(u *store.UnitOfWork) error {
		return store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{
			Tenant: s.tenant, Provider: s.provider, Role: contracts.RoleRuntime, UpdatedAt: s.at,
		})
	})
}

func profileOf(r httpapi.AgentRequest) lifecycle.AgentProfile {
	return lifecycle.AgentProfile{Name: r.Name, Role: r.Role, Instructions: r.Instructions, Provider: r.Provider, EnvironmentProvider: r.EnvironmentProvider}
}

func problemType(t *testing.T, r reply, status int) string {
	t.Helper()
	problem := decodeInto[httpapi.Problem](t, r, status)
	return strings.TrimPrefix(problem.Type, "https://dac-plane.dev/problems/")
}

func TestSurfaceAgentsThroughHTTP(t *testing.T) {
	s := newSurface(t)
	s.registerRuntime(t)
	body := s.agentBody("Reviewer")

	// Create, replay, and reuse the key with another body.
	created := s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-1", body: body})
	agent := decodeInto[lifecycle.Agent](t, created, 201)
	if agent.Revision != 1 || agent.Version != 1 || agent.State != lifecycle.AgentActive || agent.CreatedBy != s.owner || agent.AgentProfile != profileOf(body) {
		t.Fatalf("created agent = %+v", agent)
	}
	if created.header.Get("ETag") != `"1"` || created.header.Get("Location") != "/v1/tenants/"+s.tenant.String()+"/agents/"+agent.ID.String() {
		t.Fatalf("headers = %v", created.header)
	}
	if replayed := decodeInto[lifecycle.Agent](t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-1", body: body}), 201); replayed.ID != agent.ID {
		t.Fatalf("replay created %s, want %s", replayed.ID, agent.ID)
	}
	if got := problemType(t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-1", body: s.agentBody("Other")}), 422); got != "idempotency-key-reused" {
		t.Fatalf("reused key = %s", got)
	}

	// The name is taken while the agent is active; providers must be registered.
	if got := problemType(t, s.do(t, call{method: "POST", path: "/agents", as: "other", key: "agent-2", body: body}), 409); got != "agent-name-taken" {
		t.Fatalf("duplicate name = %s", got)
	}
	unregistered := s.agentBody("Unregistered")
	unregistered.Provider = mustID[ids.Provider](t)
	if got := problemType(t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-3", body: unregistered}), 422); got != "no-runtime-provider" {
		t.Fatalf("unregistered runtime = %s", got)
	}
	unregistered = s.agentBody("Unregistered")
	unregistered.EnvironmentProvider = mustID[ids.Provider](t)
	if got := problemType(t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-4", body: unregistered}), 422); got != "no-environment-provider" {
		t.Fatalf("unregistered environment = %s", got)
	}

	// Every principal of the tenant reads the tenant's agents.
	listed := decodeInto[httpapi.AgentListBody](t, s.do(t, call{method: "GET", path: "/agents", as: "other"}), 200)
	if len(listed.Agents) != 1 || listed.Agents[0].ID != agent.ID {
		t.Fatalf("listed = %+v", listed)
	}
	read := s.do(t, call{method: "GET", path: "/agents/" + agent.ID.String(), as: "other"})
	if decodeInto[lifecycle.Agent](t, read, 200).ID != agent.ID || read.header.Get("ETag") != `"1"` {
		t.Fatalf("read = %d %s", read.status, read.body)
	}
	if got := problemType(t, s.do(t, call{method: "GET", path: "/agents/" + mustID[ids.Agent](t).String(), as: "owner"}), 404); got != "not-found" {
		t.Fatalf("missing agent = %s", got)
	}

	// Only the creator revises, at the version it read.
	revised := s.agentBody("Reviewer")
	revised.Instructions = "Read the whole change first, then the tests."
	path := "/agents/" + agent.ID.String()
	if got := problemType(t, s.do(t, call{method: "PUT", path: path, as: "owner", key: "revise-1", body: revised}), 428); got != "precondition-required" {
		t.Fatalf("revise without If-Match = %s", got)
	}
	if got := problemType(t, s.do(t, call{method: "PUT", path: path, as: "other", key: "revise-1", ifMatch: `"1"`, body: revised}), 403); got != "forbidden" {
		t.Fatalf("revise by another principal = %s", got)
	}
	second := s.do(t, call{method: "PUT", path: path, as: "owner", key: "revise-1", ifMatch: `"1"`, body: revised})
	agent = decodeInto[lifecycle.Agent](t, second, 200)
	if agent.Revision != 2 || agent.Version != 2 || agent.Instructions != revised.Instructions || second.header.Get("ETag") != `"2"` {
		t.Fatalf("revised agent = %+v, ETag %s", agent, second.header.Get("ETag"))
	}
	stale := s.agentBody("Reviewer")
	stale.Role = "Reviews changes"
	if got := problemType(t, s.do(t, call{method: "PUT", path: path, as: "owner", key: "revise-2", ifMatch: `"1"`, body: stale}), 412); got != "stale-version" {
		t.Fatalf("stale revise = %s", got)
	}
	// The same edit under a new key, from the version it already moved past, is
	// stale too: only its own key replays it.
	if got := problemType(t, s.do(t, call{method: "PUT", path: path, as: "owner", key: "revise-again", ifMatch: `"1"`, body: revised}), 412); got != "stale-version" {
		t.Fatalf("repeated revise under a new key = %s", got)
	}

	// Earlier revisions stay readable.
	first := decodeInto[lifecycle.AgentRevision](t, s.do(t, call{method: "GET", path: path + "/revisions/1", as: "other"}), 200)
	if first.Number != 1 || first.AgentProfile != profileOf(body) {
		t.Fatalf("revision 1 = %+v", first)
	}
	for _, missing := range []string{"3", "0", "01", "x"} {
		if got := problemType(t, s.do(t, call{method: "GET", path: path + "/revisions/" + missing, as: "owner"}), 404); got != "not-found" {
			t.Fatalf("revision %s = %s", missing, got)
		}
	}

	// A task delegated to the agent pins its current revision and takes its
	// providers; another principal may delegate to it too.
	submit := s.submitBody()
	submit.Agent, submit.Provider, submit.EnvironmentProvider = agent.ID, ids.ProviderID{}, ids.ProviderID{}
	submission := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "other", key: "delegate-1", body: submit}), 201)
	if submission.Task.Agent == nil || *submission.Task.Agent != (lifecycle.AgentReference{Agent: agent.ID, Revision: 2}) {
		t.Fatalf("delegated task agent = %+v", submission.Task.Agent)
	}
	if submission.Provider != s.provider || submission.EnvironmentProvider == nil || *submission.EnvironmentProvider != s.environmentProvider {
		t.Fatalf("delegated providers = %s, %v", submission.Provider, submission.EnvironmentProvider)
	}
	view := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: "/tasks/" + submission.Task.ID.String(), as: "other"}), 200)
	if view.Task.Agent == nil || *view.Task.Agent != *submission.Task.Agent {
		t.Fatalf("task view agent = %+v", view.Task.Agent)
	}
	// Every audit entry of the delegated task names the agent revision; a task
	// submitted to providers names none.
	plain := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "plain-1", body: s.submitBody()}), 201)
	for task, want := range map[ids.TaskID]*lifecycle.AgentReference{submission.Task.ID: submission.Task.Agent, plain.Task.ID: nil} {
		var total, withAgent, other int
		if err := s.pool.QueryRow(s.ctx, `
			select count(*),
			       count(*) filter (where agent_id = $3::uuid and agent_revision = $4),
			       count(*) filter (where agent_id is not null and agent_id is distinct from $3::uuid)
			from audit_event where tenant_id = $1 and record_kind = 'task' and record_id = $2`,
			s.tenant.String(), task.String(), agentText(want), agentRevision(want)).Scan(&total, &withAgent, &other); err != nil {
			t.Fatal(err)
		}
		if total == 0 || other != 0 || (want != nil && withAgent != total) || (want == nil && withAgent != 0) {
			t.Fatalf("task %s audit: %d entries, %d with the agent, %d with another, want agent %+v", task, total, withAgent, other, want)
		}
	}

	// A runtime that negotiated agent instructions receives the pinned
	// revision's role and instructions beside the brief.
	rt := s.dispatch(t, submission.Operation.ID)
	if sent := rt.Ledger().Deliveries(submission.Operation.ID); sent != 1 {
		t.Fatalf("delegated start_turn delivered %d times, want 1", sent)
	}
	var start contracts.RuntimeStart
	for _, request := range rt.Ledger().Sent() {
		if request.Operation == submission.Operation.ID {
			if err := json.Unmarshal(request.Arguments, &start); err != nil {
				t.Fatal(err)
			}
		}
	}
	if start.Agent == nil || *start.Agent != (contracts.AgentInstructions{Role: revised.Role, Instructions: revised.Instructions}) || start.Brief.Objective != submit.Objective {
		t.Fatalf("delegated start_turn = %+v", start)
	}
	if started := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: "/tasks/" + submission.Task.ID.String(), as: "other"}), 200); started.Task.State == lifecycle.TaskFailed {
		t.Fatalf("delegated task failed after its start was delivered")
	}

	// A runtime that did not negotiate them is never asked to start a
	// delegated turn: the task fails visibly instead of running without them.
	unsupported := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "other", key: "delegate-old-kit", body: submit}), 201)
	rt = s.dispatchWith(t, unsupported.Operation.ID, simulator.DefaultHello())
	if sent := rt.Ledger().Deliveries(unsupported.Operation.ID); sent != 0 {
		t.Fatalf("delegated start_turn delivered %d times to a runtime without agent instructions", sent)
	}
	if failed := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: "/tasks/" + unsupported.Task.ID.String(), as: "other"}), 200); failed.Task.State != lifecycle.TaskFailed {
		t.Fatalf("delegated task state = %s, want failed", failed.Task.State)
	}
	dispatched, err := store.GetOutboxRow(s.ctx, s.pool, s.tenant, unsupported.Operation.ID.Untyped())
	if err != nil || dispatched.LastError != "agent_instructions_unsupported" {
		t.Fatalf("delegated dispatch = %+v, %v", dispatched, err)
	}
	// The same runtime still starts a task submitted to providers.
	other := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "plain-old-kit", body: s.submitBody()}), 201)
	if rt = s.dispatchWith(t, other.Operation.ID, simulator.DefaultHello()); rt.Ledger().Deliveries(other.Operation.ID) != 1 {
		t.Fatalf("undelegated start_turn delivered %d times, want 1", rt.Ledger().Deliveries(other.Operation.ID))
	}

	if got := problemType(t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "delegate-2", body: func() httpapi.SubmitTaskRequest {
		missing := submit
		missing.Agent = mustID[ids.Agent](t)
		return missing
	}()}), 422); got != "no-agent" {
		t.Fatalf("delegation to a missing agent = %s", got)
	}

	// Retiring stops delegation, keeps what was delegated and frees the name.
	if got := problemType(t, s.do(t, call{method: "POST", path: path + "/retire", as: "other", key: "retire-1", ifMatch: `"2"`, body: struct{}{}}), 403); got != "forbidden" {
		t.Fatalf("retire by another principal = %s", got)
	}
	retired := decodeInto[lifecycle.Agent](t, s.do(t, call{method: "POST", path: path + "/retire", as: "owner", key: "retire-1", ifMatch: `"2"`, body: struct{}{}}), 200)
	if retired.State != lifecycle.AgentRetired || retired.Version != 3 || retired.Revision != 2 {
		t.Fatalf("retired agent = %+v", retired)
	}
	if got := problemType(t, s.do(t, call{method: "POST", path: path + "/retire", as: "owner", key: "retire-again", ifMatch: `"2"`, body: struct{}{}}), 412); got != "stale-version" {
		t.Fatalf("repeated retire under a new key = %s", got)
	}
	var refusals int
	if err := s.pool.QueryRow(s.ctx, `select count(*) from audit_event where tenant_id = $1 and record_kind = 'agent' and record_id = $2 and outcome = 'rejected' and reason = 'version_conflict'`,
		s.tenant.String(), agent.ID.String()).Scan(&refusals); err != nil {
		t.Fatal(err)
	}
	if refusals < 3 {
		t.Fatalf("stale agent commands audited %d times, want at least 3", refusals)
	}
	if got := problemType(t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "delegate-3", body: submit}), 422); got != "no-agent" {
		t.Fatalf("delegation to a retired agent = %s", got)
	}
	if replay := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "other", key: "delegate-1", body: submit}), 201); replay.Task.ID != submission.Task.ID {
		t.Fatalf("replay after retirement created %s", replay.Task.ID)
	}
	if got := problemType(t, s.do(t, call{method: "PUT", path: path, as: "owner", key: "revise-3", ifMatch: `"3"`, body: stale}), 409); got != "refused" {
		t.Fatalf("revise a retired agent = %s", got)
	}
	decodeInto[lifecycle.Agent](t, s.do(t, call{method: "POST", path: "/agents", as: "other", key: "agent-5", body: body}), 201)
}

// TestSurfaceAgentsBeforeTheMigration serves the API on the schema before
// agents, as a binary rolled ahead of its migration does (ADR-0011): agent
// writes and delegation are unavailable, and everything else works.
func TestSurfaceAgentsBeforeTheMigration(t *testing.T) {
	s := newSurface(t)
	s.registerRuntime(t)
	db, err := sql.Open("pgx", s.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	migrator, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.DownTo(s.ctx, 16); err != nil {
		t.Fatal(err)
	}

	if got := problemType(t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "agent-1", body: s.agentBody("Reviewer")}), 503); got != "not-migrated" {
		t.Fatalf("create before the migration = %s", got)
	}
	if listed := decodeInto[httpapi.AgentListBody](t, s.do(t, call{method: "GET", path: "/agents", as: "owner"}), 200); len(listed.Agents) != 0 {
		t.Fatalf("listed = %+v", listed)
	}
	submit := s.submitBody()
	submit.Agent = mustID[ids.Agent](t)
	if got := problemType(t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "delegate-1", body: submit}), 503); got != "not-migrated" {
		t.Fatalf("delegation before the migration = %s", got)
	}
	decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-1", body: s.submitBody()}), 201)
}

func agentText(reference *lifecycle.AgentReference) any {
	if reference == nil {
		return nil
	}
	return reference.Agent.String()
}

func agentRevision(reference *lifecycle.AgentReference) any {
	if reference == nil {
		return nil
	}
	return int64(reference.Revision)
}

// TestSurfaceDelegatedTakeoverReplaysOnlyOverAnAgentConnection takes over a
// delegated start_turn whose send marker survived its worker. The runtime has
// one connection that negotiated only agent instructions and one that
// negotiated only replay: no connection can replay the delegated turn, so the
// takeover leaves it to reconciliation instead of releasing it forever.
func TestSurfaceDelegatedTakeoverReplaysOnlyOverAnAgentConnection(t *testing.T) {
	s := newSurface(t)
	s.registerRuntime(t)
	agent := decodeInto[lifecycle.Agent](t, s.do(t, call{method: "POST", path: "/agents", as: "owner", key: "takeover-agent", body: s.agentBody("Takeover")}), 201)
	submit := s.submitBody()
	submit.Agent, submit.Provider, submit.EnvironmentProvider = agent.ID, ids.ProviderID{}, ids.ProviderID{}
	submission := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "takeover-task", body: submit}), 201)
	s.dispatchPendingEnvironments(t)
	// An earlier claim committed its send marker and then lost its worker.
	if _, err := s.pool.Exec(s.ctx, `update outbox set last_error='send_started' where tenant_id=$1 and outbox_id=$2`, s.tenant.String(), submission.Operation.ID.String()); err != nil {
		t.Fatal(err)
	}

	hello := func(offered ...contracts.Capability) contracts.Lifecycle {
		h := simulator.DefaultHello()
		h.Offered = offered
		return h
	}
	agentOnly, err := simulator.NewRuntime(simulator.Config{Tenant: s.tenant, Provider: s.provider, Hello: hello(contracts.CapabilityAgentInstructions)}, simulator.RuntimeScenario{})
	if err != nil {
		t.Fatal(err)
	}
	replayOnly, err := simulator.NewRuntime(simulator.Config{Tenant: s.tenant, Provider: s.provider, Hello: hello(contracts.CapabilityOperationReplay)}, simulator.RuntimeScenario{})
	if err != nil {
		t.Fatal(err)
	}
	wire := wiretest.For(t, s.pool)
	wire.Serve(t, s.tenant, s.provider, agentOnly)
	wire.Connect(t, wire.Bind(t, s.tenant, s.provider, contracts.RoleRuntime, nil), s.provider, replayOnly)
	deadline := time.Now().Add(10 * time.Second)
	for wire.Registry.Connections(s.tenant, s.provider, contracts.RoleRuntime) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("second runtime connection never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "takeover-worker", worker.WithPorts(wire.Registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}

	if sent := agentOnly.Ledger().Deliveries(submission.Operation.ID) + replayOnly.Ledger().Deliveries(submission.Operation.ID); sent != 0 {
		t.Fatalf("takeover delivered the delegated start %d times", sent)
	}
	operation, err := store.GetOperation(s.ctx, s.pool, s.tenant, submission.Operation.ID)
	if err != nil || operation.State != lifecycle.OperationUncertain {
		t.Fatalf("operation = %s, %v; want uncertain, for reconciliation", operation.State, err)
	}
	row, err := store.GetOutboxRow(s.ctx, s.pool, s.tenant, submission.Operation.ID.Untyped())
	if err != nil || row.Status != "done" {
		t.Fatalf("outbox row = %+v, %v; want done", row, err)
	}
}
