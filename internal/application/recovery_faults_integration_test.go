//go:build integration

package application_test

import (
	"context"
	"io"
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

func TestRecoveryRestartWaitingAndTwoDecisions(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("waiting"))
	decisions := map[ids.DecisionID]simulator.RuntimeOp{}
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{s.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}, Decisions: decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, rt)
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "session"}))))
	for i, ref := range []string{"question-one", "question-two"} {
		seq := uint64(2 + i*2)
		f.raise(t, rt, s, seq, question(ref, f.at.Add(time.Hour)))
		before := f.recoverySnapshot(t)
		f.reopen(t)
		if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
			t.Fatal("restart lost waiting state")
		}
		var idText string
		if err := f.pool.QueryRow(f.ctx, `select attention_id::text from attention_request where tenant_id=$1 and state='pending'`, f.tenant.String()).Scan(&idText); err != nil {
			t.Fatal(err)
		}
		id, err := ids.Parse[ids.Attention](idText)
		if err != nil {
			t.Fatal(err)
		}
		attention, err := store.GetAttention(f.ctx, f.pool, f.tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		cmd := f.decideCommand(ref, attention, lifecycle.DecisionDeny)
		cmd.At = f.at
		view := f.decide(t, cmd)
		did, err := ids.Parse[ids.Decision](view.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		decisions[did] = simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered}
		lost := true
		port := &recoveryPort{Port: rt, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
			reply, err := rt.Send(ctx, r)
			if err == nil && lost {
				lost = false
				return contracts.Reply{}, io.ErrUnexpectedEOF
			}
			return reply, err
		}}
		if err := recoveryWorker(t, f, port).Tick(f.ctx); err != nil {
			t.Fatal(err)
		}
		if rt.Ledger().Effects(s.Operation.ID) != i+2 {
			t.Fatal("first delivery did not apply exactly once")
		}
		f.reopen(t)
		if _, err := f.pool.Exec(f.ctx, `update outbox set next_attempt_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), did.String()); err != nil {
			t.Fatal(err)
		}
		if err := recoveryWorker(t, f, port).Tick(f.ctx); err != nil {
			t.Fatal(err)
		}
		if rt.Ledger().Effects(s.Operation.ID) != i+2 {
			t.Fatal("decision redelivery repeated effect")
		}
		if replay := f.decide(t, cmd); replay.DecisionID != view.DecisionID {
			t.Fatal("command replay allocated another decision")
		}
		cmd.Kind = lifecycle.DecisionAnswer
		cmd.AnswerDigest = identity.DigestOf([]byte("changed"))
		if _, err := application.Decide(f.ctx, f.pool, cmd); err == nil {
			t.Fatal("changed decision replay accepted")
		}
		f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(seq+1, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: ref}))))
	}
	if f.count(t, "decision", "true") != 2 || rt.Ledger().Effects(s.Operation.ID) != 3 {
		t.Fatal("lost decision identity or duplicated native work")
	}
}

func TestRecoveryConcurrentCursorPreventsStaleOutcome(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "loss")
	var afterAdvance map[string]string
	port := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 2})))
		afterAdvance = f.recoverySnapshot(t)
		return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
	}}
	if err := recoveryWorker(t, f, port).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterAdvance, f.recoverySnapshot(t)) {
		t.Fatal("stale subscription overrode newer cursor")
	}
}

func TestRecoveryOutcomeAndCursorRollbackTogether(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "loss")
	f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.HeartbeatFrame(f.at)))
	port := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
	}}
	// Fail the cursor UPDATE after operation, lifecycle and history writes.
	if _, err := f.pool.Exec(f.ctx, `create function recovery_fail_cursor() returns trigger language plpgsql as $$ begin raise exception 'injected cursor failure'; end $$;
	create trigger recovery_fail_cursor before update on stream_cursor for each row execute function recovery_fail_cursor()`); err != nil {
		t.Fatal(err)
	}
	before := f.recoverySnapshot(t)
	if err := recoveryWorker(t, f, port).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
		t.Fatal("cursor failure partially committed outcome/history")
	}
	if _, err := f.pool.Exec(f.ctx, `drop trigger recovery_fail_cursor on stream_cursor`); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	if err := recoveryWorker(t, f, port).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	if err != nil || op.State != lifecycle.OperationSucceeded {
		t.Fatalf("rollback recovery=%s err=%v", op.State, err)
	}
	if rt.Ledger().Effects(s.Operation.ID) != 1 {
		t.Fatal("transaction recovery resent runtime")
	}
}

func TestRecoveryReconnectNegotiatesBeforeProviderWork(t *testing.T) {
	for _, mode := range []string{"version", "capability", "before_ready", "reconnect"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s := f.submit(t, f.submitCommand("protocol"))
			rt := f.runtime(t, s.Operation.ID)
			connect := func() *recoveryPort {
				local := simulator.DefaultHello()
				if mode == "version" {
					local.Versions = []uint32{99}
				}
				if mode == "capability" {
					local.Required = []contracts.Capability{"stream.replay"}
					local.Offered = local.Required
				}
				gate, err := contracts.NewGate(local)
				if err != nil {
					t.Fatal(err)
				}
				hello := rt.Hello()
				if mode != "before_ready" {
					_, failure := gate.Receive(contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &hello})
					if mode == "reconnect" && failure != nil {
						t.Fatal(failure)
					}
					if mode == "version" && (failure == nil || failure.Code != contracts.FailureProtocolMismatch) {
						t.Fatalf("version mismatch=%v", failure)
					}
					if mode == "capability" && (failure == nil || failure.Code != contracts.FailureUnsupportedCapability) {
						t.Fatalf("capability mismatch=%v", failure)
					}
				}
				return &recoveryPort{Port: rt, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
					if _, failure := gate.Receive(contracts.Frame{Kind: contracts.FrameRequest, Request: &r}); failure != nil {
						return contracts.Reply{}, failure
					}
					return rt.Send(ctx, r)
				}, subscribe: func(ctx context.Context, c contracts.Cursor) (contracts.Stream, error) {
					if _, open := gate.Selected(); !open {
						return nil, &contracts.Failure{Code: contracts.FailureProtocolMismatch, Message: "not negotiated"}
					}
					return rt.Subscribe(ctx, c)
				}}
			}
			if err := recoveryWorker(t, f, connect()).Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.reopen(t)
			p := connect()
			before := f.recoverySnapshot(t)
			if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
				t.Fatal("invalid reconnect changed primary state")
			}
			want := 0
			if mode == "reconnect" {
				want = 1
			}
			if rt.Ledger().Effects(s.Operation.ID) != want {
				t.Fatal("protocol gate leaked or lost provider work")
			}
		})
	}
}

func TestRecoveryReconnectResumesDurableCursor(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "resume")
	var cursors []contracts.Cursor
	p := &recoveryPort{Port: rt, subscribe: func(_ context.Context, c contracts.Cursor) (contracts.Stream, error) {
		cursors = append(cursors, c)
		if len(cursors) == 1 {
			return &recoveryStream{frames: []contracts.StreamFrame{
				simulator.RowsFrame(false, row(1, "progress", map[string]int{})),
				simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 3}),
			}}, nil
		}
		return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1), outcomeFrame(4)}}, nil
	}}
	if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	if err != nil || op.State != lifecycle.OperationUncertain {
		t.Fatal("disconnect invented an outcome")
	}
	f.reopen(t)
	if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 2 || cursors[1].After != 3 {
		t.Fatalf("reconnect cursors=%+v", cursors)
	}
	op, err = store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	if err != nil || op.State != lifecycle.OperationSucceeded || rt.Ledger().Effects(s.Operation.ID) != 1 {
		t.Fatal("resume lost outcome or repeated effect")
	}
}

func TestRecoveryRehydrationResetLoopIsBounded(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "reset")
	count := 0
	p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		count++
		return &recoveryStream{frames: []contracts.StreamFrame{simulator.CursorTooOldFrame()}}, nil
	}}
	if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	if err != nil || op.State != lifecycle.OperationUncertain || count != 2 || len(rt.Ledger().Sent()) != 1 {
		t.Fatalf("reset count=%d state=%s err=%v", count, op.State, err)
	}
}

func TestRecoveryStaleSubscriptionCannotRelabelGeneration(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "generation")
	var advanced map[string]string
	p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.ClearedFrame(2)))
		advanced = f.recoverySnapshot(t)
		return &recoveryStream{frames: []contracts.StreamFrame{simulator.HeartbeatFrame(f.at), outcomeFrame(1)}}, nil
	}}
	if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(advanced, f.recoverySnapshot(t)) {
		t.Fatal("old stream was relabelled into new generation")
	}
}
