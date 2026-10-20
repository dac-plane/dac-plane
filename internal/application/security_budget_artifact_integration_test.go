//go:build integration

package application_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSecurityLateUsageRefusesReworkWithoutIncreasingCeiling(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	r := g.finalize(t)
	g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, row(4, contracts.EntryUsageReported, contracts.UsageReported{Unit: "tokens", Cumulative: 1200}))))
	cmd := g.reviewCommand(t, r, "overspent-rework", lifecycle.DispositionRequestChanges)
	cmd.Rework = &application.ReworkTerms{Provider: g.provider, Unit: "tokens", Amount: 1, Deadline: time.Now().Add(time.Hour)}
	if _, err := application.ReviewResult(g.ctx, g.pool, cmd); err == nil {
		t.Fatal("overspent budget authorized rework")
	}
	b, err := store.GetBudget(g.ctx, g.pool, g.tenant, g.s.Budget.ID)
	if err != nil || b.Ceiling != 1000 || b.Consumed != 1200 || b.Reserved != 0 || b.Overshoot != 200 || g.count(t, "attempt", "true") != 1 || g.count(t, "result_review", "true") != 0 {
		t.Fatalf("failed rework changed accounting=%+v %v", b, err)
	}
	if task, attempt := g.states(t, g.s); task != lifecycle.TaskReadyForReview || attempt != lifecycle.AttemptCompleted {
		t.Fatalf("failed rework changed result=%s/%s", task, attempt)
	}
}

func TestSecurityArtifactRestartAfterBytesBeforeCommit(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	work := g.work(t)
	p := &recoveryPort{Port: g.bytes, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
		if r.Method == contracts.MethodStatObject {
			return contracts.Reply{}, io.ErrUnexpectedEOF
		}
		return g.bytes.Send(ctx, r)
	}}
	if err := g.artifactWorker(t, p).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 || g.bytes.Ledger().Effects(work.Manifest.Operation) != 0 || g.rt.Ledger().Effects(work.Export) != 1 {
		t.Fatal("pre-commit crash did not reach export without admitting evidence")
	}
	g.reopen(t)
	if _, err := g.pool.Exec(g.ctx, `update outbox set next_attempt_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, g.tenant.String()); err != nil {
		t.Fatal(err)
	}
	r := g.finalize(t)
	if r.Number != 1 || g.bytes.Ledger().Effects(work.Manifest.Operation) != 1 || g.count(t, "artifact_verification", "true") != 1 {
		t.Fatal("byte-stage restart did not converge on one admission")
	}
}

// This is an explicit repository-layer race, not an application scheduling API.
func TestSecurityBudgetConcurrentReservationsRespectOneCeiling(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("budget-race"))
	var attempts []lifecycle.Attempt
	f.seed(t, func(u *store.UnitOfWork) error {
		for number := uint32(2); number <= 3; number++ {
			a, err := lifecycle.NewAttempt(mustID[ids.Attempt](t), f.tenant, s.Task.ID, number, f.at)
			if err != nil {
				return err
			}
			if _, _, err := store.InsertAttempt(f.ctx, u, a); err != nil {
				return err
			}
			attempts = append(attempts, a)
		}
		return nil
	})
	start, done := make(chan struct{}), make(chan error, 2)
	for _, a := range attempts {
		reservation, audit := mustID[ids.Reservation](t), mustID[ids.Audit](t)
		go func() {
			<-start
			u, err := store.Begin(f.ctx, f.pool)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = u.Rollback(f.ctx) }()
			_, _, _, err = store.ReserveBudget(f.ctx, u, store.ReservationRequest{Tenant: f.tenant, Budget: s.Budget.ID, Reservation: reservation, Attempt: a.ID, Amount: 600}, store.Transition{ExpectedVersion: s.Budget.Version, At: f.at, Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}, AuditID: audit})
			if err == nil {
				err = u.Commit(f.ctx)
			}
			done <- err
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-done; err == nil {
			winners++
		} else {
			requireRefusal(t, err, lifecycle.RejectVersionConflict)
		}
	}
	b, err := store.GetBudget(f.ctx, f.pool, f.tenant, s.Budget.ID)
	if err != nil || winners != 1 || b.Reserved != 700 || b.Ceiling != 1000 || f.count(t, "reservation", "true") != 2 {
		t.Fatalf("budget race winners=%d budget=%+v err=%v", winners, b, err)
	}
}

func TestSecurityReworkRaceReservesAndDispatchesOnce(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	revision := g.finalize(t)
	cmd := g.reviewCommand(t, revision, "rework-race-a", lifecycle.DispositionRequestChanges)
	cmd.Rework = &application.ReworkTerms{Provider: g.provider, Deadline: time.Now().Add(time.Hour), Unit: "tokens", Amount: 600}
	start, done := make(chan struct{}), make(chan error, 2)
	for _, nonce := range []string{"rework-race-a", "rework-race-b"} {
		c := cmd
		c.Nonce = nonce
		go func() { <-start; _, err := application.ReviewResult(g.ctx, g.pool, c); done <- err }()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-done; err == nil {
			winners++
		} else {
			requireRefusal(t, err, lifecycle.RejectVersionConflict)
		}
	}
	b, err := store.GetBudget(g.ctx, g.pool, g.tenant, g.s.Budget.ID)
	if err != nil || winners != 1 || b.Reserved != 600 || b.Ceiling != 1000 || g.count(t, "attempt", "true") != 2 || g.count(t, "reservation", "true") != 2 || g.count(t, "result_review", "true") != 1 {
		t.Fatalf("rework race winners=%d budget=%+v err=%v", winners, b, err)
	}
	r, err := store.GetRevision(g.ctx, g.pool, g.tenant, revision.ID)
	if err != nil || r.ID != revision.ID || r.Artifacts[0] != revision.Artifacts[0] {
		t.Fatal("race relabelled prior evidence", err)
	}
	a, err := store.GetAttemptByTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperationByAttempt(g.ctx, g.pool, g.tenant, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	rt := g.runtime(t, op.ID)
	g.tick(t, rt)
	g.tick(t, rt)
	if rt.Ledger().Effects(op.ID) != 1 {
		t.Fatal("competing rework produced duplicate or missing runtime effect")
	}
}

func TestSecurityArtifactRestartAfterAdmissionKeepsExactRevision(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	// Restart before any bytes exist; the durable proposal drives the worker.
	g.reopen(t)
	r := g.finalize(t)
	work := g.work(t)
	effects := g.bytes.Ledger().Effects(work.Manifest.Operation)
	if effects != 1 {
		t.Fatalf("commit effects=%d", effects)
	}
	// Discard pool and worker after metadata admission, retaining the external
	// ledger. No recovery pass may allocate new evidence or another revision.
	g.reopen(t)
	if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	next, err := store.LatestRevision(g.ctx, g.pool, g.tenant, g.s.Task.ID)
	if err != nil || next.ID != r.ID || next.Number != r.Number || g.bytes.Ledger().Effects(work.Manifest.Operation) != effects || g.count(t, "artifact_verification", "true") != 1 || g.count(t, "result_revision", "true") != 1 {
		t.Fatalf("post-admission restart=%+v %v", next, err)
	}
}
