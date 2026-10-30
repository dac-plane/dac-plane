//go:build integration

package application_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

type recoveryPort struct {
	contracts.Port
	send      func(context.Context, contracts.Request) (contracts.Reply, error)
	subscribe func(context.Context, contracts.Cursor) (contracts.Stream, error)
}

func (p *recoveryPort) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	if p.send != nil {
		return p.send(ctx, r)
	}
	return p.Port.Send(ctx, r)
}

func (p *recoveryPort) Subscribe(ctx context.Context, c contracts.Cursor) (contracts.Stream, error) {
	if p.subscribe != nil {
		return p.subscribe(ctx, c)
	}
	return p.Port.Subscribe(ctx, c)
}

type recoveryStream struct{ frames []contracts.StreamFrame }

func (s *recoveryStream) Next(context.Context) (contracts.StreamFrame, error) {
	if len(s.frames) == 0 {
		return contracts.StreamFrame{}, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}
func (*recoveryStream) Close() error { return nil }

func recoveryWorker(t *testing.T, f *fixture, p contracts.Port) *worker.Worker {
	t.Helper()
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "recovery", wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: p}))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	url := f.pool.Config().ConnString()
	f.pool.Close()
	p, err := store.Open(f.ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	f.pool = p
	// The adapter endpoint resolves bindings through the current pool, so a
	// restart that reopens it must hand the wire the new pool.
	_ = wiretest.For(t, f.pool)
	t.Cleanup(p.Close)
}

// snapshot captures all authoritative rows relevant to a no-mutation assertion.
func (f *fixture) recoverySnapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"task", "attempt", "provider_operation", "attention_request", "decision", "audit_event", "durable_event", "outbox", "stream_cursor", "artifact", "result_revision", "enforcement_receipt", "result_review", "command_record", "authority_grant", "authority_use", "budget", "reservation", "artifact_manifest", "artifact_verification", "revision_verification"} {
		var raw string
		if err := f.pool.QueryRow(f.ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb)::text from `+table+` r where tenant_id=$1`, f.tenant.String()).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out[table] = raw
	}
	return out
}

func outcomeFrame(seq uint64) contracts.StreamFrame {
	return simulator.RowsFrame(false, row(seq, contracts.EntryOperationOutcome, contracts.OperationOutcome{State: lifecycle.OperationSucceeded}))
}

func TestRecoveryRuntimeTransportLossNeverResends(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("loss"))
	f.dispatchPendingEnvironments(t)
	rt := f.runtime(t, s.Operation.ID)
	var mu sync.Mutex
	effects := 0
	replaying := false
	p := &recoveryPort{Port: rt, send: func(context.Context, contracts.Request) (contracts.Reply, error) {
		mu.Lock()
		effects++
		mu.Unlock()
		return contracts.Reply{}, io.ErrUnexpectedEOF
	}, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		mu.Lock()
		defer mu.Unlock()
		if !replaying {
			return nil, errors.New("transport still lost")
		}
		return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
	}}
	if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	if err != nil || op.State != lifecycle.OperationUncertain {
		t.Fatalf("lost reply state=%s err=%v", op.State, err)
	}
	f.reopen(t)
	if _, err := f.pool.Exec(f.ctx, `update outbox set next_attempt_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	w := recoveryWorker(t, f, p)
	if err := w.Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sent := effects
	mu.Unlock()
	if sent != 1 {
		t.Fatalf("uncertain effect sent %d times", sent)
	}
	mu.Lock()
	replaying = true
	mu.Unlock()
	if err := w.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	op, err = store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
	mu.Lock()
	sent = effects
	mu.Unlock()
	if err != nil || op.State != lifecycle.OperationSucceeded || sent != 1 {
		t.Fatalf("recovery state=%s effects=%d err=%v", op.State, sent, err)
	}
}

func TestRecoveryReconcileRejectsMalformedAndDuplicateOutcomes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bad_hash", "out_of_order", "duplicate", "omitted"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.lost(t, "loss")
			frame := outcomeFrame(1)
			switch kind {
			case "bad_hash":
				frame.Rows.Entries[0].PayloadHash = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			case "out_of_order":
				frame.Rows.Entries = append(frame.Rows.Entries, frame.Rows.Entries[0])
			case "omitted":
				frame.Rows.Entries[0].Payload = nil
				frame.Rows.Entries[0].PayloadOmitted = true
			case "duplicate":
				f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 2})))
			}
			before := f.recoverySnapshot(t)
			p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
				return &recoveryStream{frames: []contracts.StreamFrame{frame}}, nil
			}}
			if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
			if err != nil || op.State != lifecycle.OperationUncertain {
				t.Fatalf("invalid evidence settled operation=%s err=%v", op.State, err)
			}
			if kind != "omitted" {
				after := f.recoverySnapshot(t)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("invalid evidence mutated product state")
				}
			}
		})
	}
}

func TestRecoveryReconcileReopensLostCursor(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cleared", "too_old", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.lost(t, "loss")
			f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.RowsFrame(false, row(10, "progress", map[string]int{}))))
			f.reopen(t)
			// The wire port runs this subscribe closure on the adapter kit's
			// session goroutine, so the captured cursors need a lock: the
			// unavailable path returns before any wire traffic orders the
			// write against this goroutine's read.
			var mu sync.Mutex
			var seen []contracts.Cursor
			p := &recoveryPort{Port: rt, subscribe: func(_ context.Context, c contracts.Cursor) (contracts.Stream, error) {
				mu.Lock()
				seen = append(seen, c)
				first := len(seen) == 1
				mu.Unlock()
				if first {
					frame := simulator.CursorTooOldFrame()
					if kind == "cleared" {
						frame = simulator.ClearedFrame(2)
					}
					return &recoveryStream{frames: []contracts.StreamFrame{frame}}, nil
				}
				if kind == "unavailable" {
					return nil, errors.New("snapshot unavailable")
				}
				return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
			}}
			if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			subscriptions := append([]contracts.Cursor(nil), seen...)
			mu.Unlock()
			if len(subscriptions) != 2 || subscriptions[0].After != 10 || subscriptions[1].After != 0 {
				t.Fatalf("subscriptions=%+v", subscriptions)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, s.Operation.ID)
			want := lifecycle.OperationSucceeded
			if kind == "unavailable" {
				want = lifecycle.OperationUncertain
			}
			if err != nil || op.State != want {
				t.Fatalf("state=%s want=%s err=%v", op.State, want, err)
			}
			c, err := store.GetCursor(f.ctx, f.pool, f.tenant, s.Operation.ID, "dispatch")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "cleared" {
				if c.Generation != 2 || c.After != 1 {
					t.Fatalf("cursor=%+v", c)
				}
			} else if c.Generation != 1 || c.After != 10 {
				t.Fatalf("high water lost=%+v", c)
			}
			if rt.Ledger().Effects(s.Operation.ID) != 1 {
				t.Fatal("recovery repeated effect")
			}
		})
	}
}

func TestRecoveryShadowObservationsCannotSettlePrimary(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"ingest", "reconcile"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.lost(t, "loss")
			f.seed(t, func(u *store.UnitOfWork) error {
				_, err := u.Exec(f.ctx, `update harness_qualification set mode='shadow',version=version+1 where tenant_id=$1`, f.tenant.String())
				return err
			})
			before := f.recoverySnapshot(t)
			p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
				return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
			}}
			for i := range 2 {
				f.reopen(t)
				if path == "ingest" {
					frame := outcomeFrame(1)
					frame.Rows.Replay = i > 0
					f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, frame))
				} else if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
				t.Fatal("shadow changed authoritative state")
			}
			if n := f.count(t, "shadow_result", "true"); n != 1 {
				t.Fatalf("shadow observations=%d want=1", n)
			}
			if rt.Ledger().Effects(s.Operation.ID) != 1 {
				t.Fatal("shadow performed external effect")
			}
		})
	}
}

func TestRecoveryOffObservationsDoNotMutate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "off")
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := u.Exec(f.ctx, `update harness_qualification set mode='off' where tenant_id=$1`, f.tenant.String())
		return err
	})
	before := f.recoverySnapshot(t)
	f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, outcomeFrame(1)))
	p := &recoveryPort{Port: rt, subscribe: func(context.Context, contracts.Cursor) (contracts.Stream, error) {
		return &recoveryStream{frames: []contracts.StreamFrame{outcomeFrame(1)}}, nil
	}}
	if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.recoverySnapshot(t)) || f.count(t, "shadow_result", "true") != 0 {
		t.Fatal("off observations changed primary or comparison state")
	}
}

func TestRecoveryShadowEvidenceIsDigestOnlyAndBatchIndependent(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.lost(t, "shadow")
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := u.Exec(f.ctx, `update harness_qualification set mode='shadow' where tenant_id=$1`, f.tenant.String())
		return err
	})
	before := f.recoverySnapshot(t)
	rows := []simulator.Row{row(1, "message.delta", map[string]string{"private": "fixture-sensitive-value"}), row(2, "progress", map[string]int{"step": 2})}
	f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, simulator.RowsFrame(false, rows...)))
	f.reopen(t)
	for _, r := range rows {
		frame := simulator.RowsFrame(true, r)
		frame.ServerTime = time.Now()
		f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1, frame))
	}
	if f.count(t, "shadow_result", "true") != 2 || f.count(t, "shadow_result", "to_jsonb(shadow_result)::text like '%fixture-sensitive-value%'") != 0 {
		t.Fatal("shadow replay duplicated evidence or persisted private input")
	}
	if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
		t.Fatal("shadow comparison changed primary state")
	}
}

// An environment reports its operations through its resource, never an
// operation outcome. A lost provision or stop reply settles from that
// observation, and the observe loop then applies it through ingestion.
func TestRecoveryLostEnvironmentRepliesSettleFromObservations(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.submit(t, f.submitCommand("lost-environment"))
	environment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{
		Default: &simulator.EnvironmentOp{Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned, SendError: io.ErrUnexpectedEOF},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "lost-environment",
		wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.environmentProvider: environment}))
	if err != nil {
		t.Fatal(err)
	}
	settle := func(operation ids.OperationID) {
		t.Helper()
		if err := w.Tick(f.ctx); err != nil {
			t.Fatal(err)
		}
		if op, err := store.GetOperation(f.ctx, f.pool, f.tenant, operation); err != nil || op.State != lifecycle.OperationUncertain {
			t.Fatalf("operation after lost reply = %s, %v; want uncertain", op.State, err)
		}
		for range 2 {
			if err := w.Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err := w.Observe(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
		if op, err := store.GetOperation(f.ctx, f.pool, f.tenant, operation); err != nil || op.State != lifecycle.OperationAccepted {
			t.Fatalf("operation after reconciliation = %s, %v; want accepted", op.State, err)
		}
	}

	settle(s.Attempt.EnvironmentOperation)
	if _, attempt := f.states(t, s); attempt != lifecycle.AttemptStarting {
		t.Fatalf("attempt after recovered provision = %s, want starting", attempt)
	}
	if n := f.count(t, "outbox", "attempt_id = $2 and payload->>'role' = 'runtime' and payload->>'method' = 'start_turn'", s.Attempt.ID.String()); n != 1 {
		t.Fatalf("runtime starts after recovered provision = %d, want 1", n)
	}

	f.cancel(t, s, "lost-environment-cancel")
	settle(f.environmentStopOperation(t, s))
	if after := f.reload(t, s.Nonce); !after.Attempt.EnvironmentStopped {
		t.Fatalf("attempt after recovered stop = %+v, want the environment stop recorded", after.Attempt)
	}
}
