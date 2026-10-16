package worker

import (
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

func mustRowID(t *testing.T) ids.AnyID {
	t.Helper()
	id, err := ids.New[ids.Any]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func TestSchedulerPersonFirstBackgroundBounded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := func(demand contracts.Demand, due time.Time) store.OutboxRow {
		return store.OutboxRow{ID: mustRowID(t), Demand: demand, NextTryAt: due}
	}
	due := now
	later := now.Add(time.Minute)
	ready := []store.OutboxRow{
		row(contracts.DemandBackground, due),
		row(contracts.DemandBackground, due),
		row(contracts.DemandBackground, due),
		row(contracts.DemandPerson, later),
		row("", due), // pre-#21 row without demand sorts as background
	}
	selected := Scheduler{BackgroundBudget: 2}.Select(ready)
	if len(selected) != 3 {
		t.Fatalf("Select() = %d rows, want 3 (1 person + 2 background)", len(selected))
	}
	if selected[0].Demand != contracts.DemandPerson {
		t.Fatal("Select() did not put the person row first")
	}
	for _, row := range selected[1:] {
		if row.Demand == contracts.DemandPerson {
			t.Fatal("Select() put a person row outside the head")
		}
		if !row.NextTryAt.Equal(due) {
			t.Fatal("Select() preferred a later background row over an earlier one")
		}
	}
}

func TestSchedulerZeroBudgetTakesNoBackground(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	ready := []store.OutboxRow{
		{ID: mustRowID(t), Demand: contracts.DemandBackground, NextTryAt: now},
		{ID: mustRowID(t), Demand: contracts.DemandPerson, NextTryAt: now},
	}
	selected := Scheduler{}.Select(ready)
	if len(selected) != 1 || selected[0].Demand != contracts.DemandPerson {
		t.Fatalf("Select() with zero budget = %v, want the person row only", selected)
	}
}

func TestExhaustionTerminalCodesExhaustImmediately(t *testing.T) {
	policy := ExhaustionPolicy{MaxAttempts: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute}
	terminal := []contracts.FailureCode{
		contracts.FailureProtocolMismatch, contracts.FailureUnsupportedCapability,
		contracts.FailureMalformedFrame, contracts.FailureUnknownOperation,
		contracts.FailureInvalidArguments, contracts.FailureUnauthenticated,
		contracts.FailureUnauthorized, contracts.FailureCancelled,
		contracts.FailureStalePrecondition,
		contracts.FailureCode("unknown_future_code"),
	}
	for _, code := range terminal {
		if _, exhausted := policy.Classify(0, code); !exhausted {
			t.Fatalf("Classify(0, %q) = retry, want exhausted", code)
		}
	}
}

func TestExhaustionTransientCodesBackOffThenGiveUp(t *testing.T) {
	policy := ExhaustionPolicy{MaxAttempts: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute}
	transient := []contracts.FailureCode{
		contracts.FailureDeadlineExceeded, contracts.FailureTransportUnavailable,
		contracts.FailureProviderFailed, contracts.FailureResourceBusy,
	}
	for _, code := range transient {
		retryIn, exhausted := policy.Classify(0, code)
		if exhausted {
			t.Fatalf("Classify(0, %q) = exhausted, want retry", code)
		}
		if retryIn < time.Second || retryIn > 2*time.Second {
			t.Fatalf("Classify(0, %q) backoff = %v, want within [1s, 2s]", code, retryIn)
		}
		if _, exhausted := policy.Classify(2, code); exhausted {
			t.Fatalf("Classify(2, %q) = exhausted, want retry", code)
		}
		if _, exhausted := policy.Classify(3, code); !exhausted {
			t.Fatalf("Classify(3, %q) = retry, want exhausted at the budget", code)
		}
	}
}

func TestExhaustionBackoffCapsAtMax(t *testing.T) {
	policy := ExhaustionPolicy{MaxAttempts: 100, BaseBackoff: time.Minute, MaxBackoff: 90 * time.Second}
	retryIn, exhausted := policy.Classify(10, contracts.FailureProviderFailed)
	if exhausted {
		t.Fatal("Classify() = exhausted, want retry")
	}
	if retryIn < 90*time.Second || retryIn > 112500*time.Millisecond {
		t.Fatalf("Classify() backoff = %v, want within [90s, 112.5s]", retryIn)
	}
}

func TestDefaultExhaustionPolicyMatchesSpecDefault(t *testing.T) {
	policy := DefaultExhaustionPolicy()
	if policy.MaxAttempts != 3 {
		t.Fatalf("DefaultExhaustionPolicy().MaxAttempts = %d, want 3", policy.MaxAttempts)
	}
	if _, exhausted := policy.Classify(3, contracts.FailureProviderFailed); !exhausted {
		t.Fatal("default policy did not exhaust at 3 attempts")
	}
}
