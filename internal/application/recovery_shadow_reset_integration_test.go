//go:build integration

package application_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestRecoveryShadowResetReopensLocalReplay(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"shadow", "off"} {
		for _, reset := range []string{"too_old", "cleared", "unavailable", "reset_loop"} {
			t.Run(mode+"/"+reset, func(t *testing.T) {
				f := newFixture(t, lifecycle.HarnessLive)
				s, rt := f.lost(t, "shadow-reset")
				f.ingest(t, rt.Event(s.Attempt.ID, s.Operation.ID, "dispatch", 1,
					simulator.RowsFrame(false, row(10, "progress", map[string]int{}))))
				if _, err := f.pool.Exec(f.ctx, `update harness_qualification set mode=$2,version=version+1 where tenant_id=$1`, f.tenant.String(), mode); err != nil {
					t.Fatal(err)
				}
				before := f.recoverySnapshot(t)
				frame := simulator.CursorTooOldFrame()
				generation := uint64(1)
				if reset == "cleared" {
					frame = simulator.ClearedFrame(2)
					generation = 2
				}
				for range 2 {
					f.reopen(t)
					// The wire port runs this subscribe closure on the
					// adapter kit's session goroutine; the unavailable path
					// returns before wire traffic can order the captured
					// writes against this goroutine's reads.
					var mu sync.Mutex
					var cursors []contracts.Cursor
					p := &recoveryPort{Port: rt, subscribe: func(_ context.Context, c contracts.Cursor) (contracts.Stream, error) {
						mu.Lock()
						cursors = append(cursors, c)
						first := len(cursors) == 1
						mu.Unlock()
						if first || reset == "reset_loop" {
							return &recoveryStream{frames: []contracts.StreamFrame{frame}}, nil
						}
						if reset == "unavailable" {
							return nil, errors.New("replay unavailable")
						}
						return &recoveryStream{frames: []contracts.StreamFrame{
							outcomeFrame(1), simulator.RowsFrame(true, row(2, "progress", map[string]int{"step": 2})),
						}}, nil
					}}
					if err := recoveryWorker(t, f, p).Reconcile(f.ctx); err != nil {
						t.Fatal(err)
					}
					wantSubscriptions := 2
					if mode == "off" {
						wantSubscriptions = 1
					}
					mu.Lock()
					subscriptions := append([]contracts.Cursor(nil), cursors...)
					mu.Unlock()
					if len(subscriptions) != wantSubscriptions || subscriptions[0].After != 10 || subscriptions[0].Generation != 1 {
						t.Fatalf("subscriptions=%+v", subscriptions)
					}
					if mode == "shadow" && (subscriptions[1].After != 0 || subscriptions[1].Generation != generation) {
						t.Fatalf("shadow replay cursor=%+v", subscriptions[1])
					}
					if !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
						t.Fatal("local replay changed primary state or durable cursor")
					}
				}
				wantEvidence := 3 // One reset and two replayed rows, deduplicated across restart.
				if reset == "unavailable" || reset == "reset_loop" {
					wantEvidence = 1
				}
				if mode == "off" {
					wantEvidence = 0
				}
				if got := f.count(t, "shadow_result", "true"); got != wantEvidence {
					t.Fatalf("shadow evidence=%d want=%d", got, wantEvidence)
				}
				if rt.Ledger().Effects(s.Operation.ID) != 1 || len(rt.Ledger().Sent()) != 1 {
					t.Fatal("comparison recovery repeated provider work")
				}
			})
		}
	}
}
