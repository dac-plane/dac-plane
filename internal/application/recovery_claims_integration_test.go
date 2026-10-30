//go:build integration

package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestRecoveryStaleSendSettlementWritesNothing(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"outbox", "attempt", "expiry"} {
		t.Run(fence, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s := f.submit(t, f.submitCommand("stale"))
			f.dispatchPendingEnvironments(t)
			rt := f.runtime(t, s.Operation.ID)
			var taken map[string]string
			p := &recoveryPort{Port: rt, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
				reply, err := rt.Send(ctx, r)
				if err != nil {
					return reply, err
				}
				switch fence {
				case "outbox":
					_, err = f.pool.Exec(ctx, `update outbox set lease_epoch=lease_epoch+1,claimed_by='replacement' where tenant_id=$1 and record_id=$2`, f.tenant.String(), s.Operation.ID.String())
				case "attempt":
					_, err = f.pool.Exec(ctx, `update attempt set lease_epoch=lease_epoch+1,lease_owner='replacement' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
				case "expiry":
					_, err = f.pool.Exec(ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and record_id=$2`, f.tenant.String(), s.Operation.ID.String())
				}
				if err != nil {
					t.Fatal(err)
				}
				taken = f.recoverySnapshot(t)
				return reply, nil
			}}
			if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil && !errors.Is(err, store.ErrStaleVersion) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(taken, f.recoverySnapshot(t)) {
				t.Fatal("stale sender committed state, history or acknowledgement")
			}
			if rt.Ledger().Effects(s.Operation.ID) != 1 {
				t.Fatal("lost external effect ledger")
			}
		})
	}
}

func TestRecoveryStaleDecisionAcknowledgementWritesNothing(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"outbox", "attempt", "expiry"} {
		t.Run(fence, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.executing(t, "decision")
			f.raise(t, rt, s, 2, question("question", f.at.Add(time.Hour)))
			cmd := f.decideCommand("answer", f.attention(t, s), lifecycle.DecisionDeny)
			cmd.At = f.at
			view := f.decide(t, cmd)
			steer := f.steer(t, s)
			var taken map[string]string
			p := &recoveryPort{Port: steer, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
				reply, err := steer.Send(ctx, r)
				if err != nil {
					return reply, err
				}
				switch fence {
				case "outbox":
					_, err = f.pool.Exec(ctx, `update outbox set lease_epoch=lease_epoch+1,claimed_by='replacement' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
				case "attempt":
					_, err = f.pool.Exec(ctx, `update attempt set lease_epoch=lease_epoch+1,lease_owner='replacement' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
				case "expiry":
					_, err = f.pool.Exec(ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
				}
				if err != nil {
					t.Fatal(err)
				}
				taken = f.recoverySnapshot(t)
				return reply, nil
			}}
			if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil && !errors.Is(err, store.ErrStaleVersion) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(taken, f.recoverySnapshot(t)) {
				t.Fatal("stale decision acknowledgement changed product state")
			}
			if steer.Ledger().Effects(s.Operation.ID) != 1 {
				t.Fatal("decision effect count changed")
			}
		})
	}
}

func TestRecoveryShadowGovernedReceiptPreservesPrimary(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), errors.New("lost receipt"))
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	g.seed(t, func(u *store.UnitOfWork) error {
		_, err := u.Exec(g.ctx, `update harness_qualification set mode='shadow',version=version+1 where tenant_id=$1`, g.tenant.String())
		return err
	})
	before := g.recoverySnapshot(t)
	for range 2 {
		g.reopen(t)
		if err := governedRecoveryWorker(t, g, e).Reconcile(g.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, g.recoverySnapshot(t)) {
		t.Fatal("shadow receipt settled primary action")
	}
	if g.count(t, "shadow_result", "true") != 1 || g.resource.Ledger().Effects(g.cmd.Request.Operation) != 1 {
		t.Fatal("lost comparison or repeated governed effect")
	}
}

func TestRecoveryPendingAndMalformedRepliesRemainUncertain(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"pending", "malformed", "wrong_delivery"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s := f.submit(t, f.submitCommand("reply"))
			f.dispatchPendingEnvironments(t)
			rt := f.runtime(t, s.Operation.ID)
			p := &recoveryPort{Port: rt, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
				reply, err := rt.Send(ctx, r)
				switch kind {
				case "pending":
					reply.Durability = lifecycle.SendPending
				case "malformed":
					reply.Outcome = "invalid"
				case "wrong_delivery":
					reply.Delivery = mustID[ids.Delivery](t)
				}
				return reply, err
			}}
			w := recoveryWorker(t, f, p)
			if err := w.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err := w.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
			if err != nil || op.State != lifecycle.OperationUncertain || len(rt.Ledger().Sent()) != 1 {
				t.Fatalf("reply=%s state=%s err=%v", kind, op.State, err)
			}
		})
	}
}

func TestRecoveryCrashBoundariesPreserveUncertainty(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"unclaimed", "marked_before_effect", "after_effect"} {
		t.Run(boundary, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s := f.submit(t, f.submitCommand("crash"))
			f.dispatchPendingEnvironments(t)
			rt := f.runtime(t, s.Operation.ID)
			if boundary == "marked_before_effect" {
				if _, err := f.pool.Exec(f.ctx, `update outbox set status='processing',last_error='send_started',lease_epoch=lease_epoch+1,claimed_by='dead',lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and record_id=$2`, f.tenant.String(), s.Operation.ID.String()); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "after_effect" {
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				p := &recoveryPort{Port: rt, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
					reply, err := rt.Send(ctx, r)
					cancel()
					return reply, err
				}}
				if err := recoveryWorker(t, f, p).Tick(ctx); err == nil {
					t.Fatal("cancelled process unexpectedly settled send")
				}
				if _, err := f.pool.Exec(f.ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and record_id=$2`, f.tenant.String(), s.Operation.ID.String()); err != nil {
					t.Fatal(err)
				}
				if _, err := f.pool.Exec(f.ctx, `update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String()); err != nil {
					t.Fatal(err)
				}
			}
			f.reopen(t)
			if err := recoveryWorker(t, f, rt).Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantEffects := lifecycle.OperationUncertain, 0
			if boundary == "unclaimed" {
				wantState, wantEffects = lifecycle.OperationAccepted, 1
			}
			if boundary == "after_effect" {
				wantEffects = 1
			}
			if op.State != wantState || rt.Ledger().Effects(s.Operation.ID) != wantEffects {
				t.Fatalf("boundary=%s state=%s effects=%d", boundary, op.State, rt.Ledger().Effects(s.Operation.ID))
			}
			if boundary == "after_effect" {
				p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
					return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
				}}
				if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
					t.Fatal(err)
				}
				op, err = store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
				if err != nil || op.State != lifecycle.OperationSucceeded || rt.Ledger().Effects(s.Operation.ID) != 1 {
					t.Fatal("applied effect was not recovered exactly once")
				}
			}
		})
	}
}
