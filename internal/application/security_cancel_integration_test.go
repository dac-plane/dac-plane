//go:build integration

package application_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// lostCancelAcknowledgement wraps the wire port the worker resolves. The
// wire protocol confirms a cancel by observation, never by a reply, so a
// lost acknowledgement is injected where the worker can observe it: the
// frame is written, the call then fails the way a connection lost
// mid-delivery would, and the worker must redeliver the intent.
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
	p := &lostCancelAcknowledgement{}
	tick := func() {
		t.Helper()
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "test-worker",
			wiretest.Wrap(t, f.pool, f.tenant, f.provider, rt, func(port contracts.Port) contracts.Port {
				p.Port = port
				return p
			}))
		if err != nil {
			t.Fatalf("worker.New() error = %v", err)
		}
		if err := w.Tick(f.ctx); err != nil {
			t.Fatalf("Tick() error = %v", err)
		}
	}
	tick()
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
	tick()
	// The adapter records a cancel asynchronously once the frame arrives.
	deadline := time.Now().Add(5 * time.Second)
	for len(rt.Ledger().Cancelled()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("lost cancel was not safely redelivered")
		}
		time.Sleep(2 * time.Millisecond)
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
