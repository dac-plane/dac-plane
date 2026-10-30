//go:build integration

package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestRecoveryArtifactRetryAndFencedAdmission(t *testing.T) {
	t.Parallel()
	for _, outcome := range []lifecycle.ArtifactCommitOutcome{lifecycle.CommitRetry, lifecycle.CommitFenced} {
		t.Run(string(outcome), func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			work := g.work(t)
			var commits []ids.OperationID
			p := &recoveryPort{Port: g.bytes, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
				if r.Method != contracts.MethodCommitManifest {
					return g.bytes.Send(ctx, r)
				}
				commits = append(commits, r.Operation)
				if len(commits) > 1 {
					return g.bytes.Send(ctx, r)
				}
				var transfer contracts.ArtifactTransfer
				if err := json.Unmarshal(r.Arguments, &transfer); err != nil {
					return contracts.Reply{}, err
				}
				receipt := contracts.ArtifactCommitted{Manifest: transfer.Manifest, Outcome: outcome}
				if outcome == lifecycle.CommitRetry {
					receipt.Reason = lifecycle.RetryMissingBytes
				}
				raw, err := json.Marshal(receipt)
				return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: raw}, err
			}}
			if err := g.artifactWorker(t, p).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 || g.count(t, "artifact_commit_evidence", "true") != 1 {
				t.Fatal("nonfinal receipt admitted or lost evidence")
			}
			g.reopen(t)
			if _, err := g.pool.Exec(g.ctx, `update outbox set next_attempt_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, g.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := g.artifactWorker(t, p).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if outcome == lifecycle.CommitFenced {
				if len(commits) != 1 || g.count(t, "artifact", "true") != 0 || g.count(t, "result_revision", "true") != 0 {
					t.Fatal("fenced declaration retried or admitted")
				}
			} else {
				if len(commits) != 2 || commits[0] != work.Manifest.Operation || commits[0] != commits[1] || g.bytes.Ledger().Effects(work.Manifest.Operation) != 1 {
					t.Fatal("retry lost stable commitment identity")
				}
				if g.count(t, "artifact", "true") != 1 || g.count(t, "artifact_verification", "true") != 1 || g.count(t, "result_revision", "true") != 1 {
					t.Fatal("retry failed to finalize once")
				}
			}
		})
	}
}

func TestRecoveryQueuedPolicyGenerationBlocksAfterRestart(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, true)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	g.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(g.ctx, u, store.PolicyUpdate{Tenant: g.tenant, ExpectedGeneration: 1, NextGeneration: 2,
			Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways},
			Ceiling:  map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, At: time.Now()})
		return err
	})
	g.reopen(t)
	g.tick(t, e)
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationRejected || g.resource.Ledger().Effects(op.ID) != 0 {
		t.Fatalf("stale queued authority state=%s err=%v", op.State, err)
	}
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	if g.resource.Ledger().Effects(op.ID) != 0 || g.count(t, "authority_use", "operation_id=$2", op.ID.String()) != 1 {
		t.Fatal("replay revived invalidated authority")
	}
}

func TestRecoveryCancelAndReviewCommandPayloadConflicts(t *testing.T) {
	t.Parallel()
	t.Run("cancel", func(t *testing.T) {
		f := newFixture(t, lifecycle.HarnessLive)
		s := f.governed(t, "queued")
		cmd := f.cancelCommand("cancel", s.Task.ID, s.Task.Version)
		first, err := application.Cancel(f.ctx, f.pool, cmd)
		if err != nil {
			t.Fatal(err)
		}
		f.reopen(t)
		before := f.recoverySnapshot(t)
		again, err := application.Cancel(f.ctx, f.pool, cmd)
		if err != nil || again != first {
			t.Fatalf("cancel replay=%+v err=%v", again, err)
		}
		cmd.ExpectedVersion++
		if _, err := application.Cancel(f.ctx, f.pool, cmd); !errors.Is(err, application.ErrPayloadMismatch) {
			t.Fatalf("changed cancel=%v", err)
		}
		if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
			t.Fatal("cancel replay mutated product state")
		}
	})
	t.Run("review", func(t *testing.T) {
		g := newResultFixture(t)
		g.propose(t, true)
		revision := g.finalize(t)
		cmd := g.reviewCommand(t, revision, "review", lifecycle.DispositionAccept)
		first, err := application.ReviewResult(g.ctx, g.pool, cmd)
		if err != nil {
			t.Fatal(err)
		}
		g.reopen(t)
		before := g.recoverySnapshot(t)
		again, err := application.ReviewResult(g.ctx, g.pool, cmd)
		if err != nil || again.Review.ID != first.Review.ID {
			t.Fatalf("review replay=%+v err=%v", again, err)
		}
		cmd.Disposition = lifecycle.DispositionReject
		if _, err := application.ReviewResult(g.ctx, g.pool, cmd); !errors.Is(err, application.ErrPayloadMismatch) {
			t.Fatalf("changed review=%v", err)
		}
		if !reflect.DeepEqual(before, g.recoverySnapshot(t)) || g.count(t, "result_review", "true") != 1 {
			t.Fatal("review replay changed immutable disposition")
		}
		if _, err := store.LatestRevision(g.ctx, g.pool, g.tenant, g.s.Task.ID); err != nil {
			t.Fatal(err)
		}
	})
}
