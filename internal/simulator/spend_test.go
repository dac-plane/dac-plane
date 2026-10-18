package simulator_test

import (
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSpendAssertionReplayIsRefused(t *testing.T) {
	tenant := mustID[ids.Tenant](t)
	principal := mustID[ids.Principal](t)
	at := testNow

	terms := func(nonce string) lifecycle.SpendTerms {
		return lifecycle.SpendTerms{
			Tenant: tenant, Principal: principal,
			Task: mustID[ids.Task](t), Attempt: mustID[ids.Attempt](t), Operation: mustID[ids.Operation](t),
			Amount: 100, Unit: "tokens", Nonce: identity.DigestOf([]byte(nonce)),
		}
	}
	begin := func(t *testing.T, nonce string) lifecycle.SpendChallenge {
		t.Helper()
		challenge, err := lifecycle.BeginSpend(mustID[ids.Spend](t), terms(nonce), at.Add(time.Hour), at)
		if err != nil {
			t.Fatalf("BeginSpend() error = %v", err)
		}
		return challenge
	}
	spender := &simulator.Spender{Tenant: tenant, Principal: principal}

	// One assertion authorizes exactly once: the receipt records the
	// authorized terms only, and a second assertion of the same challenge is
	// a replay the machine refuses.
	open := begin(t, "nonce-1")
	assertion, err := spender.Assert(open, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	asserted, receipt, change, err := open.Assert(assertion, open.Version)
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if asserted.State != lifecycle.SpendAsserted || asserted.Version != open.Version+1 {
		t.Fatalf("asserted challenge = %+v, want an advanced asserted state", asserted)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt does not validate: %v", err)
	}
	if receipt.Terms != open.Terms || receipt.Proof != assertion.Proof || !receipt.IssuedAt.Equal(assertion.AssertedAt) {
		t.Fatalf("receipt = %+v, want the authorized terms only", receipt)
	}
	if change.Record != lifecycle.RecordSpend || change.Event != "assert" || change.To != string(lifecycle.SpendAsserted) {
		t.Errorf("change = %+v, want the spend assertion recorded", change)
	}

	replay, err := spender.Assert(asserted, at.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if _, second, _, err := asserted.Assert(replay, asserted.Version); lifecycle.RejectionOf(err) != lifecycle.RejectTerminalState {
		t.Fatalf("second Assert() error = %v, want the terminal state", err)
	} else if second != (lifecycle.SpendReceipt{}) {
		t.Fatalf("a refused replay issued a second receipt: %+v", second)
	}

	// An answer at the expiry instant is too late.
	expiring := begin(t, "nonce-2")
	late := &simulator.Spender{Tenant: tenant, Principal: principal, Expired: true}
	stale, err := late.Assert(expiring, at)
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if !stale.AssertedAt.Equal(expiring.ExpiresAt) {
		t.Fatalf("expired assertion answered at %v, want the expiry instant", stale.AssertedAt)
	}
	if _, _, _, err := expiring.Assert(stale, expiring.Version); lifecycle.RejectionOf(err) != lifecycle.RejectRequestExpired {
		t.Fatalf("Assert() at expiry error = %v, want request_expired", err)
	}

	// Terms that drifted from the challenge are a mismatch.
	level := begin(t, "nonce-3")
	drift := &simulator.Spender{Tenant: tenant, Principal: principal, Drift: true}
	off, err := drift.Assert(level, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if off.Terms == level.Terms {
		t.Fatalf("drifted assertion kept the challenge terms")
	}
	if _, _, _, err := level.Assert(off, level.Version); lifecycle.RejectionOf(err) != lifecycle.RejectAssertionMismatch {
		t.Fatalf("drifted Assert() error = %v, want assertion_mismatch", err)
	}

	// A spender that replays its previous assertion answers the next
	// challenge with the previous one, which is a mismatch for it.
	first := begin(t, "nonce-4")
	next := begin(t, "nonce-5")
	played := &simulator.Spender{Tenant: tenant, Principal: principal, Played: true}
	previous, err := played.Assert(first, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	reused, err := played.Assert(next, at.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Assert() error = %v", err)
	}
	if reused != previous {
		t.Fatalf("played assertion = %+v, want the reused %+v", reused, previous)
	}
	if _, _, _, err := next.Assert(reused, next.Version); lifecycle.RejectionOf(err) != lifecycle.RejectAssertionMismatch {
		t.Fatalf("replayed Assert() error = %v, want assertion_mismatch", err)
	}

	// A challenge for another principal is refused before any ceremony.
	foreign := &simulator.Spender{Tenant: tenant, Principal: mustID[ids.Principal](t)}
	if _, err := foreign.Assert(begin(t, "nonce-6"), at); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Fatalf("Assert() for another principal error = %v, want an invalid scenario", err)
	}
}
