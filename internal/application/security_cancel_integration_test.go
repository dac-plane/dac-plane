//go:build integration

package application_test

import (
	"context"
	"io"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

type lostCancelAcknowledgement struct {
	contracts.Port
	lost bool
}

func (p *lostCancelAcknowledgement) Cancel(ctx context.Context, c contracts.Cancel) error {
	if err := p.Port.Cancel(ctx, c); err != nil {
		return err
	}
	if !p.lost {
		p.lost = true
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestSecurityCancelLostAcknowledgementPartialStopsAndRestart(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "cancel-loss")
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(2, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{ResourceRef: "box", State: lifecycle.RunRunning, Sequence: 1, ObservedAt: f.at}))))
	f.cancel(t, s, "cancel-loss-command")
	p := &lostCancelAcknowledgement{Port: rt}
	f.tick(t, p)
	if !p.lost {
		t.Fatal("cancel transport loss not exercised")
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatal("transport acknowledgement inferred termination")
	}
	f.reopen(t)
	if _, err := f.pool.Exec(f.ctx, `update outbox set next_attempt_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	f.tick(t, p)
	if len(rt.Ledger().Cancelled()) != 2 {
		t.Fatal("lost cancel was not safely redelivered")
	}
	partial := f.event(rt, s, 1, simulator.RowsFrame(false, row(3, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true})))
	f.ingestNow(t, partial)
	f.ingestNow(t, partial)
	if task, attempt := f.states(t, s); task != lifecycle.TaskCancelling || attempt != lifecycle.AttemptCancellationRequested {
		t.Fatalf("partial termination=%s/%s", task, attempt)
	}
	if partial := f.reload(t, s.Nonce); !partial.Attempt.RuntimeStopped || partial.Attempt.EnvironmentStopped {
		t.Fatal("partial stops not retained")
	}
	f.reopen(t)
	stopped := f.event(rt, s, 1, simulator.RowsFrame(false, row(4, contracts.EntryEnvironmentObserved, contracts.EnvironmentObserved{ResourceRef: "box", State: lifecycle.RunAbsent, Sequence: 2, ObservedAt: f.at})))
	f.ingestNow(t, stopped)
	before := f.reload(t, s.Nonce)
	f.ingestNow(t, stopped)
	if after := f.reload(t, s.Nonce); after.Task != before.Task || after.Attempt != before.Attempt || after.Budget != before.Budget || after.Task.State != lifecycle.TaskCancelled || after.Attempt.State != lifecycle.AttemptTerminated {
		t.Fatalf("duplicate stop changed terminal state=%+v", after)
	}
	if f.count(t, "durable_event", "record_kind='budget' and event='release'") != 1 {
		t.Fatal("duplicate cancellation released more than once")
	}
}
