//go:build integration

package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/provision"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/oidctest"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

const audience = "https://dac-plane.example/api"

// surface is a migrated database, a provisioned tenant with an owner, an
// unrelated principal and a tenant reader, a live simulated provider, and the
// Surface API served over HTTP with real access tokens.
type surface struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	server      *httptest.Server
	issuer      *oidctest.Issuer
	key         any
	tenant      ids.TenantID
	owner       ids.PrincipalID
	provider    ids.ProviderID
	at          time.Time
	provisioner *provision.Provisioner
	reader      ids.PrincipalID
}

func newSurface(t *testing.T) *surface {
	t.Helper()
	return newSurfaceWith(t, httpapi.Options{PollInterval: 20 * time.Millisecond, Heartbeat: time.Second, StreamLifetime: 5 * time.Second})
}

func newSurfaceWith(t *testing.T, options httpapi.Options) *surface {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	s := &surface{ctx: ctx, pool: pool, provider: mustID[ids.Provider](t), at: time.Now().UTC().Truncate(time.Microsecond)}
	s.issuer = oidctest.New(t)
	s.issuer.Audience = audience
	key := oidctest.NewRSAKey(t)
	s.issuer.Publish("k1", key)
	s.key = key

	p, err := provision.New(pool, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.provisioner = p
	if s.tenant, err = p.CreateTenant(ctx, ids.TenantID{}); err != nil {
		t.Fatal(err)
	}
	for subject, reader := range map[string]bool{"owner": false, "other": false, "reader": true} {
		principal, err := p.CreatePrincipal(ctx, s.tenant, ids.PrincipalID{}, identity.PrincipalHuman)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.BindIdentity(ctx, s.tenant, principal, s.issuer.URL(), subject); err != nil {
			t.Fatal(err)
		}
		if reader {
			if err := p.SetTenantReader(ctx, s.tenant, principal, true); err != nil {
				t.Fatal(err)
			}
		}
		if subject == "owner" {
			s.owner = principal
		}
		if reader {
			s.reader = principal
		}
	}
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uow.Rollback(ctx) }()
	if _, err := store.UpsertHarness(ctx, uow, lifecycle.HarnessQualification{Tenant: s.tenant, Provider: s.provider, Mode: lifecycle.HarnessLive, Version: 1, UpdatedAt: s.at}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	config := authn.Config{Issuers: []authn.IssuerConfig{{Issuer: s.issuer.URL(), Audience: audience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 3600}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator, err := authn.New(ctx, config, authn.StoreResolver(pool), logger, authn.WithHTTPClient(s.issuer.Client()))
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(pool, authenticator, logger, options)
	if err != nil {
		t.Fatal(err)
	}
	s.server = httptest.NewServer(api)
	t.Cleanup(s.server.Close)
	return s
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *surface) token(t *testing.T, subject string) string {
	t.Helper()
	s.issuer.Subject = subject
	return s.issuer.Token(t, oidctest.Spec{Key: s.key})
}

type call struct {
	method, path, as, key, ifMatch, accept string
	body                                   any
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (s *surface) do(t *testing.T, c call) reply {
	t.Helper()
	var reader io.Reader
	if c.body != nil {
		data, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(s.ctx, c.method, s.server.URL+"/v1/tenants/"+s.tenant.String()+c.path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if c.as != "" {
		request.Header.Set("Authorization", "Bearer "+s.token(t, c.as))
	}
	if c.key != "" {
		request.Header.Set("Idempotency-Key", c.key)
	}
	if c.ifMatch != "" {
		request.Header.Set("If-Match", c.ifMatch)
	}
	if c.accept != "" {
		request.Header.Set("Accept", c.accept)
	}
	response, err := s.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: response.StatusCode, header: response.Header, body: body}
}

func decodeInto[T any](t *testing.T, r reply, status int) T {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	var value T
	if err := json.Unmarshal(r.body, &value); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return value
}

func (s *surface) submitBody() httpapi.SubmitTaskRequest {
	return httpapi.SubmitTaskRequest{Provider: s.provider, Demand: contracts.DemandPerson, Deadline: s.at.Add(time.Hour), Unit: "tokens", Ceiling: 1000, Amount: 100}
}

func (s *surface) seed(t *testing.T, apply func(*store.UnitOfWork) error) {
	t.Helper()
	u, err := store.Begin(s.ctx, s.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(s.ctx) }()
	if err := apply(u); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(s.ctx); err != nil {
		t.Fatal(err)
	}
}

// dispatch runs one worker pass with a runtime that accepts the operation.
func (s *surface) dispatch(t *testing.T, operation ids.OperationID) *simulator.Runtime {
	t.Helper()
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: s.tenant, Provider: s.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "surface-worker",
		wiretest.Ports(t, s.pool, s.tenant, map[ids.ProviderID]contracts.Port{s.provider: rt}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}
	return rt
}

func row(sequence uint64, kind string, payload any) simulator.Row {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return simulator.Row{Sequence: sequence, Updated: sequence, Kind: kind, Payload: string(data)}
}

// observe ingests provider rows for the submission, as the adapter side would.
func (s *surface) observe(t *testing.T, rt *simulator.Runtime, sub httpapi.SubmissionBody, rows ...simulator.Row) {
	t.Helper()
	event := rt.Event(sub.Attempt.ID, sub.Operation.ID, "events", 1, simulator.RowsFrame(false, rows...))
	if _, err := application.Ingest(s.ctx, s.pool, contracts.Peer{Tenant: s.tenant, Provider: s.provider}, event, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSurfaceLifecycleThroughHTTP(t *testing.T) {
	s := newSurface(t)

	// Submit, replay, and reuse a key with another body.
	created := s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-1", body: s.submitBody()})
	sub := decodeInto[httpapi.SubmissionBody](t, created, http.StatusCreated)
	if created.header.Get("Location") != "/v1/tenants/"+s.tenant.String()+"/tasks/"+sub.Task.ID.String() || created.header.Get("ETag") == "" {
		t.Fatalf("Location %q ETag %q", created.header.Get("Location"), created.header.Get("ETag"))
	}
	if again := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-1", body: s.submitBody()}), http.StatusCreated); again.Task.ID != sub.Task.ID {
		t.Fatalf("replay created task %s, want %s", again.Task.ID, sub.Task.ID)
	}
	changed := s.submitBody()
	changed.Amount = 101
	if r := s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-1", body: changed}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("reused key with another body: %d %s", r.status, r.body)
	}

	// Owner and tenant reader see the task; another principal and an unbound
	// identity do not, and cannot tell why.
	taskPath := "/tasks/" + sub.Task.ID.String()
	if view := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "owner"}), http.StatusOK); view.Task.ID != sub.Task.ID || view.Attempt == nil {
		t.Fatalf("owner view = %+v", view)
	}
	decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "reader"}), http.StatusOK)
	hidden := s.do(t, call{method: "GET", path: taskPath, as: "other"})
	missing := s.do(t, call{method: "GET", path: "/tasks/" + mustID[ids.Task](t).String(), as: "other"})
	if hidden.status != http.StatusNotFound || string(hidden.body) != string(missing.body) {
		t.Fatalf("unowned task %d %s, missing task %d %s", hidden.status, hidden.body, missing.status, missing.body)
	}
	if r := s.do(t, call{method: "GET", path: taskPath, as: "stranger"}); r.status != http.StatusUnauthorized {
		t.Fatalf("unbound identity: %d", r.status)
	}

	// The provider starts work and asks a question.
	rt := s.dispatch(t, sub.Operation.ID)
	s.observe(t, rt, sub,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryAttentionRequested, contracts.AttentionRequested{NativeRef: "q-1", Kind: lifecycle.AttentionQuestion, ExpiresAt: time.Now().Add(time.Hour)}),
	)
	list := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "owner"}), http.StatusOK)
	if len(list.Attention) != 1 {
		t.Fatalf("open attention = %d", len(list.Attention))
	}
	if other := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "other"}), http.StatusOK); len(other.Attention) != 0 {
		t.Fatal("another principal sees the owner's attention request")
	}
	attention := list.Attention[0]
	attentionPath := "/attention/" + attention.ID.String()
	read := s.do(t, call{method: "GET", path: attentionPath, as: "owner"})
	if read.status != http.StatusOK || read.header.Get("ETag") != `"`+strconv.FormatUint(attention.Version, 10)+`"` {
		t.Fatalf("attention read %d ETag %q", read.status, read.header.Get("ETag"))
	}

	// Decide: missing and stale preconditions, then success and replay.
	answer := httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: identity.DigestOf([]byte("yes"))}
	decisions := attentionPath + "/decisions"
	if r := s.do(t, call{method: "POST", path: decisions, as: "owner", key: "decide-1", body: answer}); r.status != http.StatusPreconditionRequired {
		t.Fatalf("decision without If-Match: %d %s", r.status, r.body)
	}
	if r := s.do(t, call{method: "POST", path: decisions, as: "owner", key: "decide-stale", ifMatch: `"` + strconv.FormatUint(attention.Version+5, 10) + `"`, body: answer}); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale decision: %d %s", r.status, r.body)
	}
	decided := decodeInto[httpapi.DecisionBody](t, s.do(t, call{method: "POST", path: decisions, as: "owner", key: "decide-1", ifMatch: read.header.Get("ETag"), body: answer}), http.StatusCreated)
	replayed := decodeInto[httpapi.DecisionBody](t, s.do(t, call{method: "POST", path: decisions, as: "owner", key: "decide-1", ifMatch: read.header.Get("ETag"), body: answer}), http.StatusCreated)
	if decided.Decision == "" || replayed.Decision != decided.Decision {
		t.Fatalf("decision %+v replay %+v", decided, replayed)
	}

	// Events: the task stream carries the lifecycle in order with record
	// versions, resumes after a cursor, and resets a foreign cursor.
	page := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: taskPath + "/events", as: "owner"}), http.StatusOK)
	if page.Reset || len(page.Events) < 3 {
		t.Fatalf("task page = %+v", page)
	}
	seenAttention := false
	for i, event := range page.Events {
		if event.Task == nil || *event.Task != sub.Task.ID || event.RecordVersion == 0 {
			t.Fatalf("event %d = %+v", i, event)
		}
		if i > 0 && !(cursorPosition(t, page.Events[i-1].Cursor) < cursorPosition(t, event.Cursor)) {
			t.Fatalf("events out of order: %s then %s", page.Events[i-1].Cursor, event.Cursor)
		}
		seenAttention = seenAttention || event.Record == lifecycle.RecordAttention
	}
	if !seenAttention {
		t.Fatal("attention request events missing from the task stream")
	}
	if page.NextCursor != page.Events[len(page.Events)-1].Cursor {
		t.Fatalf("next_cursor %s, last event %s", page.NextCursor, page.Events[len(page.Events)-1].Cursor)
	}
	middle := page.Events[1].Cursor
	resumed := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: taskPath + "/events?after=" + middle, as: "owner"}), http.StatusOK)
	if len(resumed.Events) != len(page.Events)-2 || resumed.Events[0].Cursor != page.Events[2].Cursor {
		t.Fatalf("resume after %s = %d events, want %d", middle, len(resumed.Events), len(page.Events)-2)
	}
	if reset := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: taskPath + "/events?after=9.1", as: "owner"}), http.StatusOK); !reset.Reset || reset.NextCursor != page.NextCursor {
		t.Fatalf("foreign-generation cursor = %+v, want reset to %s", reset, page.NextCursor)
	}
	if r := s.do(t, call{method: "GET", path: taskPath + "/events", as: "other"}); r.status != http.StatusNotFound {
		t.Fatalf("another principal's task stream: %d", r.status)
	}
	tenantForReader := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: "/events", as: "reader"}), http.StatusOK)
	tenantForOther := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: "/events", as: "other"}), http.StatusOK)
	if len(tenantForReader.Events) < len(page.Events) || len(tenantForOther.Events) != 0 {
		t.Fatalf("tenant stream: reader %d events, other %d events", len(tenantForReader.Events), len(tenantForOther.Events))
	}

	// Server-Sent Events: replay from a cursor, then a new event arrives live.
	s.assertStream(t, taskPath+"/events", middle, page.Events[2:], func() {
		task := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "owner"}), http.StatusOK)
		decodeInto[httpapi.CancelBody](t, s.do(t, call{method: "POST", path: taskPath + "/cancel", as: "owner", key: "cancel-1", ifMatch: `"` + strconv.FormatUint(task.Task.Version, 10) + `"`, body: httpapi.CancelRequest{}}), http.StatusAccepted)
	})
	if r := s.do(t, call{method: "POST", path: taskPath + "/cancel", as: "owner", key: "cancel-stale", ifMatch: `"1"`, body: httpapi.CancelRequest{}}); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale cancel: %d %s", r.status, r.body)
	}
}

func TestSurfaceMigrationAdministrationThroughHTTP(t *testing.T) {
	s := newSurface(t)
	target := mustID[ids.Provider](t)
	s.seed(t, func(u *store.UnitOfWork) error {
		if err := store.SetHarnessAdministrator(s.ctx, u, s.tenant, s.owner, true); err != nil {
			return err
		}
		_, err := store.UpsertHarness(s.ctx, u, lifecycle.HarnessQualification{
			Tenant: s.tenant, Provider: target, Mode: lifecycle.HarnessLive, Version: 1, UpdatedAt: s.at,
		})
		return err
	})

	createPath := "/migration-passes"
	createBody := httpapi.CreateMigrationRequest{From: s.provider, To: target}
	createdReply := s.do(t, call{method: "POST", path: createPath, as: "owner", key: "migration-create", body: createBody})
	created := decodeInto[httpapi.MigrationBody](t, createdReply, http.StatusCreated)
	passPath := createPath + "/" + created.Migration.ID.String()
	if createdReply.header.Get("Location") != "/v1/tenants/"+s.tenant.String()+passPath || createdReply.header.Get("ETag") != `"1"` {
		t.Fatalf("Location %q ETag %q", createdReply.header.Get("Location"), createdReply.header.Get("ETag"))
	}
	if created.Preview != nil || created.Migration.State != lifecycle.MigrationPlanned {
		t.Fatalf("created migration = %+v", created)
	}
	replayed := decodeInto[httpapi.MigrationBody](t, s.do(t, call{method: "POST", path: createPath, as: "owner", key: "migration-create", body: createBody}), http.StatusCreated)
	if replayed.Migration != created.Migration {
		t.Fatalf("create replay = %+v, want %+v", replayed, created)
	}
	changed := createBody
	changed.To = mustID[ids.Provider](t)
	if r := s.do(t, call{method: "POST", path: createPath, as: "owner", key: "migration-create", body: changed}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("changed create replay: %d %s", r.status, r.body)
	}

	read := s.do(t, call{method: "GET", path: passPath, as: "owner"})
	decodeInto[httpapi.MigrationBody](t, read, http.StatusOK)
	missingPath := createPath + "/" + mustID[ids.Migration](t).String()
	hidden := s.do(t, call{method: "GET", path: passPath, as: "other"})
	missing := s.do(t, call{method: "GET", path: missingPath, as: "other"})
	if hidden.status != http.StatusNotFound || string(hidden.body) != string(missing.body) {
		t.Fatalf("hidden pass %d %s, missing pass %d %s", hidden.status, hidden.body, missing.status, missing.body)
	}
	if r := s.do(t, call{method: "POST", path: passPath + "/preview", as: "other", key: "hidden-preview", ifMatch: read.header.Get("ETag"), body: httpapi.EmptyCommandRequest{}}); r.status != http.StatusNotFound {
		t.Fatalf("non-administrator preview: %d %s", r.status, r.body)
	}

	administer := func(action, key string, version uint64, status int) (httpapi.MigrationBody, reply) {
		t.Helper()
		r := s.do(t, call{method: "POST", path: passPath + "/" + action, as: "owner", key: key, ifMatch: `"` + strconv.FormatUint(version, 10) + `"`, body: httpapi.EmptyCommandRequest{}})
		view := decodeInto[httpapi.MigrationBody](t, r, status)
		if r.header.Get("ETag") != `"`+strconv.FormatUint(view.Migration.Version, 10)+`"` {
			t.Fatalf("%s ETag %q, version %d", action, r.header.Get("ETag"), view.Migration.Version)
		}
		return view, r
	}

	previewed, previewReply := administer("preview", "migration-preview", created.Migration.Version, http.StatusOK)
	if previewed.Preview == nil || previewed.Preview.FromVersion != 1 || previewed.Preview.ToVersion != 1 {
		t.Fatalf("preview = %+v", previewed)
	}
	if r := s.do(t, call{method: "POST", path: passPath + "/hold", as: "owner", key: "migration-stale", ifMatch: `"1"`, body: httpapi.EmptyCommandRequest{}}); r.status != http.StatusPreconditionFailed {
		t.Fatalf("stale migration command: %d %s", r.status, r.body)
	}
	held, _ := administer("hold", "migration-hold", previewed.Migration.Version, http.StatusOK)
	if held.Migration.State != lifecycle.MigrationHeld || held.Preview != nil {
		t.Fatalf("held migration = %+v", held)
	}
	released, _ := administer("release", "migration-release", held.Migration.Version, http.StatusOK)
	previewed, _ = administer("preview", "migration-preview-fresh", released.Migration.Version, http.StatusOK)
	executing, _ := administer("execute", "migration-execute", previewed.Migration.Version, http.StatusAccepted)
	if executing.Migration.State != lifecycle.MigrationExecuting || executing.Preview == nil {
		t.Fatalf("executing migration = %+v", executing)
	}

	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "surface-migration")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ReapClaims(s.ctx); err != nil {
		t.Fatal(err)
	}
	completed := decodeInto[httpapi.MigrationBody](t, s.do(t, call{method: "GET", path: passPath, as: "owner"}), http.StatusOK)
	if completed.Migration.State != lifecycle.MigrationCompleted {
		t.Fatalf("completed migration = %+v", completed)
	}

	secondReply := s.do(t, call{method: "POST", path: createPath, as: "owner", key: "migration-create-second", body: createBody})
	second := decodeInto[httpapi.MigrationBody](t, secondReply, http.StatusCreated)
	passPath = createPath + "/" + second.Migration.ID.String()
	cancelled, _ := administer("cancel", "migration-cancel", second.Migration.Version, http.StatusOK)
	if cancelled.Migration.State != lifecycle.MigrationCancelled {
		t.Fatalf("cancelled migration = %+v", cancelled)
	}

	s.seed(t, func(u *store.UnitOfWork) error {
		return store.SetHarnessAdministrator(s.ctx, u, s.tenant, s.owner, false)
	})
	if r := s.do(t, call{method: "GET", path: passPath, as: "owner"}); r.status != http.StatusNotFound {
		t.Fatalf("revoked administrator read: %d %s", r.status, r.body)
	}
	if r := s.do(t, call{method: "POST", path: createPath, as: "owner", key: "migration-create", body: createBody}); r.status != http.StatusForbidden {
		t.Fatalf("revoked administrator create replay: %d %s", r.status, r.body)
	}
	if r := s.do(t, call{method: "POST", path: createPath + "/" + created.Migration.ID.String() + "/preview", as: "owner", key: "migration-preview", ifMatch: previewReply.header.Get("ETag"), body: httpapi.EmptyCommandRequest{}}); r.status != http.StatusNotFound {
		t.Fatalf("revoked administrator command replay: %d %s", r.status, r.body)
	}
}

func TestSurfaceAuthorityRevocationThroughHTTP(t *testing.T) {
	s := newSurface(t)
	s.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(s.ctx, u, store.PolicyUpdate{
			Tenant: s.tenant, ExpectedGeneration: 1, NextGeneration: 1,
			Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways},
			Ceiling:  map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways},
			At:       s.at,
		})
		return err
	})

	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "authority-submit", body: s.submitBody()}), http.StatusCreated)
	rt := s.dispatch(t, sub.Operation.ID)
	input := json.RawMessage(`{"resource":"screen","value":"safe"}`)
	s.observe(t, rt, sub,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "authority-session"}),
		row(2, contracts.EntryAttentionRequested, contracts.AttentionRequested{
			NativeRef: "authority-approval", Kind: lifecycle.AttentionActionApproval,
			Action: "resource.mutate", ActionDigest: identity.DigestOf(input),
			Scope: identity.Scope{Class: identity.ScopeSubject, Target: "screen"}, ExpiresAt: s.at.Add(time.Hour),
		}),
	)
	attention := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "owner"}), http.StatusOK).Attention
	if len(attention) != 1 {
		t.Fatalf("action approvals = %d", len(attention))
	}
	requestPath := "/attention/" + attention[0].ID.String()
	decision := decodeInto[httpapi.DecisionBody](t, s.do(t, call{
		method: "POST", path: requestPath + "/decisions", as: "owner", key: "authority-approve",
		ifMatch: `"` + strconv.FormatUint(attention[0].Version, 10) + `"`,
		body:    httpapi.DecisionRequest{Kind: lifecycle.DecisionApproveStanding},
	}), http.StatusCreated)
	grant, err := ids.Parse[ids.Grant](decision.Grant)
	if err != nil {
		t.Fatalf("standing grant %q: %v", decision.Grant, err)
	}

	revokePath := "/authority-grants/" + grant.String() + "/revoke"
	revoked := s.do(t, call{method: "POST", path: revokePath, as: "owner", key: "authority-revoke", body: httpapi.EmptyCommandRequest{}})
	if revoked.status != http.StatusNoContent || len(revoked.body) != 0 || revoked.header.Get("Content-Type") != "" || revoked.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("revoke response = %d headers=%v body=%q", revoked.status, revoked.header, revoked.body)
	}
	replayed := s.do(t, call{method: "POST", path: revokePath, as: "owner", key: "authority-revoke", body: httpapi.EmptyCommandRequest{}})
	if replayed.status != http.StatusNoContent || len(replayed.body) != 0 {
		t.Fatalf("revoke replay = %d %q", replayed.status, replayed.body)
	}
	stored, err := store.GetGrant(s.ctx, s.pool, s.tenant, grant)
	if err != nil || stored.RevokedAt.IsZero() {
		t.Fatalf("stored grant = %+v, %v", stored, err)
	}
	var audits int
	if err := s.pool.QueryRow(s.ctx, `select count(*) from audit_event where tenant_id=$1 and record_kind='authority_grant' and record_id=$2 and event='revoke' and outcome='applied'`, s.tenant.String(), grant.String()).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("revocation audits = %d, %v", audits, err)
	}

	hidden := s.do(t, call{method: "POST", path: revokePath, as: "other", key: "authority-hidden", body: httpapi.EmptyCommandRequest{}})
	missing := s.do(t, call{method: "POST", path: "/authority-grants/" + mustID[ids.Grant](t).String() + "/revoke", as: "other", key: "authority-missing", body: httpapi.EmptyCommandRequest{}})
	if hidden.status != http.StatusNotFound || string(hidden.body) != string(missing.body) {
		t.Fatalf("hidden grant %d %s, missing grant %d %s", hidden.status, hidden.body, missing.status, missing.body)
	}
}

// assertStream opens an SSE stream after cursor, requires it to replay
// exactly want, then runs act and requires a further live event.
func (s *surface) assertStream(t *testing.T, path, after string, want []httpapi.EventBody, act func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", s.server.URL+"/v1/tenants/"+s.tenant.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token(t, "owner"))
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", after)
	response, err := s.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status %d type %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	var got []string
	for scanner.Scan() {
		id, ok := strings.CutPrefix(scanner.Text(), "id: ")
		if !ok {
			continue
		}
		got = append(got, id)
		if len(got) == len(want) {
			for i, event := range want {
				if got[i] != event.Cursor {
					t.Fatalf("replayed %v, want cursors of %v", got, want)
				}
			}
			act()
		}
		if len(got) > len(want) {
			if cursorPosition(t, id) <= cursorPosition(t, want[len(want)-1].Cursor) {
				t.Fatalf("live event %s not after replay", id)
			}
			return
		}
	}
	t.Fatalf("stream ended after %v without a live event (%v)", got, scanner.Err())
}

func cursorPosition(t *testing.T, cursor string) int64 {
	t.Helper()
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 {
		t.Fatalf("cursor %q", cursor)
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("cursor %q", cursor)
	}
	return n
}

// TestSurfaceResultReviewThroughHTTP drives a result to review: the provider
// proposes a revision, the artifact worker admits it, and the owner reads the
// attempt and revision and reviews it through the API.
func TestSurfaceResultReviewThroughHTTP(t *testing.T) {
	s := newSurface(t)
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-r", body: s.submitBody()}), http.StatusCreated)
	rt := s.dispatch(t, sub.Operation.ID)
	storeID := mustID[ids.Provider](t)
	bytesStore, err := simulator.NewByteArtifactStore(simulator.Config{Tenant: s.tenant, Provider: storeID})
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("exact patch bytes")
	rt.WithArtifactExport(bytesStore, map[string][]byte{"patch": patch})
	s.observe(t, rt, sub,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryResultProposed, contracts.ResultProposed{SessionRef: "s-1", TurnRef: "t-1", SourceRevision: "source-sha-1", Artifacts: []contracts.ArtifactDeclaration{
			{NativeRef: "patch", Kind: lifecycle.ArtifactCodeChange, Digest: identity.DigestOf(patch), Size: uint64(len(patch)), MediaType: "text/plain"},
		}}),
		row(3, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: time.Now().UTC()}),
	)
	cfg := worker.DefaultConfig()
	cfg.Policy.BaseBackoff, cfg.Policy.MaxBackoff = time.Millisecond, time.Millisecond
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "surface-artifacts",
		wiretest.Ports(t, s.pool, s.tenant, map[ids.ProviderID]contracts.Port{s.provider: rt},
			wiretest.ArtifactStores(map[ids.ProviderID]worker.ArtifactStorePort{s.provider: {Provider: storeID, Port: bytesStore}})))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}

	taskPath := "/tasks/" + sub.Task.ID.String()
	read := s.do(t, call{method: "GET", path: taskPath, as: "owner"})
	view := decodeInto[httpapi.TaskBody](t, read, http.StatusOK)
	if view.Task.State != lifecycle.TaskReadyForReview || len(view.Revisions) != 1 {
		t.Fatalf("task %s with %d revisions", view.Task.State, len(view.Revisions))
	}
	revision := view.Revisions[0].Revision
	attempt := s.do(t, call{method: "GET", path: taskPath + "/attempts/" + sub.Attempt.ID.String(), as: "owner"})
	if loaded := decodeInto[lifecycle.Attempt](t, attempt, http.StatusOK); loaded.ID != sub.Attempt.ID || attempt.header.Get("ETag") == "" {
		t.Fatalf("attempt %+v ETag %q", loaded, attempt.header.Get("ETag"))
	}
	if r := s.do(t, call{method: "GET", path: "/tasks/" + mustID[ids.Task](t).String() + "/attempts/" + sub.Attempt.ID.String(), as: "owner"}); r.status != http.StatusNotFound {
		t.Fatalf("attempt under another task: %d", r.status)
	}
	revisionPath := taskPath + "/revisions/" + revision.ID.String()
	if got := decodeInto[httpapi.RevisionBody](t, s.do(t, call{method: "GET", path: revisionPath, as: "owner"}), http.StatusOK); got.Revision.ID != revision.ID || len(got.Reviews) != 0 {
		t.Fatalf("revision %+v", got)
	}

	accept := httpapi.ReviewRequest{Revision: revision.ID, Disposition: lifecycle.DispositionAccept}
	if r := s.do(t, call{method: "POST", path: taskPath + "/reviews", as: "other", key: "review-x", ifMatch: read.header.Get("ETag"), body: accept}); r.status != http.StatusNotFound {
		t.Fatalf("review by another principal: %d %s", r.status, r.body)
	}
	if r := s.do(t, call{method: "POST", path: taskPath + "/reviews", as: "reader", key: "review-y", ifMatch: read.header.Get("ETag"), body: accept}); r.status != http.StatusForbidden {
		t.Fatalf("review by a tenant reader who may see but not review: %d %s", r.status, r.body)
	}
	reviewed := decodeInto[httpapi.ReviewBody](t, s.do(t, call{method: "POST", path: taskPath + "/reviews", as: "owner", key: "review-1", ifMatch: read.header.Get("ETag"), body: accept}), http.StatusCreated)
	if reviewed.Review.Disposition != lifecycle.DispositionAccept || reviewed.Rework != nil {
		t.Fatalf("review = %+v", reviewed)
	}
	if after := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "owner"}), http.StatusOK); after.Task.State != lifecycle.TaskSucceeded || len(after.Revisions[0].Reviews) != 1 {
		t.Fatalf("after review: %s with %d reviews", after.Task.State, len(after.Revisions[0].Reviews))
	}
}

// TestSurfaceGovernedSubmissionThroughHTTP completes a spend-governed
// submission with a spend assertion and its If-Match precondition.
func TestSurfaceGovernedSubmissionThroughHTTP(t *testing.T) {
	s := newSurface(t)
	body := s.submitBody()
	body.Spend = true
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "governed-1", body: body}), http.StatusCreated)
	if sub.Challenge == nil || sub.Receipt != nil || sub.Dispatch != nil {
		t.Fatalf("governed submission = challenge %v receipt %v dispatch %v", sub.Challenge, sub.Receipt, sub.Dispatch)
	}
	assertion := httpapi.SpendAssertionRequest{SubmissionKey: "governed-1", Assertion: lifecycle.SpendAssertion{
		Challenge: sub.Challenge.ID, Terms: sub.Challenge.Terms, Proof: identity.DigestOf([]byte("ceremony proof")), AssertedAt: time.Now().UTC(),
	}}
	path := "/tasks/" + sub.Task.ID.String() + "/spend-assertions"
	if r := s.do(t, call{method: "POST", path: path, as: "owner", key: "assert-1", body: assertion}); r.status != http.StatusPreconditionRequired {
		t.Fatalf("assertion without If-Match: %d %s", r.status, r.body)
	}
	if r := s.do(t, call{method: "POST", path: "/tasks/" + mustID[ids.Task](t).String() + "/spend-assertions", as: "owner", key: "assert-x", ifMatch: `"1"`, body: assertion}); r.status != http.StatusNotFound {
		t.Fatalf("assertion under another task: %d %s", r.status, r.body)
	}
	asserted := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: path, as: "owner", key: "assert-1", ifMatch: `"` + strconv.FormatUint(sub.Challenge.Version, 10) + `"`, body: assertion}), http.StatusOK)
	if asserted.Receipt == nil || asserted.Dispatch == nil {
		t.Fatalf("asserted submission = receipt %v dispatch %v", asserted.Receipt, asserted.Dispatch)
	}
}

// TestSurfaceReaderRevocationAppliesToTheNextRequest revokes the tenant-reader
// permission between two requests.
func TestSurfaceReaderRevocationAppliesToTheNextRequest(t *testing.T) {
	s := newSurface(t)
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-rv", body: s.submitBody()}), http.StatusCreated)
	taskPath := "/tasks/" + sub.Task.ID.String()
	decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "reader"}), http.StatusOK)
	if err := s.provisioner.SetTenantReader(s.ctx, s.tenant, s.reader, false); err != nil {
		t.Fatal(err)
	}
	if r := s.do(t, call{method: "GET", path: taskPath, as: "reader"}); r.status != http.StatusNotFound {
		t.Fatalf("revoked reader: %d", r.status)
	}
	if page := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: "/events", as: "reader"}), http.StatusOK); len(page.Events) != 0 {
		t.Fatalf("revoked reader still sees %d tenant events", len(page.Events))
	}
}

// TestSurfaceForeignCursorsReset uses one stream's cursor on another: the
// stream must reset instead of resuming at a position it never had.
func TestSurfaceForeignCursorsReset(t *testing.T) {
	s := newSurface(t)
	first := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-a", body: s.submitBody()}), http.StatusCreated)
	second := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-b", body: s.submitBody()}), http.StatusCreated)
	firstStream := "/tasks/" + first.Task.ID.String() + "/events"
	secondStream := "/tasks/" + second.Task.ID.String() + "/events"
	firstPage := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: firstStream, as: "owner"}), http.StatusOK)
	secondPage := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: secondStream, as: "owner"}), http.StatusOK)
	tenantPage := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: "/events", as: "owner"}), http.StatusOK)
	if len(firstPage.Events) == 0 || len(secondPage.Events) == 0 {
		t.Fatal("tasks have no events")
	}
	for name, use := range map[string]struct{ path, cursor string }{
		"task cursor on another task":      {secondStream, firstPage.Events[0].Cursor},
		"tenant cursor on a task stream":   {secondStream, tenantPage.Events[0].Cursor},
		"task cursor on the tenant stream": {"/events", firstPage.Events[0].Cursor},
	} {
		t.Run(name, func(t *testing.T) {
			page := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: use.path + "?after=" + use.cursor, as: "owner"}), http.StatusOK)
			if !page.Reset || len(page.Events) != 0 {
				t.Fatalf("foreign cursor resumed: reset %v, %d events", page.Reset, len(page.Events))
			}
			if resumed := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: use.path + "?after=" + page.NextCursor, as: "owner"}), http.StatusOK); resumed.Reset {
				t.Fatalf("reset cursor %s is itself reset", page.NextCursor)
			}
		})
	}
}

// TestSurfaceStreamNeverRepeatsEventsAcrossHeartbeats sets the heartbeat
// below the poll interval, so heartbeats fire between polls, and requires
// every event id on the stream to be unique.
func TestSurfaceStreamNeverRepeatsEventsAcrossHeartbeats(t *testing.T) {
	s := newSurfaceWith(t, httpapi.Options{PollInterval: 300 * time.Millisecond, Heartbeat: 10 * time.Millisecond, StreamLifetime: 2 * time.Second})
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-h", body: s.submitBody()}), http.StatusCreated)
	path := "/tasks/" + sub.Task.ID.String() + "/events"
	want := decodeInto[httpapi.EventPageBody](t, s.do(t, call{method: "GET", path: path, as: "owner"}), http.StatusOK)
	request, err := http.NewRequestWithContext(s.ctx, "GET", s.server.URL+"/v1/tenants/"+s.tenant.String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token(t, "owner"))
	request.Header.Set("Accept", "text/event-stream")
	response, err := s.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	seen, heartbeats := map[string]bool{}, 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == ": heartbeat" {
			heartbeats++
		}
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			if seen[id] {
				t.Fatalf("event %s sent twice", id)
			}
			seen[id] = true
		}
	}
	if heartbeats < 3 || len(seen) != len(want.Events) {
		t.Fatalf("stream sent %d heartbeats and %d events, want >= 3 and %d", heartbeats, len(seen), len(want.Events))
	}
}

// TestSurfaceFollowsTheRuntimeStreamOverTheWire proves the deployed path for
// provider observations: the worker subscribes to the accepted turn's stream
// over the adapter wire and ingests what the runtime streams, with no direct
// Ingest. A second pass resumes from the durable cursor and applies nothing
// twice.
func TestSurfaceFollowsTheRuntimeStreamOverTheWire(t *testing.T) {
	s := newSurface(t)
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-o", body: s.submitBody()}), http.StatusCreated)
	stream := &simulator.StreamScript{Frames: []contracts.StreamFrame{
		simulator.ConnectedFrame(time.Now().UTC()),
		simulator.RowsFrame(false,
			row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
			row(2, contracts.EntryAttentionRequested, contracts.AttentionRequested{NativeRef: "q-1", Kind: lifecycle.AttentionQuestion, ExpiresAt: time.Now().Add(time.Hour)}),
		),
	}}
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: s.tenant, Provider: s.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{sub.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted, Stream: stream}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := worker.DefaultConfig()
	cfg.ObserveIdle = 500 * time.Millisecond
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "surface-observer",
		wiretest.Ports(t, s.pool, s.tenant, map[ids.ProviderID]contracts.Port{s.provider: rt}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}
	if list := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "owner"}), http.StatusOK); len(list.Attention) != 0 {
		t.Fatalf("attention before any observation = %d", len(list.Attention))
	}
	for pass := range 2 {
		if err := w.Observe(s.ctx); err != nil {
			t.Fatal(err)
		}
		list := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "owner"}), http.StatusOK)
		if len(list.Attention) != 1 {
			t.Fatalf("pass %d: open attention = %d, want the one the runtime streamed", pass, len(list.Attention))
		}
		cursor, err := store.GetCursor(s.ctx, s.pool, s.tenant, sub.Operation.ID, "events")
		if err != nil || cursor.After != 2 {
			t.Fatalf("pass %d: cursor = %+v, %v; want after 2", pass, cursor, err)
		}
	}
	if subscriptions := len(rt.Ledger().Subscriptions()); subscriptions != 2 {
		t.Fatalf("runtime saw %d subscriptions, want one per pass", subscriptions)
	}
}
