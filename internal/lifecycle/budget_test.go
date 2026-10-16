package lifecycle

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

func newBudget(t *testing.T, ceiling uint64) Budget {
	t.Helper()
	budget, err := NewBudget(newID[ids.Budget](t), newID[ids.Tenant](t), newID[ids.Task](t), "usd_micros", ceiling, testNow)
	if err != nil {
		t.Fatalf("NewBudget() error = %v", err)
	}
	return budget
}

func reserve(t *testing.T, b Budget, amount uint64) (Budget, Reservation) {
	t.Helper()
	next, reservation, _, err := b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), amount, b.Version, testNow)
	if err != nil {
		t.Fatalf("Reserve(%d) error = %v", amount, err)
	}
	return next, reservation
}

func usage(t *testing.T, b Budget, r Reservation, cumulative uint64) (Budget, Reservation, []Change) {
	t.Helper()
	nextBudget, nextReservation, changes, err := b.ReportUsage(r, cumulative, b.Version, testNow)
	if err != nil {
		t.Fatalf("ReportUsage(%d) error = %v", cumulative, err)
	}
	return nextBudget, nextReservation, changes
}

func TestReserveConsumeRelease(t *testing.T) {
	b, r := reserve(t, newBudget(t, 100), 60)
	if b.Reserved != 60 || b.Consumed != 0 || r.State != ReservationHeld {
		t.Fatalf("after reserve: budget %+v, reservation %+v", b, r)
	}
	b, r, _ = usage(t, b, r, 25)
	if b.Reserved != 35 || b.Consumed != 25 || r.Consumed != 25 {
		t.Fatalf("after usage: reserved %d consumed %d", b.Reserved, b.Consumed)
	}
	b, r, changes, err := b.Release(r, b.Version, testNow)
	if err != nil || len(changes) != 2 {
		t.Fatalf("Release() = %v, %v", changes, err)
	}
	if b.Reserved != 0 || b.Consumed != 25 || r.State != ReservationReleased {
		t.Fatalf("after release: budget %+v", b)
	}
	for _, change := range changes {
		if err := change.Audit(newID[ids.Audit](t), testSystem, testNow).Validate(); err != nil {
			t.Fatalf("release audit invalid: %v", err)
		}
	}
	_, _, _, err = b.Release(r, b.Version, testNow)
	testRejectCode(t, err, RejectTerminalState)
}

func TestCompetingReservationsCannotExceedCeiling(t *testing.T) {
	b := newBudget(t, 100)
	first, _, _, err := b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 70, b.Version, testNow)
	if err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	// A competitor that read the same version loses the compare-and-set.
	_, _, _, err = b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 70, b.Version, testNow)
	if err != nil {
		t.Fatalf("competitor on stale copy unexpectedly checked against old counts: %v", err)
	}
	_, _, _, err = first.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 70, b.Version, testNow)
	testRejectCode(t, err, RejectVersionConflict)
	// Retrying at the current version is refused by the ceiling.
	_, _, _, err = first.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 31, first.Version, testNow)
	testRejectCode(t, err, RejectCeilingReached)
	exact, _ := reserve(t, first, 30)
	if exact.State() != BudgetExhausted {
		t.Fatalf("budget at ceiling = %s, want exhausted", exact.State())
	}
	_, _, _, err = exact.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 1, exact.Version, testNow)
	testRejectCode(t, err, RejectCeilingReached)
}

func TestReplayedAndReorderedUsageDoNotDoubleConsume(t *testing.T) {
	b, r := reserve(t, newBudget(t, 100), 50)
	b, r, _ = usage(t, b, r, 30)
	for _, report := range []uint64{30, 10, 0} {
		next, nextReservation, changes := usage(t, b, r, report)
		if len(changes) != 0 || next != b || nextReservation != r {
			t.Fatalf("report %d changed state: %d changes", report, len(changes))
		}
	}
	b, _, _ = usage(t, b, r, 40)
	if b.Consumed != 40 || b.Reserved != 10 {
		t.Fatalf("after later cumulative report: consumed %d reserved %d", b.Consumed, b.Reserved)
	}
}

func TestInFlightAndLateUsageRecordOvershoot(t *testing.T) {
	b, r := reserve(t, newBudget(t, 100), 100)
	b, r, _ = usage(t, b, r, 120)
	if b.Consumed != 120 || b.Reserved != 0 || b.Overshoot != 20 || b.State() != BudgetExhausted {
		t.Fatalf("in-flight overshoot: %+v", b)
	}
	b, r, _, err := b.Release(r, b.Version, testNow)
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	b, _, _ = usage(t, b, r, 130)
	if b.Consumed != 130 || b.Overshoot != 30 {
		t.Fatalf("late usage after release: %+v", b)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate() after overshoot: %v", err)
	}
}

func TestReleaseReturnsOnlyUnusedHold(t *testing.T) {
	b, r := reserve(t, newBudget(t, 100), 40)
	b, r, _ = usage(t, b, r, 55)
	if b.Reserved != 0 {
		t.Fatalf("usage past the hold left %d reserved", b.Reserved)
	}
	b, _, _, err := b.Release(r, b.Version, testNow)
	if err != nil || b.Reserved != 0 || b.Consumed != 55 {
		t.Fatalf("Release() = %+v, %v", b, err)
	}
}

func TestBudgetRejectsForeignReservationAndBadAmounts(t *testing.T) {
	b, r := reserve(t, newBudget(t, 100), 10)
	other := newBudget(t, 100)
	_, _, _, err := other.ReportUsage(r, 5, other.Version, testNow)
	testRejectCode(t, err, RejectWrongRecord)
	_, _, _, err = other.Release(r, other.Version, testNow)
	testRejectCode(t, err, RejectWrongRecord)

	for name, call := range map[string]func() error{
		"zero reservation": func() error {
			_, _, _, err := b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), 0, b.Version, testNow)
			return err
		},
		"oversized reservation": func() error {
			_, _, _, err := b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), MaxAmount+1, b.Version, testNow)
			return err
		},
		"oversized usage": func() error { _, _, _, err := b.ReportUsage(r, MaxAmount+1, b.Version, testNow); return err },
		"zero ceiling": func() error {
			_, err := NewBudget(newID[ids.Budget](t), newID[ids.Tenant](t), newID[ids.Task](t), "tokens", 0, testNow)
			return err
		},
		"bad unit": func() error {
			_, err := NewBudget(newID[ids.Budget](t), newID[ids.Tenant](t), newID[ids.Task](t), "USD", 10, testNow)
			return err
		},
	} {
		if !errors.Is(call(), ErrInvalid) {
			t.Errorf("%s: want ErrInvalid", name)
		}
	}
}

func TestUsageCannotOverflowConsumed(t *testing.T) {
	b, r := reserve(t, newBudget(t, MaxAmount), MaxAmount)
	b, _, _ = usage(t, b, r, MaxAmount)
	// A second reservation's usage would push Consumed past MaxAmount.
	late := r
	late.ID, late.Consumed = newID[ids.Reservation](t), 0
	_, _, _, err := b.ReportUsage(late, 1, b.Version, testNow)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("usage beyond MaxAmount = %v, want ErrInvalid", err)
	}
}

// TestBudgetInvariantsUnderRandomSequences drives seeded random sequences of
// reserve, usage (including replays) and release, and checks after every step
// that the aggregate counts equal the sum over reservations, no count wrapped,
// and no reservation was granted past the ceiling.
func TestBudgetInvariantsUnderRandomSequences(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		b := newBudget(t, 1000)
		var reservations []Reservation
		for step := 0; step < 200; step++ {
			switch op := rng.IntN(3); {
			case op == 0 || len(reservations) == 0:
				before := b.committed()
				amount := uint64(rng.IntN(300) + 1)
				next, r, _, err := b.Reserve(newID[ids.Reservation](t), newID[ids.Attempt](t), amount, b.Version, testNow)
				if err == nil {
					if before+amount > b.Ceiling {
						t.Fatalf("seed %d: granted %d past ceiling with %d committed", seed, amount, before)
					}
					b, reservations = next, append(reservations, r)
				} else if RejectionOf(err) != RejectCeilingReached {
					t.Fatalf("seed %d: Reserve() error = %v", seed, err)
				}
			case op == 1:
				i := rng.IntN(len(reservations))
				cumulative := reservations[i].Consumed + uint64(rng.IntN(120))
				if rng.IntN(4) == 0 && reservations[i].Consumed > 0 {
					cumulative = uint64(rng.IntN(int(reservations[i].Consumed))) // replayed or reordered
				}
				next, r, _, err := b.ReportUsage(reservations[i], cumulative, b.Version, testNow)
				if err != nil {
					t.Fatalf("seed %d: ReportUsage() error = %v", seed, err)
				}
				b, reservations[i] = next, r
			default:
				i := rng.IntN(len(reservations))
				next, r, _, err := b.Release(reservations[i], b.Version, testNow)
				if err == nil {
					b, reservations[i] = next, r
				} else if RejectionOf(err) != RejectTerminalState {
					t.Fatalf("seed %d: Release() error = %v", seed, err)
				}
			}

			var reserved, consumed uint64
			for _, r := range reservations {
				reserved += r.outstanding()
				consumed += r.Consumed
			}
			if b.Reserved != reserved || b.Consumed != consumed {
				t.Fatalf("seed %d step %d: budget %d/%d, reservations %d/%d", seed, step, b.Reserved, b.Consumed, reserved, consumed)
			}
			if err := b.Validate(); err != nil {
				t.Fatalf("seed %d step %d: Validate() = %v", seed, step, err)
			}
		}
	}
}

func newSpendTerms(t *testing.T) SpendTerms {
	t.Helper()
	return SpendTerms{
		Tenant: newID[ids.Tenant](t), Principal: newID[ids.Principal](t), Task: newID[ids.Task](t),
		Attempt: newID[ids.Attempt](t), Operation: newID[ids.Operation](t),
		Amount: 2500, Unit: "usd_micros", Nonce: identity.Digest("sha256:" + strings.Repeat("c", 64)),
	}
}

func newSpend(t *testing.T) SpendChallenge {
	t.Helper()
	challenge, err := BeginSpend(newID[ids.Spend](t), newSpendTerms(t), testNow.Add(5*time.Minute), testNow)
	if err != nil {
		t.Fatalf("BeginSpend() error = %v", err)
	}
	return challenge
}

func TestSpendAssertionIssuesReceiptOnce(t *testing.T) {
	c := newSpend(t)
	assertion := SpendAssertion{Challenge: c.ID, Terms: c.Terms, Proof: testDigest, AssertedAt: testNow.Add(time.Minute)}
	asserted, receipt, change, err := c.Assert(assertion, c.Version)
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if asserted.State != SpendAsserted || receipt.Terms != c.Terms || receipt.Challenge != c.ID || receipt.Proof != testDigest {
		t.Fatalf("Assert() = %s, receipt %+v", asserted.State, receipt)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt Validate() = %v", err)
	}
	if err := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: c.Terms.Principal}, testNow).Validate(); err != nil {
		t.Fatalf("assert audit invalid: %v", err)
	}
	_, _, _, err = asserted.Assert(assertion, asserted.Version)
	testRejectCode(t, err, RejectTerminalState)
	_, _, _, err = asserted.Assert(assertion, c.Version)
	testRejectCode(t, err, RejectVersionConflict)
}

func TestSpendAssertionMustMatchEveryTerm(t *testing.T) {
	for name, mutate := range map[string]func(a *SpendAssertion){
		"challenge": func(a *SpendAssertion) { a.Challenge = newID[ids.Spend](t) },
		"tenant":    func(a *SpendAssertion) { a.Terms.Tenant = newID[ids.Tenant](t) },
		"principal": func(a *SpendAssertion) { a.Terms.Principal = newID[ids.Principal](t) },
		"task":      func(a *SpendAssertion) { a.Terms.Task = newID[ids.Task](t) },
		"attempt":   func(a *SpendAssertion) { a.Terms.Attempt = newID[ids.Attempt](t) },
		"operation": func(a *SpendAssertion) { a.Terms.Operation = newID[ids.Operation](t) },
		"amount":    func(a *SpendAssertion) { a.Terms.Amount++ },
		"unit":      func(a *SpendAssertion) { a.Terms.Unit = "tokens" },
		"nonce":     func(a *SpendAssertion) { a.Terms.Nonce = testDigest },
	} {
		t.Run(name, func(t *testing.T) {
			c := newSpend(t)
			assertion := SpendAssertion{Challenge: c.ID, Terms: c.Terms, Proof: testDigest, AssertedAt: testNow}
			mutate(&assertion)
			next, _, _, err := c.Assert(assertion, c.Version)
			testRejectCode(t, err, RejectAssertionMismatch)
			if next.State != SpendPending {
				t.Fatalf("mismatched Assert() moved challenge to %s", next.State)
			}
		})
	}
}

func TestSpendChallengeExpires(t *testing.T) {
	c := newSpend(t)
	late := SpendAssertion{Challenge: c.ID, Terms: c.Terms, Proof: testDigest, AssertedAt: c.ExpiresAt}
	_, _, _, err := c.Assert(late, c.Version)
	testRejectCode(t, err, RejectRequestExpired)

	_, _, err = c.Expire(c.Version, c.ExpiresAt.Add(-time.Second))
	testRejectCode(t, err, RejectInvalidTransition)
	expired, _, err := c.Expire(c.Version, c.ExpiresAt)
	if err != nil || expired.State != SpendExpired {
		t.Fatalf("Expire() = %s, %v", expired.State, err)
	}
	_, _, _, err = expired.Assert(SpendAssertion{Challenge: c.ID, Terms: c.Terms, Proof: testDigest, AssertedAt: testNow}, expired.Version)
	testRejectCode(t, err, RejectTerminalState)
}

func TestSpendAuthorizationDoesNotReplaceBudget(t *testing.T) {
	// A receipt for an amount within the authorized spend still cannot
	// reserve past the budget ceiling: the two are independent.
	c := newSpend(t)
	_, receipt, _, err := c.Assert(SpendAssertion{Challenge: c.ID, Terms: c.Terms, Proof: testDigest, AssertedAt: testNow}, c.Version)
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	b := newBudget(t, receipt.Terms.Amount-1)
	_, _, _, err = b.Reserve(newID[ids.Reservation](t), receipt.Terms.Attempt, receipt.Terms.Amount, b.Version, testNow)
	testRejectCode(t, err, RejectCeilingReached)
}

func TestBudgetValidateRequiresOvershootToCoverExcess(t *testing.T) {
	b := newBudget(t, 100)
	b.Consumed, b.Overshoot = 150, 50
	if err := b.Validate(); err != nil {
		t.Fatalf("overshoot equal to excess: %v", err)
	}
	b.Overshoot = 10
	if err := b.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overshoot below excess Validate() = %v, want ErrInvalid", err)
	}
}
