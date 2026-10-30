//go:build integration

package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

func TestResultProposalConcurrentStreamsJoinOnce(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	proposed := g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(1, contracts.EntryResultProposed, g.proposal)))
	proposed.Stream = "proposal"
	finished := g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(1, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: time.Now()})))
	finished.Stream = "finish"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, e := range []contracts.Event{proposed, finished} {
		wg.Add(1)
		go func(e contracts.Event) {
			defer wg.Done()
			_, err := application.Ingest(g.ctx, g.pool, g.peer(), e, time.Now())
			errs <- err
		}(e)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []contracts.Event{proposed, finished} {
		g.ingestNow(t, e)
	}
	if g.count(t, "result_proposal", "true") != 1 || g.count(t, "artifact_work", "true") != 1 {
		t.Fatal("join not exactly once")
	}
	g.finalize(t)
}

func TestResultProposalRejectsForgedChangedAndLateEvidence(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	e := g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(2, contracts.EntryResultProposed, g.proposal)))
	wrong := g.peer()
	wrong.Provider = mustID[ids.Provider](t)
	if _, err := application.Ingest(g.ctx, g.pool, wrong, e, time.Now()); err == nil {
		t.Fatal("forged provider accepted")
	}
	wrong = g.peer()
	wrong.Tenant = mustID[ids.Tenant](t)
	if _, err := application.Ingest(g.ctx, g.pool, wrong, e, time.Now()); err == nil {
		t.Fatal("cross-tenant proposal accepted")
	}
	g.ingestNow(t, e)
	changed := g.proposal
	changed.SourceRevision = "different"
	got := g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(3, contracts.EntryResultProposed, changed))))
	if got.Refused != 1 {
		t.Fatal("changed proposal accepted")
	}
	if g.count(t, "artifact_work", "true") != 0 {
		t.Fatal("proposal activated before completed turn")
	}
	task, err := store.GetTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = application.Cancel(g.ctx, g.pool, g.cancelCommand("stop", task.ID, task.Version)); err != nil {
		t.Fatal(err)
	}
	got = g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(4, contracts.EntryResultProposed, g.proposal), row(5, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: time.Now()}))))
	if got.Refused != 1 || g.count(t, "artifact_work", "true") != 0 {
		t.Fatal("late result revived cancelled work")
	}
}

func TestArtifactPartialAdmissionCannotFinalize(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	test := g.proposal.Artifacts[0]
	test.NativeRef = "tests"
	test.Kind = lifecycle.ArtifactTestResult
	g.proposal.Artifacts = append(g.proposal.Artifacts, test)
	g.propose(t, true)
	if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "artifact", "true") != 1 || g.count(t, "result_revision", "true") != 0 {
		t.Fatal("partial proposal finalized")
	}
}

type mutateArtifactPort struct {
	contracts.Port
	store   *simulator.ByteArtifactStore
	upload  contracts.ArtifactTransfer
	rewrite bool
}

func (p *mutateArtifactPort) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, r)
	if err != nil {
		return reply, err
	}
	if r.Method == contracts.MethodAuthorizeUpload {
		var a contracts.UploadAuthorized
		if err = json.Unmarshal(reply.Result, &a); err != nil {
			return reply, err
		}
		if err = json.Unmarshal(r.Arguments, &p.upload); err != nil {
			return reply, err
		}
		p.upload.Upload = a.Upload
	}
	if r.Method == contracts.MethodStatObject && !p.rewrite {
		if err = p.store.Upload(p.upload, []byte("changed after stat")); err != nil {
			return reply, err
		}
	}
	if r.Method == contracts.MethodCommitManifest && p.rewrite {
		var c contracts.ArtifactCommitted
		if err = json.Unmarshal(reply.Result, &c); err != nil {
			return reply, err
		}
		c.Manifest.SourceRevision = "forged"
		reply.Result, err = json.Marshal(c)
	}
	return reply, err
}
func TestArtifactCommitRechecksBytesAndExactManifest(t *testing.T) {
	t.Parallel()
	for _, rewrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutated_bytes", true: "mismatched_receipt"}[rewrite], func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			port := &mutateArtifactPort{Port: g.bytes, store: g.bytes, rewrite: rewrite}
			if err := g.artifactWorker(t, port).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 {
				t.Fatal("stat or mismatched receipt was treated as commitment")
			}
		})
	}
}

func TestArtifactCurrentWriterRejectsStaleAndFutureFences(t *testing.T) {
	t.Parallel()
	for _, epoch := range []uint64{0, 2} {
		t.Run(map[uint64]string{0: "future", 2: "stale"}[epoch], func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			if _, err := g.pool.Exec(g.ctx, `update attempt set artifact_writer_epoch=$3 where tenant_id=$1 and attempt_id=$2`, g.tenant.String(), g.s.Attempt.ID.String(), int64(epoch)); err != nil {
				t.Fatal(err)
			}
			if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if len(g.bytes.Ledger().Sent()) != 0 || g.count(t, "artifact", "true") != 0 {
				t.Fatal("invalid writer reached store")
			}
		})
	}
}

func TestArtifactShadowAndCancellationBeforeAdmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"shadow", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			if mode == "shadow" {
				g.seed(t, func(u *store.UnitOfWork) error {
					_, err := store.UpsertHarness(g.ctx, u, lifecycle.HarnessQualification{Tenant: g.tenant, Provider: g.provider, Mode: lifecycle.HarnessShadow, Version: 2, UpdatedAt: time.Now()})
					return err
				})
			} else {
				task, err := store.GetTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = application.Cancel(g.ctx, g.pool, g.cancelCommand("cancel-result", task.ID, task.Version)); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if len(g.bytes.Ledger().Sent()) != 0 || g.count(t, "artifact", "true") != 0 {
				t.Fatal("inactive scope admitted artifacts")
			}
		})
	}
}

func (g *resultFixture) work(t *testing.T) store.ArtifactWork {
	t.Helper()
	var id string
	if err := g.pool.QueryRow(g.ctx, `select operation_id::text from artifact_work where tenant_id=$1`, g.tenant.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	op, err := ids.Parse[ids.Operation](id)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.GetArtifactWork(g.ctx, g.pool, g.tenant, op)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestArtifactCrashAfterStoreCommitFencesOldWorker(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	work := g.work(t)
	pause := g.bytes.Control().Pause(work.Manifest.Operation, simulator.AfterEffect)
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.artifactWorker(t, g.bytes).Tick(ctx) }()
	select {
	case <-pause.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("commit effect not reached")
	}
	// Expire only after confirmed external commitment, not via a timing race.
	if _, err := g.pool.Exec(g.ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and outbox_id=$2`, g.tenant.String(), work.Manifest.Operation.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.pool.Exec(g.ctx, `update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and attempt_id=$2`, g.tenant.String(), g.s.Attempt.ID.String()); err != nil {
		t.Fatal(err)
	}
	cancel()
	pause.Release()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if g.count(t, "artifact", "true") != 0 {
		t.Fatal("stale worker admitted committed bytes")
	}
	g.finalize(t)
	if g.bytes.Ledger().Effects(work.Manifest.Operation) != 1 {
		t.Fatal("recovery repeated commitment")
	}
}

func TestResultRuntimeFailureAndReconciliationPreservePhase(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(4, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationSucceeded}))))
	// A different bound turn fails while this attempt is finalizing; its
	// failure must resolve the Task's verification instead of stranding it.
	g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(5, contracts.EntryTurnFinished, failedTurn("s-1", "late-turn", time.Now())))))
	if task, attempt := g.states(t, g.s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("failure stranded %s/%s", task, attempt)
	}
}

func TestReviewConcurrentDispositionsOnlyOneWins(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	r := g.finalize(t)
	a := g.reviewCommand(t, r, "accept", lifecycle.DispositionAccept)
	b := g.reviewCommand(t, r, "reject", lifecycle.DispositionReject)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, cmd := range []application.ReviewResultCommand{a, b} {
		wg.Add(1)
		go func(cmd application.ReviewResultCommand) {
			defer wg.Done()
			_, err := application.ReviewResult(g.ctx, g.pool, cmd)
			errs <- err
		}(cmd)
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		}
	}
	if success != 1 || g.count(t, "result_review", "true") != 1 {
		t.Fatal("concurrent reviews both took effect")
	}
}

func TestArtifactMigrationPreservesEvidenceAndRefusesDowngrade(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	g.finalize(t)
	db, err := sql.Open("pgx", g.pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.ApplyVersion(g.ctx, 6, false); err == nil || !strings.Contains(err.Error(), "version 6 artifact evidence") {
		t.Fatalf("downgrade=%v", err)
	}
	if v, err := p.GetDBVersion(g.ctx); err != nil || v != migrations.CurrentVersion {
		t.Fatalf("schema=%d %v", v, err)
	}
	if _, err = g.pool.Exec(g.ctx, `update artifact_work set manifest='{}' where tenant_id=$1`, g.tenant.String()); err == nil {
		t.Fatal("staged manifest changed")
	}
	if _, err = g.pool.Exec(g.ctx, `delete from artifact_verification where tenant_id=$1`, g.tenant.String()); err == nil {
		t.Fatal("verification evidence deleted")
	}
}

func TestReviewSpendGovernedReworkAwaitsExactReceipt(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	r := g.finalize(t)
	cmd := g.reviewCommand(t, r, "spend-rework", lifecycle.DispositionRequestChanges)
	cmd.Rework = &application.ReworkTerms{Provider: g.provider, EnvironmentProvider: g.environmentProvider, Deadline: time.Now().Add(time.Hour), Unit: "tokens", Amount: 100, Spend: true}
	view, err := application.ReviewResult(g.ctx, g.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	s := view.Rework
	if s == nil || s.Challenge == nil || s.Dispatch != nil || s.Task.State != lifecycle.TaskQueued {
		t.Fatalf("premature dispatch=%+v", view)
	}
	spender := &simulator.Spender{Tenant: g.tenant, Principal: g.principal}
	assertion, err := spender.Assert(*s.Challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ac := application.AssertSpend{Command: application.Command{Actor: g.actor, Nonce: "rework-receipt", ExpectedVersion: s.Challenge.Version}, ReviewNonce: cmd.Nonce, Assertion: assertion}
	admitted, err := application.Assert(g.ctx, g.pool, ac)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Dispatch != nil || admitted.Receipt == nil || admitted.Task.State != lifecycle.TaskRunning {
		t.Fatalf("assertion failed to dispatch=%+v", admitted)
	}
	environmentOperation, err := store.GetOperation(g.ctx, g.pool, g.tenant, admitted.Attempt.EnvironmentOperation)
	if err != nil || environmentOperation.State != lifecycle.OperationDispatched {
		t.Fatalf("environment provision = %+v, %v; want dispatched after receipt", environmentOperation, err)
	}
	replay, err := application.Assert(g.ctx, g.pool, ac)
	if err != nil || replay.Operation.ID != admitted.Operation.ID || replay.Attempt.EnvironmentOperation != admitted.Attempt.EnvironmentOperation || replay.Dispatch != nil {
		t.Fatal("assertion replay changed work", err)
	}
	if g.count(t, "attempt", "true") != 2 || g.count(t, "spend_receipt", "true") != 1 {
		t.Fatal("duplicate spend rework")
	}
}

func TestResultRuntimeReconciliationRestoresFinalizing(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	// Drive only the crash's uncertainty marker; the proposal and final result
	// still originate through the application/worker boundary.
	g.seed(t, func(u *store.UnitOfWork) error {
		op, err := store.GetOperation(g.ctx, u, g.tenant, g.s.Operation.ID)
		if err != nil {
			return err
		}
		id, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		op, _, err = store.ApplyOperation(g.ctx, u, g.tenant, op.ID, lifecycle.OperationOutcomeUnknown, "", store.Transition{ExpectedVersion: op.Version, Actor: worker.SystemActor(), At: time.Now(), AuditID: id})
		if err != nil {
			return err
		}
		return (progress.Step{UOW: u, Tenant: g.tenant, Actor: worker.SystemActor(), At: time.Now()}).FollowOperation(g.ctx, op)
	})
	if task, attempt := g.states(t, g.s); task != lifecycle.TaskVerifying || attempt != lifecycle.AttemptReconciling {
		t.Fatalf("expected recovery pause=%s/%s", task, attempt)
	}
	work := g.work(t)
	for range 2 {
		if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
			t.Fatal(err)
		}
		var status string
		var attempts int
		if err := g.pool.QueryRow(g.ctx, `select status,attempts from outbox where tenant_id=$1 and outbox_id=$2`, g.tenant.String(), work.Manifest.Operation.String()).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || attempts != 0 {
			t.Fatalf("reconciliation consumed artifact work: status=%s attempts=%d", status, attempts)
		}
	}
	if g.count(t, "provider_operation", "operation_id in ($2,$3,$4,$5)", work.Authorize.String(), work.Export.String(), work.Stat.String(), work.Manifest.Operation.String()) != 0 || g.count(t, "artifact_verification", "true") != 0 {
		t.Fatal("reconciliation started artifact dispatch or admitted artifacts")
	}
	g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(4, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationSucceeded}))))
	if task, attempt := g.states(t, g.s); task != lifecycle.TaskVerifying || attempt != lifecycle.AttemptFinalizing {
		t.Fatalf("reconciliation lost phase=%s/%s", task, attempt)
	}
	g.finalize(t)
}

type delayedArtifactPort struct {
	contracts.Port
	delay time.Duration
}

func (p delayedArtifactPort) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	timer := time.NewTimer(p.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return contracts.Reply{}, ctx.Err()
	case <-timer.C:
		return p.Port.Send(ctx, r)
	}
}

func TestArtifactRenewsLeaseAcrossSlowStages(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	cfg := worker.DefaultConfig()
	cfg.ClaimTTL = 2 * time.Second
	cfg.SendTimeout = time.Second
	port := delayedArtifactPort{Port: g.bytes, delay: 750 * time.Millisecond}
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "slow-worker",
		wiretest.Ports(t, g.pool, g.tenant, map[ids.ProviderID]contracts.Port{g.provider: g.rt},
			wiretest.ArtifactStores(map[ids.ProviderID]worker.ArtifactStorePort{g.provider: {Provider: g.storeID, Port: port}})))
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "result_revision", "true") != 1 {
		t.Fatal("aggregate latency exhausted an otherwise valid lease")
	}
}

func TestArtifactDemotionOrCancelWhileCommitInFlightPreventsAdmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"shadow", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			work := g.work(t)
			pause := g.bytes.Control().Pause(work.Manifest.Operation, simulator.AfterEffect)
			w := g.artifactWorker(t, g.bytes)
			done := make(chan error, 1)
			go func() { done <- w.Tick(g.ctx) }()
			select {
			case <-pause.Reached():
			case <-time.After(10 * time.Second):
				t.Fatal("commit not reached")
			}
			if mode == "shadow" {
				g.seed(t, func(u *store.UnitOfWork) error {
					_, err := store.UpsertHarness(g.ctx, u, lifecycle.HarnessQualification{Tenant: g.tenant, Provider: g.provider, Mode: lifecycle.HarnessShadow, Version: 2, UpdatedAt: time.Now()})
					return err
				})
			} else {
				task, err := store.GetTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = application.Cancel(g.ctx, g.pool, g.cancelCommand("cancel-inflight", task.ID, task.Version)); err != nil {
					t.Fatal(err)
				}
			}
			pause.Release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 {
				t.Fatal("in-flight scope change did not fence admission")
			}
		})
	}
}

func TestArtifactExpiryDuringFinalBudgetLockRollsBackAdmission(t *testing.T) {
	t.Parallel()
	g := newResultFixture(t)
	g.propose(t, true)
	work := g.work(t)
	cfg := worker.DefaultConfig()
	cfg.ClaimTTL = 2 * time.Second
	cfg.SendTimeout = time.Second
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "expiry-worker",
		wiretest.Ports(t, g.pool, g.tenant, map[ids.ProviderID]contracts.Port{g.provider: g.rt},
			wiretest.ArtifactStores(map[ids.ProviderID]worker.ArtifactStorePort{g.provider: {Provider: g.storeID, Port: g.bytes}})))
	if err != nil {
		t.Fatal(err)
	}
	// Hold only the budget, so the worker reaches finalization after a real
	// immutable store commit, then waits to release the unused reservation.
	locked, err := store.Begin(g.ctx, g.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(g.ctx) }()
	if _, err = store.GetBudgetForUpdate(g.ctx, locked, g.tenant, g.s.Budget.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Tick(g.ctx) }()
	waitCtx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	for {
		var blocked bool
		if err = g.pool.QueryRow(waitCtx, `select exists(select 1 from pg_stat_activity where datname=current_database() and wait_event_type='Lock' and query like '%from budget%for update%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("worker never waited on budget")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A server-side wait exercises expiry while the result transaction already
	// holds scope locks; its final acknowledgement must use a fresh clock.
	if _, err = locked.Exec(g.ctx, `select pg_sleep(2.2)`); err != nil {
		t.Fatal(err)
	}
	if err = locked.Rollback(g.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("expired admission=%v", err)
	}
	if g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 {
		t.Fatal("expired transaction committed admission")
	}
	g.finalize(t)
	if g.bytes.Ledger().Effects(work.Manifest.Operation) != 1 {
		t.Fatal("lease recovery repeated external commitment")
	}
}
