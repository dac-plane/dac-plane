//go:build integration

package application_test

import (
	"context"
	"io"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSecurityLateUsageRechecksQueuedReworkCoverage(t *testing.T) {
	for _, usage := range []uint64{1200, 950, 900} {
		t.Run(strconv.FormatUint(usage, 10), func(t *testing.T) {
			g := newResultFixture(t)
			g.propose(t, true)
			revision := g.finalize(t)
			cmd := g.reviewCommand(t, revision, "queued-rework", lifecycle.DispositionRequestChanges)
			cmd.Rework = &application.ReworkTerms{Provider: g.provider, Unit: "tokens", Amount: 100, Deadline: time.Now().Add(time.Hour)}
			if _, err := application.ReviewResult(g.ctx, g.pool, cmd); err != nil {
				t.Fatal(err)
			}
			g.ingestNow(t, g.event(g.rt, g.s, 1, simulator.RowsFrame(false,
				row(4, contracts.EntryUsageReported, contracts.UsageReported{Unit: "tokens", Cumulative: usage}))))
			attempt, err := store.GetAttemptByTask(g.ctx, g.pool, g.tenant, g.s.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err := store.GetOperationByAttempt(g.ctx, g.pool, g.tenant, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			rt := g.runtime(t, op.ID)
			g.tick(t, rt)
			g.tick(t, rt)
			wantEffects := 0
			if usage+100 == 1000 {
				wantEffects = 1
			}
			if effects := rt.Ledger().Effects(op.ID); effects != wantEffects {
				t.Fatalf("usage=%d: runtime effects=%d, want %d", usage, effects, wantEffects)
			}
			b, err := store.GetBudget(g.ctx, g.pool, g.tenant, g.s.Budget.ID)
			if err != nil || b.Consumed != usage || b.Ceiling != 1000 {
				t.Fatalf("dispatch changed usage or ceiling: %+v %v", b, err)
			}
			if wantEffects == 0 && b.Reserved != 0 {
				t.Fatalf("refused dispatch retained reservation: %+v", b)
			}
		})
	}
}

func TestSecurityRecoveryAccountsUsageOnBothSidesOfOutcome(t *testing.T) {
	for _, usageFirst := range []bool{true, false} {
		name := "after_outcome"
		if usageFirst {
			name = "before_outcome"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s := f.submit(t, f.submitCommand("usage-recovery"))
			rt := f.runtime(t, s.Operation.ID)
			usageSeq, outcomeSeq := uint64(1), uint64(2)
			if !usageFirst {
				usageSeq, outcomeSeq = 2, 1
			}
			usage := row(usageSeq, contracts.EntryUsageReported, contracts.UsageReported{Unit: s.Unit, Cumulative: 1200})
			outcome := row(outcomeSeq, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationSucceeded})
			frame := simulator.RowsFrame(false, usage, outcome)
			if !usageFirst {
				frame = simulator.RowsFrame(false, outcome, usage)
			}
			p := &recoveryPort{Port: rt,
				send: func(context.Context, contracts.Request) (contracts.Reply, error) {
					return contracts.Reply{}, io.ErrUnexpectedEOF
				},
				subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
					return &recoveryStream{frames: []contracts.StreamFrame{frame}}, nil
				},
			}
			if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.ingestNow(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.HeartbeatFrame(f.at)))
			// Fail settlement after accounting and outcome writes. Neither may
			// survive without the matching durable cursor and history.
			if _, err := f.pool.Exec(f.ctx, `create function security_fail_usage_cursor() returns trigger language plpgsql as $$ begin raise exception 'injected cursor failure'; end $$;
			create trigger security_fail_usage_cursor before update on stream_cursor for each row execute function security_fail_usage_cursor()`); err != nil {
				t.Fatal(err)
			}
			before := f.recoverySnapshot(t)
			if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
				t.Fatal("cursor failure partially committed usage, outcome or history")
			}
			if _, err := f.pool.Exec(f.ctx, `drop trigger security_fail_usage_cursor on stream_cursor`); err != nil {
				t.Fatal(err)
			}
			f.reopen(t)
			if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			got := f.ingestNow(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, frame))
			b, err := store.GetBudget(f.ctx, f.pool, f.tenant, s.Budget.ID)
			if err != nil || b.Consumed != 1200 || b.Overshoot != 200 || got.Duplicates != 2 {
				t.Fatalf("recovery accounting=%+v replay=%+v err=%v", b, got, err)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
			if err != nil || op.State != lifecycle.OperationSucceeded || rt.Ledger().Effects(op.ID) != 0 {
				t.Fatalf("recovery outcome=%+v err=%v", op, err)
			}
			if n := f.count(t, "durable_event", "record_kind='budget' and event='consume'"); n != 1 {
				t.Fatalf("usage charged %d times", n)
			}
		})
	}
}
