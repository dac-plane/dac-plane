//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // Register the pgx stdlib driver for database/sql migration setup.

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func migrate(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
}

func openPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

type tenantFixture struct {
	tenant    ids.TenantID
	principal ids.PrincipalID
	provider  ids.ProviderID
	actor     lifecycle.Actor
}

func seedTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) tenantFixture {
	t.Helper()
	f := tenantFixture{tenant: mustID[ids.Tenant](t), principal: mustID[ids.Principal](t), provider: mustID[ids.Provider](t)}
	f.actor = lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal}
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if err := store.InsertTenant(ctx, uow, f.tenant); err != nil {
		t.Fatalf("InsertTenant() error = %v", err)
	}
	if err := store.InsertPrincipal(ctx, uow, identity.Principal{Tenant: f.tenant, ID: f.principal, Kind: identity.PrincipalHuman}); err != nil {
		t.Fatalf("InsertPrincipal() error = %v", err)
	}
	if err := store.InsertPolicy(ctx, uow, identity.Policy{
		Tenant: f.tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
		Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
	}); err != nil {
		t.Fatalf("InsertPolicy() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	return f
}

func newOutbox() *store.OutboxPayload {
	id, err := ids.New[ids.Any]()
	if err != nil {
		panic(err)
	}
	return &store.OutboxPayload{ID: id, Body: []byte(`{"dispatch":"test"}`)}
}

func newAuditID(t *testing.T) ids.AuditID {
	t.Helper()
	return mustID[ids.Audit](t)
}

func createTask(t *testing.T, ctx context.Context, uow *store.UnitOfWork, f tenantFixture, at time.Time) lifecycle.Task {
	t.Helper()
	task, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, at)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	stored, replayed, err := store.InsertTask(ctx, uow, task)
	if err != nil || replayed {
		t.Fatalf("InsertTask() = %v, %v, %v", stored, replayed, err)
	}
	return stored
}

func createAttempt(t *testing.T, ctx context.Context, uow *store.UnitOfWork, f tenantFixture, task lifecycle.Task, number uint32, at time.Time) lifecycle.Attempt {
	t.Helper()
	attempt, err := lifecycle.NewAttempt(mustID[ids.Attempt](t), f.tenant, task.ID, number, at)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	stored, replayed, err := store.InsertAttempt(ctx, uow, attempt)
	if err != nil || replayed {
		t.Fatalf("InsertAttempt() = %v, %v, %v", stored, replayed, err)
	}
	return stored
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, where string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from `+table+` where `+where, args...).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestPersistenceAtomicCommitAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() {
		if uow != nil {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, now)
	tr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	submitted, replayed, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskSubmit, "", tr)
	if err != nil || replayed || submitted.State != lifecycle.TaskQueued {
		t.Fatalf("ApplyTask() = %v, %v, %v", submitted, replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := countRows(t, ctx, pool, "audit_event", "tenant_id = $1", f.tenant.String()); got != 1 {
		t.Fatalf("audit rows = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, "durable_event", "tenant_id = $1", f.tenant.String()); got != 1 {
		t.Fatalf("durable rows = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, "outbox", "tenant_id = $1", f.tenant.String()); got != 1 {
		t.Fatalf("outbox rows = %d, want 1", got)
	}

	abandoned, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() {
		if abandoned != nil {
			_ = abandoned.Rollback(ctx)
		}
	}()
	other, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, now)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	if _, _, err := store.InsertTask(ctx, abandoned, other); err != nil {
		t.Fatalf("InsertTask() error = %v", err)
	}
	if err := abandoned.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if _, err := store.GetTask(ctx, pool, f.tenant, other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask() after rollback = %v, want not found", err)
	}
}

func TestPersistenceStaleReplayAndMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()
	at := func(tr store.Transition) store.Transition { return tr }

	commit := func(uow *store.UnitOfWork) {
		t.Helper()
		if err := uow.Commit(ctx); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
	}
	uow, _ := store.Begin(ctx, pool)
	defer func() {
		if uow != nil {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, now)
	base := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	for _, step := range []struct {
		event  lifecycle.TaskEvent
		target lifecycle.TaskState
		want   lifecycle.TaskState
	}{
		{lifecycle.TaskSubmit, "", lifecycle.TaskQueued},
		{lifecycle.TaskDispatch, "", lifecycle.TaskRunning},
		{lifecycle.TaskAwaitInput, "", lifecycle.TaskWaitingInput},
	} {
		tr := at(base)
		tr.ExpectedVersion = task.Version
		tr.AuditID, tr.Outbox = newAuditID(t), newOutbox()
		var err error
		task, _, err = store.ApplyTask(ctx, uow, f.tenant, task.ID, step.event, step.target, tr)
		if err != nil {
			t.Fatalf("ApplyTask(%s) error = %v", step.event, err)
		}
	}
	// Unresolved input fans out; commit one target, then retry the same
	// command (replay) and a different target (mismatch).
	tr := at(base)
	tr.ExpectedVersion, tr.AuditID, tr.Outbox = task.Version, newAuditID(t), newOutbox()
	failed, replayed, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskInputUnresolved, lifecycle.TaskFailed, tr)
	if err != nil || replayed || failed.State != lifecycle.TaskFailed {
		t.Fatalf("ApplyTask(unresolved->failed) = %v, %v, %v", failed, replayed, err)
	}
	retry := at(base)
	retry.ExpectedVersion, retry.AuditID = task.Version, newAuditID(t)
	if _, replayed, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskInputUnresolved, lifecycle.TaskFailed, retry); err != nil || !replayed {
		t.Fatalf("replay ApplyTask() = %v, %v, want replayed", replayed, err)
	}
	clash := at(base)
	clash.ExpectedVersion, clash.AuditID = task.Version, newAuditID(t)
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskInputUnresolved, lifecycle.TaskRunning, clash); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("clashing target error = %v, want payload mismatch", err)
	}
	stale := at(base)
	stale.ExpectedVersion, stale.AuditID = 1, newAuditID(t)
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskCancel, "", stale); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("stale ApplyTask() error = %v, want stale version", err)
	}
	commit(uow)
}

func TestPersistenceTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	other := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() {
		if uow != nil {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, now)
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := store.GetTask(ctx, pool, other.tenant, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant GetTask() = %v, want not found", err)
	}
	ro, _ := store.Begin(ctx, pool)
	defer func() { _ = ro.Rollback(ctx) }()
	tr := store.Transition{ExpectedVersion: 1, At: now, Actor: other.actor, AuditID: newAuditID(t)}
	if _, _, err := store.ApplyTask(ctx, ro, other.tenant, task.ID, lifecycle.TaskDispatch, "", tr); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant ApplyTask() = %v, want not found", err)
	}
}

func TestPersistenceInsertReplayAndMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task, err := lifecycle.NewTask(mustID[ids.Task](t), f.tenant, f.principal, now)
	if err != nil {
		t.Fatalf("NewTask() error = %v", err)
	}
	if _, replayed, err := store.InsertTask(ctx, uow, task); err != nil || replayed {
		t.Fatalf("first InsertTask() = %v, %v", replayed, err)
	}
	if _, replayed, err := store.InsertTask(ctx, uow, task); err != nil || !replayed {
		t.Fatalf("replay InsertTask() = %v, %v, want replayed", replayed, err)
	}
	changed := task
	changed.Owner = mustID[ids.Principal](t)
	if _, _, err := store.InsertTask(ctx, uow, changed); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("changed-owner InsertTask() = %v, want payload mismatch", err)
	}
}

func TestPersistenceRefusalAuditsWithoutOutbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	submit := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskSubmit, "", submit); err != nil {
		t.Fatalf("ApplyTask(submit) error = %v", err)
	}
	tr := store.Transition{ExpectedVersion: 2, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	// Accept is illegal from queued: the refusal must audit without dispatch.
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskAccept, "", tr); err == nil {
		t.Fatal("illegal TaskAccept succeeded")
	} else if lifecycle.RejectionOf(err) == "" {
		t.Fatalf("illegal TaskAccept error = %v, want a transition rejection", err)
	}
	// The row is unchanged and only refusal evidence exists.
	stored, err := store.GetTask(ctx, uow, f.tenant, task.ID)
	if err != nil || stored.State != lifecycle.TaskQueued || stored.Version != 2 {
		t.Fatalf("GetTask() = %v, %v", stored, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := countRows(t, ctx, pool, "audit_event", "tenant_id = $1 and outcome = 'rejected'", f.tenant.String()); got != 1 {
		t.Fatalf("rejected audits = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, "outbox", "tenant_id = $1 and event = 'accept'", f.tenant.String()); got != 0 {
		t.Fatalf("outbox rows for the refused accept = %d, want 0", got)
	}
}

func approvalFixture(t *testing.T, ctx context.Context, uow *store.UnitOfWork, f tenantFixture, at time.Time) (lifecycle.AttentionRequest, lifecycle.Attempt) {
	t.Helper()
	task := createTask(t, ctx, uow, f, at)
	attempt := createAttempt(t, ctx, uow, f, task, 1, at)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, at)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	request, err := lifecycle.NewAttentionRequest(lifecycle.AttentionRequest{
		ID:               mustID[ids.Attention](t),
		Tenant:           f.tenant,
		Task:             task.ID,
		Attempt:          attempt.ID,
		Kind:             lifecycle.AttentionActionApproval,
		Provider:         f.provider,
		Operation:        operation.ID,
		ActionDigest:     identity.DigestOf([]byte(`{"op":"write"}`)),
		PolicyGeneration: 1,
		Responders:       []ids.PrincipalID{f.principal},
		ExpiresAt:        at.Add(time.Hour),
	}, at)
	if err != nil {
		t.Fatalf("NewAttentionRequest() error = %v", err)
	}
	stored, replayed, err := store.InsertAttention(ctx, uow, request)
	if err != nil || replayed {
		t.Fatalf("InsertAttention() = %v, %v, %v", stored, replayed, err)
	}
	return stored, attempt
}

func TestPersistenceDecideAttention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	request, _ := approvalFixture(t, ctx, uow, f, now)
	dec := lifecycle.Decision{
		ID:             mustID[ids.Decision](t),
		Tenant:         f.tenant,
		Request:        request.ID,
		RequestVersion: 1,
		Principal:      f.principal,
		Kind:           lifecycle.DecisionApproveOnce,
		DecidedAt:      now,
	}
	tr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	decided, recorded, replayed, err := store.DecideAttention(ctx, uow, dec, 1, tr)
	if err != nil || replayed || decided.State != lifecycle.AttentionDecided {
		t.Fatalf("DecideAttention() = %v, %v, %v, %v", decided, recorded, replayed, err)
	}
	retry := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t)}
	if _, _, replayed, err := store.DecideAttention(ctx, uow, dec, 1, retry); err != nil || !replayed {
		t.Fatalf("replay DecideAttention() = %v, %v, want replayed", replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := countRows(t, ctx, pool, "decision", "tenant_id = $1", f.tenant.String()); got != 1 {
		t.Fatalf("decision rows = %d, want 1", got)
	}
}

func TestPersistenceBudgetSpend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)

	budget, err := lifecycle.NewBudget(mustID[ids.Budget](t), f.tenant, task.ID, "tokens", 100, now)
	if err != nil {
		t.Fatalf("NewBudget() error = %v", err)
	}
	if _, _, err := store.InsertBudget(ctx, uow, budget); err != nil {
		t.Fatalf("InsertBudget() error = %v", err)
	}
	tr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	reserved, reservation, replayed, err := store.ReserveBudget(ctx, uow, store.ReservationRequest{Tenant: f.tenant, Budget: budget.ID, Reservation: mustID[ids.Reservation](t), Attempt: attempt.ID, Amount: 60}, tr)
	if err != nil || replayed || reserved.Reserved != 60 {
		t.Fatalf("ReserveBudget() = %v, %v, %v, %v", reserved, reservation, replayed, err)
	}
	usage := store.Transition{ExpectedVersion: 2, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	consumed, held, replayed, err := store.ReportUsage(ctx, uow, f.tenant, budget.ID, reservation.ID, 20, usage)
	if err != nil || replayed || consumed.Consumed != 20 {
		t.Fatalf("ReportUsage() = %v, %v, %v, %v", consumed, held, replayed, err)
	}
	// A reordered report below the high-water mark changes nothing.
	again := store.Transition{ExpectedVersion: 3, At: now, Actor: f.actor, AuditID: newAuditID(t)}
	if _, _, replayed, err := store.ReportUsage(ctx, uow, f.tenant, budget.ID, reservation.ID, 10, again); err != nil || !replayed {
		t.Fatalf("reordered ReportUsage() = %v, %v, want replayed", replayed, err)
	}

	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	terms := lifecycle.SpendTerms{
		Tenant: f.tenant, Principal: f.principal, Task: task.ID, Attempt: attempt.ID,
		Operation: operation.ID, Amount: 5, Unit: "tokens", Nonce: identity.DigestOf([]byte("nonce-1")),
	}
	challenge, err := lifecycle.BeginSpend(mustID[ids.Spend](t), terms, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("BeginSpend() error = %v", err)
	}
	if _, _, err := store.InsertSpendChallenge(ctx, uow, challenge); err != nil {
		t.Fatalf("InsertSpendChallenge() error = %v", err)
	}
	assertion := lifecycle.SpendAssertion{Challenge: challenge.ID, Terms: terms, Proof: identity.DigestOf([]byte("proof-1")), AssertedAt: now}
	assertTr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	asserted, receipt, replayed, err := store.AssertSpendChallenge(ctx, uow, f.tenant, challenge.ID, assertion, 1, assertTr)
	if err != nil || replayed || asserted.State != lifecycle.SpendAsserted {
		t.Fatalf("AssertSpendChallenge() = %v, %v, %v, %v", asserted, receipt, replayed, err)
	}
	if _, _, replayed, err := store.AssertSpendChallenge(ctx, uow, f.tenant, challenge.ID, assertion, 1, assertTr); err != nil || !replayed {
		t.Fatalf("replay AssertSpendChallenge() = %v, %v, want replayed", replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceAuthorityLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}

	// Approval confers a one-time grant; authorizing retires it in the same
	// transaction as the allowing use.
	policy, err := store.GetPolicy(ctx, uow, f.tenant)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	grant := identity.AuthorityGrant{
		ID: mustID[ids.Grant](t), Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider,
		Action: "message.send", Mode: identity.GrantAllowOnce, Operation: operation.ID,
		InputDigest: identity.DigestOf([]byte(`{"to":"ops"}`)), GrantedBy: f.principal,
		PolicyGeneration: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := identity.IssueGrant(grant, policy); err != nil {
		t.Fatalf("IssueGrant() error = %v", err)
	}
	if err := store.InsertGrant(ctx, uow, grant); err != nil {
		t.Fatalf("InsertGrant() error = %v", err)
	}
	request := identity.ActionRequest{
		Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider, Operation: operation.ID,
		Action: "message.send", Scope: identity.Scope{Class: identity.ScopeGlobal},
		InputDigest: identity.DigestOf([]byte(`{"to":"ops"}`)),
	}
	use, retired, err := identity.Authorize(request, &grant, policy, now)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if use.Outcome != identity.UseAllowed || retired == nil {
		t.Fatalf("Authorize() = %v, retired %v", use, retired)
	}
	if _, _, err := store.InsertUse(ctx, uow, use); err != nil {
		t.Fatalf("InsertUse() error = %v", err)
	}
	if _, _, err := store.RetireGrant(ctx, uow, f.tenant, grant.ID, now); err != nil {
		t.Fatalf("RetireGrant() error = %v", err)
	}
	// The retired grant authorizes nothing further.
	if reuse, _, err := identity.Authorize(request, retired, policy, now); err != nil {
		t.Fatalf("Authorize() reuse error = %v", err)
	} else if reuse.Outcome != identity.UseDenied || reuse.Reason != identity.DenialRetired {
		t.Fatalf("Authorize() reuse = %v, want denied retired", reuse)
	}
	// A policy change fences the old generation both purely and in storage.
	if _, err := store.UpdatePolicyGeneration(ctx, uow, store.PolicyUpdate{
		Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 2,
		Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
		Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
		At:       now,
	}); err != nil {
		t.Fatalf("UpdatePolicyGeneration() error = %v", err)
	}
	current, err := store.GetPolicy(ctx, uow, f.tenant)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	if stale, _, err := identity.Authorize(request, &grant, current, now); err != nil {
		t.Fatalf("Authorize() stale error = %v", err)
	} else if stale.Reason != identity.DenialStaleGeneration {
		t.Fatalf("Authorize() stale = %v, want stale_generation", stale)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceClaimFencing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	claim, err := lifecycle.NewMaintenanceClaim(mustID[ids.Claim](t), f.tenant, f.provider, "worker-a", now)
	if err != nil {
		t.Fatalf("NewMaintenanceClaim() error = %v", err)
	}
	if _, _, err := store.InsertClaim(ctx, uow, claim); err != nil {
		t.Fatalf("InsertClaim() error = %v", err)
	}
	take := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	held, err := store.TakeClaim(ctx, uow, f.tenant, claim.ID, "worker-a", time.Minute, take)
	if err != nil || held.State != lifecycle.ClaimClaimed || held.Epoch != 1 {
		t.Fatalf("TakeClaim() = %v, %v", held, err)
	}
	// A second holder is refused while the lease runs.
	clash := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t)}
	if _, err := store.TakeClaim(ctx, uow, f.tenant, claim.ID, "worker-b", time.Minute, clash); lifecycle.RejectionOf(err) != lifecycle.RejectClaimHeld {
		t.Fatalf("takeover TakeClaim() = %v, want claim_held", err)
	}
	// After expiry the takeover fences the previous holder.
	later := store.Transition{At: now.Add(2 * time.Minute), Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	taken, err := store.TakeClaim(ctx, uow, f.tenant, claim.ID, "worker-b", time.Minute, later)
	if err != nil || taken.Epoch != 2 || taken.Claimant != "worker-b" {
		t.Fatalf("takeover TakeClaim() = %v, %v", taken, err)
	}
	// A stale epoch can no longer release.
	staleRelease := func(c lifecycle.MaintenanceClaim) (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
		return c.Release("worker-a", 1, now.Add(3*time.Minute))
	}
	staleTr := store.Transition{At: now.Add(3 * time.Minute), Actor: f.actor, AuditID: newAuditID(t)}
	if _, err := store.HoldClaim(ctx, uow, f.tenant, claim.ID, staleRelease, staleTr); lifecycle.RejectionOf(err) != lifecycle.RejectStaleClaim {
		t.Fatalf("stale HoldClaim() = %v, want stale_claim", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceResumeReaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	claim, err := lifecycle.NewMaintenanceClaim(mustID[ids.Claim](t), f.tenant, f.provider, "worker-a", now)
	if err != nil {
		t.Fatalf("NewMaintenanceClaim() error = %v", err)
	}
	if _, _, err := store.InsertClaim(ctx, uow, claim); err != nil {
		t.Fatalf("InsertClaim() error = %v", err)
	}
	take := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	if _, err := store.TakeClaim(ctx, uow, f.tenant, claim.ID, "worker-a", time.Minute, take); err != nil {
		t.Fatalf("TakeClaim() error = %v", err)
	}

	cursor := contracts.Cursor{Tenant: f.tenant, Attempt: attempt.ID, Operation: operation.ID, Stream: "transcript", Generation: 1}
	if _, err := store.UpsertCursor(ctx, uow, cursor); err != nil {
		t.Fatalf("UpsertCursor() error = %v", err)
	}
	cursor.After = 41
	if _, err := store.UpsertCursor(ctx, uow, cursor); err != nil {
		t.Fatalf("UpsertCursor() advance error = %v", err)
	}
	// Drive the operation to unknown durability for the resume probe.
	opTr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	if _, _, err := store.ApplyOperation(ctx, uow, f.tenant, operation.ID, lifecycle.OperationDispatch, "", opTr); err != nil {
		t.Fatalf("ApplyOperation(dispatch) error = %v", err)
	}
	unknown := store.Transition{ExpectedVersion: 2, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	uncertain, _, err := store.ApplyOperation(ctx, uow, f.tenant, operation.ID, lifecycle.OperationSendUnknown, "", unknown)
	if err != nil || uncertain.State != lifecycle.OperationUncertain {
		t.Fatalf("ApplyOperation(send_unknown) = %v, %v", uncertain, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	cursors, err := store.ListCursors(ctx, pool, f.tenant, attempt.ID)
	if err != nil || len(cursors) != 1 || cursors[0].After != 41 {
		t.Fatalf("ListCursors() = %v, %v", cursors, err)
	}
	uncertainOps, err := store.UncertainOperations(ctx, pool, f.tenant)
	if err != nil || len(uncertainOps) != 1 {
		t.Fatalf("UncertainOperations() = %v, %v", uncertainOps, err)
	}
	open, err := store.OpenClaims(ctx, pool, f.tenant)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenClaims() = %v, %v", open, err)
	}
	pending, err := store.OpenOutboxEntries(ctx, pool, f.tenant, 0)
	if err != nil || len(pending) == 0 {
		t.Fatalf("OpenOutboxEntries() = %v, %v", pending, err)
	}
}

func TestPersistenceArtifactsAndReview(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	manifest := lifecycle.ArtifactManifest{
		Artifact: mustID[ids.Artifact](t), Tenant: f.tenant, Task: task.ID, Attempt: attempt.ID,
		Operation: operation.ID, Producer: f.provider, Kind: lifecycle.ArtifactCodeChange,
		Digest: identity.DigestOf([]byte("bytes")), Size: 5, MediaType: "text/plain", WriterEpoch: 1,
	}
	if _, _, err := store.InsertManifest(ctx, uow, manifest); err != nil {
		t.Fatalf("InsertManifest() error = %v", err)
	}
	artifact := lifecycle.Artifact{ArtifactManifest: manifest, CommittedAt: now}
	if _, _, err := store.InsertArtifact(ctx, uow, artifact); err != nil {
		t.Fatalf("InsertArtifact() error = %v", err)
	}
	revision, err := lifecycle.NewResultRevision(mustID[ids.Revision](t), attempt.ID, []lifecycle.Artifact{artifact}, nil, now)
	if err != nil {
		t.Fatalf("NewResultRevision() error = %v", err)
	}
	if _, _, err := store.InsertRevision(ctx, uow, revision); err != nil {
		t.Fatalf("InsertRevision() error = %v", err)
	}
	// Drive the task to review, then accept it through a recorded review.
	drive := []lifecycle.TaskEvent{lifecycle.TaskSubmit, lifecycle.TaskDispatch, lifecycle.TaskProposeResult, lifecycle.TaskVerificationPassed}
	for _, event := range drive {
		current, err := store.GetTask(ctx, uow, f.tenant, task.ID)
		if err != nil {
			t.Fatalf("GetTask() error = %v", err)
		}
		tr := store.Transition{ExpectedVersion: current.Version, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
		if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, event, "", tr); err != nil {
			t.Fatalf("ApplyTask(%s) error = %v", event, err)
		}
	}
	rev := lifecycle.ResultReview{
		ID: mustID[ids.Review](t), Tenant: f.tenant, Task: task.ID, Revision: revision.ID,
		Reviewer: f.principal, Disposition: lifecycle.DispositionAccept,
		TaskVersion: 5, ReviewedAt: now,
	}
	tr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	accepted, recorded, replayed, err := store.RecordReview(ctx, uow, revision, rev, []ids.PrincipalID{f.principal}, tr)
	if err != nil || replayed || accepted.State != lifecycle.TaskSucceeded {
		t.Fatalf("RecordReview() = %v, %v, %v, %v", accepted, recorded, replayed, err)
	}
	// A concurrent review for the same version loses exactly once.
	rival := rev
	rival.ID = mustID[ids.Review](t)
	rival.Disposition = lifecycle.DispositionReject
	rivalTr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	if _, _, _, err := store.RecordReview(ctx, uow, revision, rival, []ids.PrincipalID{f.principal}, rivalTr); err == nil {
		t.Fatal("rival RecordReview() succeeded")
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}
