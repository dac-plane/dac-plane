//go:build integration

package application_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

type resultFixture struct {
	*fixture
	s        application.Submission
	rt       *simulator.Runtime
	bytes    *simulator.ByteArtifactStore
	storeID  ids.ProviderID
	proposal contracts.ResultProposed
}

func newResultFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "result")
	id := mustID[ids.Provider](t)
	b, err := simulator.NewByteArtifactStore(simulator.Config{Tenant: f.tenant, Provider: id})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("exact patch bytes")
	rt.WithArtifactExport(b, map[string][]byte{"patch": data, "tests": []byte("exit=0")})
	p := contracts.ResultProposed{SessionRef: "s-1", TurnRef: "t-1", SourceRevision: "source-sha-1", Artifacts: []contracts.ArtifactDeclaration{{NativeRef: "patch", Kind: lifecycle.ArtifactCodeChange, Digest: identity.DigestOf(data), Size: uint64(len(data)), MediaType: "text/plain"}}}
	return &resultFixture{fixture: f, s: s, rt: rt, bytes: b, storeID: id, proposal: p}
}

func (g *resultFixture) propose(t *testing.T, first bool) {
	t.Helper()
	proposal := row(2, contracts.EntryResultProposed, g.proposal)
	finished := row(3, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: g.at.Add(time.Second)})
	if !first {
		proposal.Sequence, proposal.Updated = 3, 3
		finished.Sequence, finished.Updated = 2, 2
		g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, finished, proposal)))
	} else {
		g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false, proposal, finished)))
	}
}

func (g *resultFixture) artifactWorker(t *testing.T, port contracts.Port) *worker.Worker {
	t.Helper()
	cfg := worker.DefaultConfig()
	cfg.Policy.BaseBackoff = time.Millisecond
	cfg.Policy.MaxBackoff = time.Millisecond
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), map[ids.ProviderID]contracts.Port{g.provider: g.rt}, cfg, "artifact-worker", worker.WithArtifactStores(map[ids.ProviderID]worker.ArtifactStorePort{g.provider: {Provider: g.storeID, Port: port}}))
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func (g *resultFixture) finalize(t *testing.T) lifecycle.ResultRevision {
	t.Helper()
	if err := g.artifactWorker(t, g.bytes).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	r, err := store.LatestRevision(g.ctx, g.pool, g.tenant, g.s.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (g *resultFixture) reviewCommand(t *testing.T, r lifecycle.ResultRevision, nonce string, d lifecycle.Disposition) application.ReviewResultCommand {
	t.Helper()
	task, err := store.GetTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return application.ReviewResultCommand{Command: application.Command{Actor: g.actor, Nonce: nonce, ExpectedVersion: task.Version}, Task: task.ID, Revision: r.ID, Disposition: d, At: time.Now()}
}

func TestResultArtifactAdmissionBothObservationOrders(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(map[bool]string{true: "proposal_first", false: "turn_first"}[first], func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, first)
			if task, attempt := g.states(t, g.s); task != lifecycle.TaskVerifying || attempt != lifecycle.AttemptFinalizing {
				t.Fatalf("before admission: %s/%s", task, attempt)
			}
			r := g.finalize(t)
			if len(r.Artifacts) != 1 || len(r.Verifications) != 1 || r.Number != 1 || !r.Previous.IsZero() {
				t.Fatalf("revision=%+v", r)
			}
			a, err := store.GetArtifact(g.ctx, g.pool, g.tenant, r.Artifacts[0])
			if err != nil {
				t.Fatal(err)
			}
			v, err := store.GetArtifactVerification(g.ctx, g.pool, g.tenant, a.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if a.Digest != g.proposal.Artifacts[0].Digest || v.Manifest != a.ArtifactManifest || v.Store != g.storeID {
				t.Fatalf("artifact=%+v evidence=%+v", a, v)
			}
			if task, attempt := g.states(t, g.s); task != lifecycle.TaskReadyForReview || attempt != lifecycle.AttemptCompleted {
				t.Fatalf("after admission: %s/%s", task, attempt)
			}
			op, err := store.GetOperationByAttempt(g.ctx, g.pool, g.tenant, g.s.Attempt.ID)
			if err != nil || op.ID != g.s.Operation.ID {
				t.Fatalf("runtime lookup=%+v %v", op, err)
			}
			for i := 0; i < 2; i++ {
				g.finalize(t)
			}
			if g.count(t, "result_revision", "true") != 1 || g.count(t, "artifact_verification", "true") != 1 || g.count(t, "revision_verification", "true") != 1 {
				t.Fatal("duplicate finalization")
			}
			reservation, err := store.GetReservation(g.ctx, g.pool, g.tenant, g.s.Reservation.ID)
			if err != nil || reservation.State != lifecycle.ReservationReleased {
				t.Fatalf("hold=%+v %v", reservation, err)
			}
		})
	}
}

func TestResultArtifactMismatchNeverCreatesRevision(t *testing.T) {
	for _, mode := range []string{"digest", "size", "missing"} {
		t.Run(mode, func(t *testing.T) {
			g := newResultFixture(t)
			if mode == "digest" {
				g.proposal.Artifacts[0].Digest = identity.DigestOf([]byte("forged"))
			}
			if mode == "size" {
				g.proposal.Artifacts[0].Size++
			}
			if mode == "missing" {
				g.rt.WithArtifactExport(g.bytes, map[string][]byte{})
			}
			g.propose(t, true)
			w := g.artifactWorker(t, g.bytes)
			for i := 0; i < 3; i++ {
				if err := w.Tick(g.ctx); err != nil {
					t.Fatal(err)
				}
				_, err := g.pool.Exec(g.ctx, `update outbox set next_attempt_at=now() where tenant_id=$1`, g.tenant.String())
				if err != nil {
					t.Fatal(err)
				}
			}
			if g.count(t, "artifact", "true") != 0 || g.count(t, "artifact_manifest", "true") != 0 || g.count(t, "result_revision", "true") != 0 {
				t.Fatal("unverified evidence admitted")
			}
			if task, attempt := g.states(t, g.s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
				t.Fatalf("exhaustion=%s/%s", task, attempt)
			}
		})
	}
}

func TestReviewAcceptRejectAndImmutableDisposition(t *testing.T) {
	for _, d := range []lifecycle.Disposition{lifecycle.DispositionAccept, lifecycle.DispositionReject} {
		t.Run(string(d), func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			r := g.finalize(t)
			cmd := g.reviewCommand(t, r, "review", d)
			v, err := application.ReviewResult(g.ctx, g.pool, cmd)
			if err != nil {
				t.Fatal(err)
			}
			again, err := application.ReviewResult(g.ctx, g.pool, cmd)
			if err != nil || again.Review.ID != v.Review.ID {
				t.Fatalf("review replay=%+v %v", again, err)
			}
			cmd.Nonce = "rival"
			cmd.Disposition = lifecycle.DispositionRequestChanges
			cmd.Rework = &application.ReworkTerms{Provider: g.provider, Deadline: time.Now().Add(time.Hour), Unit: "tokens", Amount: 100}
			if _, err = application.ReviewResult(g.ctx, g.pool, cmd); err == nil {
				t.Fatal("terminal disposition changed")
			}
			if g.count(t, "result_review", "true") != 1 {
				t.Fatal("extra review")
			}
		})
	}
}

func TestReviewRequestChangesSchedulesNewAttemptAndPreservesLineage(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	first := g.finalize(t)
	cmd := g.reviewCommand(t, first, "rework", lifecycle.DispositionRequestChanges)
	cmd.Rework = &application.ReworkTerms{Provider: g.provider, Deadline: time.Now().Add(time.Hour), Unit: "tokens", Amount: 100}
	v, err := application.ReviewResult(g.ctx, g.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if v.Rework == nil || v.Rework.Attempt.Number != 2 || v.Rework.Task.ID != g.s.Task.ID {
		t.Fatalf("rework=%+v", v)
	}
	if _, err = application.ReviewResult(g.ctx, g.pool, cmd); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "attempt", "true") != 2 || g.count(t, "result_review", "true") != 1 {
		t.Fatal("duplicate rework")
	}
	second := *v.Rework
	rt := g.runtime(t, second.Operation.ID).WithArtifactExport(g.bytes, map[string][]byte{"patch": []byte("exact patch bytes")})
	g.tick(t, rt)
	g.at = time.Now().UTC().Truncate(time.Microsecond)
	p := g.proposal
	p.SessionRef, p.TurnRef, p.SourceRevision = "s-2", "t-2", "source-sha-2"
	g.ingestNow(t, g.event(rt, second, 1, simulator.RowsFrame(false, row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-2"}), row(2, contracts.EntryResultProposed, p), row(3, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-2", TurnRef: "t-2", Outcome: lifecycle.TurnCompleted, EndedAt: time.Now()}))))
	g.rt = rt
	next := g.finalize(t)
	if next.Number != 2 || next.Previous != first.ID || next.Attempt != second.Attempt.ID || next.Artifacts[0] == first.Artifacts[0] {
		t.Fatalf("lineage=%+v", next)
	}
	old, err := store.GetRevision(g.ctx, g.pool, g.tenant, first.ID)
	if err != nil || old.Artifacts[0] != first.Artifacts[0] {
		t.Fatal("prior revision changed", err)
	}
	stale := g.reviewCommand(t, first, "stale-review", lifecycle.DispositionAccept)
	if _, err = application.ReviewResult(g.ctx, g.pool, stale); err == nil {
		t.Fatal("old revision accepted")
	}
}

func TestReviewBudgetFailureAndUnauthorizedActorRollBack(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	r := g.finalize(t)
	cmd := g.reviewCommand(t, r, "too-large", lifecycle.DispositionRequestChanges)
	cmd.Rework = &application.ReworkTerms{Provider: g.provider, Deadline: time.Now().Add(time.Hour), Unit: "tokens", Amount: 1001}
	if _, err := application.ReviewResult(g.ctx, g.pool, cmd); err == nil {
		t.Fatal("budget bypass")
	}
	if g.count(t, "attempt", "true") != 1 || g.count(t, "result_review", "true") != 0 {
		t.Fatal("partial review committed")
	}
	stranger := identity.Principal{Tenant: g.tenant, ID: mustID[ids.Principal](t), Kind: identity.PrincipalService}
	g.seed(t, func(u *store.UnitOfWork) error { return store.InsertPrincipal(g.ctx, u, stranger) })
	actor, err := identity.NewAuthorizationContext(stranger)
	if err != nil {
		t.Fatal(err)
	}
	cmd = g.reviewCommand(t, r, "stranger", lifecycle.DispositionAccept)
	cmd.Actor = actor
	if _, err = application.ReviewResult(g.ctx, g.pool, cmd); err == nil {
		t.Fatal("unauthorized acceptance")
	}
}

type lostArtifactReply struct {
	contracts.Port
	once sync.Once
}

func (p *lostArtifactReply) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, r)
	if err == nil && r.Method == contracts.MethodCommitManifest {
		lost := false
		p.once.Do(func() { lost = true })
		if lost {
			return contracts.Reply{}, errors.New("lost reply after immutable commit")
		}
	}
	return reply, err
}

func TestArtifactLostCommitReplyRecoversExactManifest(t *testing.T) {
	g := newResultFixture(t)
	g.propose(t, true)
	port := &lostArtifactReply{Port: g.bytes}
	w := g.artifactWorker(t, port)
	if err := w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "artifact", "true") != 0 {
		t.Fatal("lost reply guessed as admission")
	}
	if _, err := g.pool.Exec(g.ctx, `update outbox set next_attempt_at=now() where tenant_id=$1`, g.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if err := g.artifactWorker(t, port).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.count(t, "artifact", "true") != 1 || g.count(t, "result_revision", "true") != 1 {
		t.Fatal("recorded commit not recovered")
	}
	effects := 0
	for _, e := range g.bytes.Ledger().Applied() {
		if e.Kind == simulator.ManifestCommitted {
			effects++
		}
	}
	if effects != 1 {
		t.Fatalf("commit effects=%d", effects)
	}
}
