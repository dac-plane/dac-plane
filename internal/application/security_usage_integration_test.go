//go:build integration

package application_test

import (
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSecurityUsageLateReportsRemainAccountedAfterTerminal(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "security-usage")
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(2, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)))))
	before := f.reload(t, s.Nonce)
	if before.Attempt.State != lifecycle.AttemptFailed || before.Reservation.State != lifecycle.ReservationReleased {
		t.Fatalf("terminal setup = %+v", before)
	}
	frame := simulator.RowsFrame(false, row(3, "usage.reported", map[string]any{"unit": s.Unit, "cumulative": 1200}))
	got := f.ingestNow(t, f.event(rt, s, 1, frame))
	if got.Applied != 1 {
		t.Fatalf("late usage = %+v, want accounted", got)
	}
	after := f.reload(t, s.Nonce)
	if after.Budget.Consumed != 1200 || after.Budget.Overshoot != 200 || after.Budget.Reserved != 0 || after.Reservation.State != lifecycle.ReservationReleased || after.Task != before.Task || after.Attempt != before.Attempt {
		t.Fatalf("late usage revived work or lost accounting: %+v", after)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, frame)); got.Duplicates != 1 {
		t.Fatalf("same cursor replay = %+v", got)
	}
	// A second observation of the same cumulative total also has no effect.
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(4, "usage.reported", map[string]any{"unit": s.Unit, "cumulative": 1200}))))
	if replay := f.reload(t, s.Nonce); replay.Budget != after.Budget || replay.Reservation != after.Reservation {
		t.Fatalf("cumulative replay changed accounting: %+v", replay)
	}
	if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(5, "usage.reported", map[string]any{"unit": s.Unit, "cumulative": 1100})))); got.Progress != 1 {
		t.Fatalf("older cumulative observation = %+v, want progress only", got)
	}
	for i, payload := range []map[string]any{
		{"unit": "wrong_unit", "cumulative": 1300},
		{"unit": s.Unit, "cumulative": 1300, "attempt_id": s.Attempt.ID.String()},
	} {
		if got := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(uint64(6+i), "usage.reported", payload)))); got.Refused != 1 {
			t.Fatalf("invalid usage %+v = %+v", payload, got)
		}
	}
	if rejected := f.reload(t, s.Nonce); rejected.Budget != after.Budget || rejected.Reservation != after.Reservation {
		t.Fatalf("invalid usage changed accounting: %+v", rejected)
	}
	if n := f.count(t, "durable_event", "record_kind='budget' and event='consume'"); n != 1 {
		t.Fatalf("usage events = %d, want one", n)
	}
}

func TestSecurityUsageNonLiveReportsCannotChangeAccounting(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "security-shadow-usage")
	before := f.reload(t, s.Nonce)
	cursor, err := store.GetCursor(f.ctx, f.pool, f.tenant, s.Operation.ID, eventStream)
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []lifecycle.HarnessMode{lifecycle.HarnessShadow, lifecycle.HarnessOff} {
		f.seed(t, func(u *store.UnitOfWork) error {
			_, err := store.UpsertHarness(f.ctx, u, lifecycle.HarnessQualification{Tenant: f.tenant, Provider: f.provider, Mode: mode, Version: uint64(i + 2), UpdatedAt: f.at})
			return err
		})
		f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
			row(uint64(2+i), "usage.reported", map[string]any{"unit": s.Unit, "cumulative": 1200}))))
		if after := f.reload(t, s.Nonce); after.Budget != before.Budget || after.Reservation != before.Reservation || after.Attempt != before.Attempt || after.Task != before.Task {
			t.Fatalf("%s usage changed primary state: %+v", mode, after)
		}
		if after, err := store.GetCursor(f.ctx, f.pool, f.tenant, s.Operation.ID, eventStream); err != nil || after != cursor {
			t.Fatalf("%s usage advanced primary cursor: %+v, %v", mode, after, err)
		}
	}
}
