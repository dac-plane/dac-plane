//go:build integration

package application_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // Register the pgx stdlib driver for migration setup.

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// fixture is one tenant with one principal and one simulated provider. Only
// identity and harness qualification are seeded through the store: there is
// no M1 command surface for them. Every lifecycle fact is created and read
// through the command boundary, the dispatch loop or the simulator ledger.
type fixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	tenant    ids.TenantID
	principal ids.PrincipalID
	actor     identity.AuthorizationContext
	provider  ids.ProviderID
	at        time.Time
}

func newFixture(t *testing.T, mode lifecycle.HarnessMode) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	_ = db.Close()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)

	f := &fixture{
		ctx: ctx, pool: pool, tenant: mustID[ids.Tenant](t), principal: mustID[ids.Principal](t),
		provider: mustID[ids.Provider](t), at: time.Now().UTC().Truncate(time.Microsecond),
	}
	principal := identity.Principal{Tenant: f.tenant, ID: f.principal, Kind: identity.PrincipalHuman}
	if f.actor, err = identity.NewAuthorizationContext(principal); err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	f.seed(t, func(uow *store.UnitOfWork) error {
		if err := store.InsertTenant(ctx, uow, f.tenant); err != nil {
			return err
		}
		if err := store.InsertPrincipal(ctx, uow, principal); err != nil {
			return err
		}
		if err := store.InsertPolicy(ctx, uow, identity.Policy{Tenant: f.tenant, Generation: 1}); err != nil {
			return err
		}
		if mode == "" {
			return nil
		}
		_, err := store.UpsertHarness(ctx, uow, lifecycle.HarnessQualification{
			Tenant: f.tenant, Provider: f.provider, Mode: mode, Version: 1, UpdatedAt: f.at,
		})
		return err
	})
	return f
}

func (f *fixture) seed(t *testing.T, write func(*store.UnitOfWork) error) {
	t.Helper()
	uow, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := write(uow); err != nil {
		_ = uow.Rollback(f.ctx)
		t.Fatalf("seed: %v", err)
	}
	if err := uow.Commit(f.ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func (f *fixture) submitCommand(nonce string) application.SubmitTask {
	return application.SubmitTask{
		Command:  application.Command{Actor: f.actor, Nonce: nonce},
		At:       f.at,
		Provider: f.provider,
		Demand:   contracts.DemandPerson,
		Deadline: f.at.Add(time.Hour),
		Unit:     "tokens",
		Ceiling:  1000,
		Amount:   100,
	}
}

func (f *fixture) submit(t *testing.T, cmd application.SubmitTask) application.Submission {
	t.Helper()
	submission, err := application.Submit(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return submission
}

// runtime returns a simulated runtime that accepts the given operations.
func (f *fixture) runtime(t *testing.T, operations ...ids.OperationID) *simulator.Runtime {
	t.Helper()
	scenario := simulator.RuntimeScenario{Operations: map[ids.OperationID]simulator.RuntimeOp{}}
	for _, operation := range operations {
		scenario.Operations[operation] = simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}
	}
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, scenario)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return rt
}

// tick runs one dispatch pass with port behind the selected provider.
func (f *fixture) tick(t *testing.T, port contracts.Port) {
	t.Helper()
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		map[ids.ProviderID]contracts.Port{f.provider: port}, worker.DefaultConfig(), "test-worker")
	if err != nil {
		t.Fatalf("worker.New() error = %v", err)
	}
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
}

func (f *fixture) reload(t *testing.T, nonce string) application.Submission {
	t.Helper()
	submission, err := application.GetSubmission(f.ctx, f.pool, f.actor, nonce)
	if err != nil {
		t.Fatalf("GetSubmission() error = %v", err)
	}
	return submission
}

func requireRefusal(t *testing.T, err error, code lifecycle.RejectionCode) {
	t.Helper()
	var refusal *lifecycle.TransitionError
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("error = %v, want refusal %s", err, code)
	}
}

func TestSubmitTaskDispatchesTurnThroughSelectedHarness(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	switch {
	case s.Task.State != lifecycle.TaskQueued:
		t.Fatalf("task = %s, want queued", s.Task.State)
	case s.Attempt.State != lifecycle.AttemptAllocated:
		t.Fatalf("attempt = %s, want allocated", s.Attempt.State)
	case s.Budget.Reserved != 100 || s.Reservation.State != lifecycle.ReservationHeld || s.Reservation.Amount != 100:
		t.Fatalf("budget %+v reservation %+v, want 100 held", s.Budget, s.Reservation)
	case s.Operation.State != lifecycle.OperationDispatched:
		t.Fatalf("operation = %s, want dispatched", s.Operation.State)
	case s.Dispatch == nil || s.Dispatch.Status != "pending" || s.Dispatch.Demand != contracts.DemandPerson:
		t.Fatalf("dispatch = %+v, want pending with person demand", s.Dispatch)
	case s.Mode != lifecycle.HarnessLive || s.Provider != f.provider:
		t.Fatalf("selection = %s/%s, want live %s", s.Mode, s.Provider, f.provider)
	}

	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := rt.Ledger().Deliveries(s.Operation.ID); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	sent := rt.Ledger().Sent()
	if len(sent) != 1 || sent[0].Method != contracts.MethodStartTurn || sent[0].Demand != contracts.DemandPerson || sent[0].Attempt != s.Attempt.ID {
		t.Fatalf("sent = %+v, want one person start_turn for the attempt", sent)
	}
	after := f.reload(t, "n-1")
	if after.Operation.State != lifecycle.OperationAccepted || after.Dispatch.Status != "done" {
		t.Fatalf("after tick: operation %s dispatch %s, want accepted and done", after.Operation.State, after.Dispatch.Status)
	}
	if after.Reservation.State != lifecycle.ReservationHeld || after.Budget.Reserved != 100 {
		t.Fatalf("dispatch took a second hold: budget %+v", after.Budget)
	}
}

func TestSubmitTaskReplayReturnsOriginalSubmission(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	first := f.submit(t, f.submitCommand("n-1"))
	replay := f.submit(t, f.submitCommand("n-1"))
	if replay.Task.ID != first.Task.ID || replay.Attempt.ID != first.Attempt.ID || replay.Operation.ID != first.Operation.ID {
		t.Fatalf("replay created a new submission: %s vs %s", replay.Task.ID, first.Task.ID)
	}
	other := f.submit(t, f.submitCommand("n-2"))
	if other.Task.ID == first.Task.ID {
		t.Fatal("a different nonce returned the same task")
	}

	f.tick(t, f.runtime(t, first.Operation.ID, other.Operation.ID))
	later := f.submit(t, f.submitCommand("n-1"))
	if later.Task.ID != first.Task.ID || later.Operation.State != lifecycle.OperationAccepted {
		t.Fatalf("replay after dispatch = %s/%s, want the original, now accepted", later.Task.ID, later.Operation.State)
	}
}

func TestSubmitTaskConcurrentDuplicatesConverge(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	const racers = 4
	results := make([]application.Submission, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = application.Submit(f.ctx, f.pool, f.submitCommand("n-race"))
		}()
	}
	wg.Wait()
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if results[i].Task.ID != results[0].Task.ID {
			t.Fatalf("racers produced different tasks: %s and %s", results[i].Task.ID, results[0].Task.ID)
		}
	}
	rt := f.runtime(t, results[0].Operation.ID)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 1 {
		t.Fatalf("sent %d requests, want exactly 1", got)
	}
}

func TestSubmitTaskChangedPayloadRejected(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	original := f.submit(t, f.submitCommand("n-1"))
	changed := f.submitCommand("n-1")
	changed.Amount = 200
	if _, err := application.Submit(f.ctx, f.pool, changed); !errors.Is(err, application.ErrPayloadMismatch) {
		t.Fatalf("changed payload = %v, want ErrPayloadMismatch", err)
	}
	after := f.reload(t, "n-1")
	if after.Task.ID != original.Task.ID || after.Amount != 100 || after.Reservation.Amount != 100 {
		t.Fatalf("original changed: %+v", after)
	}
}

func TestSubmitTaskRejectsInvalidCommands(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	for name, mutate := range map[string]func(c *application.SubmitTask){
		"zero actor":       func(c *application.SubmitTask) { c.Actor = identity.AuthorizationContext{} },
		"empty nonce":      func(c *application.SubmitTask) { c.Nonce = "" },
		"expected version": func(c *application.SubmitTask) { c.ExpectedVersion = 3 },
		"deadline at time": func(c *application.SubmitTask) { c.Deadline = c.At },
		"zero amount":      func(c *application.SubmitTask) { c.Amount = 0 },
		"malformed unit":   func(c *application.SubmitTask) { c.Unit = "Tokens" },
		"unknown demand":   func(c *application.SubmitTask) { c.Demand = "urgent" },
	} {
		cmd := f.submitCommand("n-" + name)
		mutate(&cmd)
		if _, err := application.Submit(f.ctx, f.pool, cmd); !errors.Is(err, application.ErrInvalidCommand) {
			t.Errorf("%s: Submit() = %v, want ErrInvalidCommand", name, err)
		}
	}

	unknown := f.submitCommand("n-unknown-provider")
	unknown.Provider = mustID[ids.Provider](t)
	if _, err := application.Submit(f.ctx, f.pool, unknown); !errors.Is(err, application.ErrNoHarness) {
		t.Fatalf("unknown provider = %v, want ErrNoHarness", err)
	}
	off := mustID[ids.Provider](t)
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, err := store.UpsertHarness(f.ctx, uow, lifecycle.HarnessQualification{Tenant: f.tenant, Provider: off, Mode: lifecycle.HarnessOff, Version: 1, UpdatedAt: f.at})
		return err
	})
	disabled := f.submitCommand("n-off")
	disabled.Provider = off
	if _, err := application.Submit(f.ctx, f.pool, disabled); !errors.Is(err, application.ErrNoHarness) {
		t.Fatalf("harness off = %v, want ErrNoHarness", err)
	}

	stranger, err := identity.NewAuthorizationContext(identity.Principal{Tenant: f.tenant, ID: mustID[ids.Principal](t), Kind: identity.PrincipalHuman})
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	intruder := f.submitCommand("n-stranger")
	intruder.Actor = stranger
	if _, err := application.Submit(f.ctx, f.pool, intruder); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("unknown principal = %v, want ErrUnauthorized", err)
	}
}

func TestSubmitTaskCeilingRefusalDoesNotDispatch(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	cmd := f.submitCommand("n-over")
	cmd.Amount = cmd.Ceiling + 1
	_, err := application.Submit(f.ctx, f.pool, cmd)
	requireRefusal(t, err, lifecycle.RejectCeilingReached)
	if _, err := application.GetSubmission(f.ctx, f.pool, f.actor, "n-over"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("refused submission recorded: %v", err)
	}
	rt := f.runtime(t)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 0 {
		t.Fatalf("sent %d requests after a refused submission", got)
	}
}

func TestSubmitTaskPersistsCorrelationIdentities(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	if s.Operation.Attempt != s.Attempt.ID || s.Operation.Provider != f.provider || s.Attempt.Task != s.Task.ID ||
		s.Reservation.Attempt != s.Attempt.ID || s.Budget.Task != s.Task.ID || s.Task.Owner != f.principal {
		t.Fatalf("submission identities do not correlate: %+v", s)
	}
	again := f.reload(t, "n-1")
	if again.Operation.ID != s.Operation.ID || again.Attempt.ID != s.Attempt.ID || again.Provider != s.Provider {
		t.Fatal("re-read submission names different identities")
	}
}

func TestDispatchShadowHarnessWithholdsSend(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessShadow)
	s := f.submit(t, f.submitCommand("n-1"))
	if s.Mode != lifecycle.HarnessShadow {
		t.Fatalf("bound mode = %s, want shadow", s.Mode)
	}
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 0 {
		t.Fatalf("shadow harness sent %d requests", got)
	}
	after := f.reload(t, "n-1")
	if after.Dispatch.Status != "done" || after.Operation.State == lifecycle.OperationAccepted {
		t.Fatalf("shadow dispatch = %s operation %s, want consumed without an effect", after.Dispatch.Status, after.Operation.State)
	}
}

func TestDispatchHarnessDemotionExhausts(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("n-1"))
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, err := store.ApplyHarness(f.ctx, uow, f.tenant, f.provider, lifecycle.HarnessDisable, store.Transition{
			ExpectedVersion: 1, At: f.at, Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}, AuditID: mustID[ids.Audit](t),
		})
		return err
	})
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 0 {
		t.Fatalf("demoted harness sent %d requests", got)
	}
	after := f.reload(t, "n-1")
	if after.Dispatch.ExhaustedAt.IsZero() || after.Dispatch.LastError != "harness_not_live" {
		t.Fatalf("dispatch after demotion = %+v, want exhausted harness_not_live", after.Dispatch)
	}
	if after.Mode != lifecycle.HarnessLive {
		t.Fatalf("bound mode changed to %s; the submission keeps the mode it selected", after.Mode)
	}
}

// governed submits a spend-governed task and checks nothing is enqueued.
func (f *fixture) governed(t *testing.T, nonce string) application.Submission {
	t.Helper()
	cmd := f.submitCommand(nonce)
	cmd.Spend = true
	s := f.submit(t, cmd)
	if s.Challenge == nil || s.Challenge.State != lifecycle.SpendPending || s.Receipt != nil {
		t.Fatalf("governed submission challenge = %+v receipt = %+v", s.Challenge, s.Receipt)
	}
	if s.Dispatch != nil || s.Operation.State != lifecycle.OperationRecorded {
		t.Fatalf("governed submission enqueued before a receipt: %+v", s.Dispatch)
	}
	if s.Reservation.State != lifecycle.ReservationHeld {
		t.Fatalf("governed submission reservation = %s, want held", s.Reservation.State)
	}
	return s
}

func (f *fixture) assertCommand(nonce string, s application.Submission, assertion lifecycle.SpendAssertion) application.AssertSpend {
	return application.AssertSpend{
		Command:         application.Command{Actor: f.actor, Nonce: nonce, ExpectedVersion: s.Challenge.Version},
		SubmissionNonce: s.Nonce,
		Assertion:       assertion,
	}
}

func TestSubmitSpendGovernedAwaitsReceiptThenDispatches(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-gov")
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 0 {
		t.Fatalf("sent %d requests before the receipt", got)
	}

	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	done, err := application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if done.Receipt == nil || done.Challenge.State != lifecycle.SpendAsserted || done.Receipt.Terms.Amount != done.Reservation.Amount {
		t.Fatalf("after assertion: challenge %+v receipt %+v", done.Challenge, done.Receipt)
	}
	if done.Dispatch == nil || done.Dispatch.Status != "pending" || done.Dispatch.Demand != contracts.DemandPerson {
		t.Fatalf("dispatch after receipt = %+v, want pending with the submission's demand", done.Dispatch)
	}

	again, err := application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion))
	if err != nil || again.Dispatch.ID != done.Dispatch.ID {
		t.Fatalf("replayed assertion = %+v, %v", again.Dispatch, err)
	}

	f.tick(t, rt)
	if got := rt.Ledger().Deliveries(s.Operation.ID); got != 1 {
		t.Fatalf("deliveries after receipt = %d, want 1", got)
	}
}

func TestSubmitSpendGovernedRejectsMismatchedAssertion(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-gov")
	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal, Drift: true}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	_, err = application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion))
	requireRefusal(t, err, lifecycle.RejectAssertionMismatch)
	after := f.reload(t, "n-gov")
	if after.Challenge.State != lifecycle.SpendPending || after.Receipt != nil || after.Dispatch != nil {
		t.Fatalf("after mismatch: challenge %s receipt %+v dispatch %+v", after.Challenge.State, after.Receipt, after.Dispatch)
	}
}

func TestSubmitSpendGovernedRejectsExpiredAssertion(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-gov")
	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal, Expired: true}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	_, err = application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion))
	requireRefusal(t, err, lifecycle.RejectRequestExpired)
	if after := f.reload(t, "n-gov"); after.Receipt != nil || after.Dispatch != nil {
		t.Fatalf("expired assertion left a receipt or dispatch: %+v", after)
	}
}

func TestSubmitSpendAssertsRejectStaleVersion(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-gov")
	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	stale := f.assertCommand("a-1", s, assertion)
	stale.ExpectedVersion++
	_, err = application.Assert(f.ctx, f.pool, stale)
	requireRefusal(t, err, lifecycle.RejectVersionConflict)
	if after := f.reload(t, "n-gov"); after.Challenge.State != lifecycle.SpendPending || after.Dispatch != nil {
		t.Fatalf("stale assertion changed state: %+v", after)
	}
	f.submit(t, f.submitCommand("n-plain"))
	plain := stale
	plain.ExpectedVersion = s.Challenge.Version
	plain.SubmissionNonce = "n-plain"
	plain.Nonce = "a-2"
	if _, err := application.Assert(f.ctx, f.pool, plain); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("assertion against an ungoverned submission = %v, want ErrInvalidCommand", err)
	}
}

// TestRefusalEvidencePersistsAfterRollback is not an acceptance test: it
// reads the audit table directly to prove what the boundary cannot show. A
// refused multi-write command rolls back entirely, yet its refusal audit
// survives, and nothing else from the command does.
func TestRefusalEvidencePersistsAfterRollback(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	count := func(table, where string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx, `select count(*) from `+table+` where tenant_id = $1 and `+where, f.tenant.String()).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	cmd := f.submitCommand("n-over")
	cmd.Amount = cmd.Ceiling + 1
	_, err := application.Submit(f.ctx, f.pool, cmd)
	requireRefusal(t, err, lifecycle.RejectCeilingReached)
	if got := count("audit_event", "outcome = 'rejected' and reason = 'ceiling_reached'"); got != 1 {
		t.Fatalf("ceiling refusal audits = %d, want 1", got)
	}
	for _, table := range []string{"task", "attempt", "budget", "provider_operation", "outbox", "command_record"} {
		if got := count(table, "true"); got != 0 {
			t.Fatalf("%s rows after a refused submission = %d, want 0", table, got)
		}
	}
	if got := count("audit_event", "outcome = 'applied'"); got != 0 {
		t.Fatalf("applied audits after a refused submission = %d, want 0", got)
	}

	s := f.governed(t, "n-gov")
	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal, Drift: true}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	_, err = application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion))
	requireRefusal(t, err, lifecycle.RejectAssertionMismatch)
	if got := count("audit_event", "outcome = 'rejected' and reason = 'assertion_mismatch'"); got != 1 {
		t.Fatalf("assertion refusal audits = %d, want 1", got)
	}
	if got := count("command_record", "command = 'assert_spend'"); got != 0 {
		t.Fatalf("refused assertion recorded its nonce: %d rows", got)
	}
}

// TestReceiptWithoutHeldReservationIsRefused is not an acceptance test: no M1
// command releases a reservation before assertion, so the store releases it
// here to prove the boundary still requires a held reservation, not only a
// receipt, before it enqueues a governed dispatch.
func TestReceiptWithoutHeldReservationIsRefused(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "n-gov")
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, _, err := store.ReleaseBudget(f.ctx, uow, f.tenant, s.Budget.ID, s.Reservation.ID, store.Transition{
			ExpectedVersion: s.Budget.Version, At: f.at, Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}, AuditID: mustID[ids.Audit](t),
		})
		return err
	})
	spender := &simulator.Spender{Tenant: f.tenant, Principal: f.principal}
	assertion, err := spender.Assert(*s.Challenge, f.at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Spender.Assert() error = %v", err)
	}
	if _, err := application.Assert(f.ctx, f.pool, f.assertCommand("a-1", s, assertion)); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("assertion with a released reservation = %v, want ErrInvalidCommand", err)
	}
	if after := f.reload(t, "n-gov"); after.Receipt != nil || after.Dispatch != nil {
		t.Fatalf("receipt or dispatch committed without a held reservation: %+v", after)
	}
}
