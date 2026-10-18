package simulator_test

import (
	"context"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestShadowComparisonUsesEffectLogs(t *testing.T) {
	first := mustID[ids.Operation](t)
	second := mustID[ids.Operation](t)
	third := mustID[ids.Operation](t)

	effect := func(operation ids.OperationID, kind simulator.EffectKind) simulator.Effect {
		return simulator.Effect{Operation: operation, Kind: kind}
	}

	cases := []struct {
		name    string
		primary []simulator.Effect
		shadow  []simulator.Effect
		want    simulator.Comparison
	}{
		{
			name:    "every pair matches",
			primary: []simulator.Effect{effect(first, simulator.TurnStarted)},
			shadow:  []simulator.Effect{effect(first, simulator.TurnStarted)},
			want:    simulator.Comparison{Matched: 1},
		},
		{
			name:    "a kind differs",
			primary: []simulator.Effect{effect(first, simulator.TurnStarted)},
			shadow:  []simulator.Effect{effect(first, simulator.TurnSteered)},
			want:    simulator.Comparison{Mismatched: 1},
		},
		{
			name: "the shadow missed one of two",
			primary: []simulator.Effect{
				effect(first, simulator.TurnStarted),
				effect(first, simulator.TurnSteered),
			},
			shadow: []simulator.Effect{effect(first, simulator.TurnStarted)},
			want:   simulator.Comparison{Matched: 1, Missing: 1},
		},
		{
			name: "operations only one side applied",
			primary: []simulator.Effect{
				effect(first, simulator.TurnStarted),
				effect(second, simulator.EnvironmentProvisioned),
			},
			shadow: []simulator.Effect{
				effect(first, simulator.TurnStarted),
				effect(third, simulator.EnvironmentProvisioned),
			},
			want: simulator.Comparison{Matched: 1, Missing: 1, Extra: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := simulator.Compare(tc.primary, tc.shadow)
			if got != tc.want {
				t.Fatalf("Compare() = %+v, want %+v", got, tc.want)
			}
			wantBalanced := tc.want.Mismatched == 0 && tc.want.Missing == 0 && tc.want.Extra == 0
			if got.Balanced() != wantBalanced {
				t.Fatalf("Balanced() = %v, want %v", got.Balanced(), wantBalanced)
			}
			if got.Digest() != identity.DigestOf([]byte(got.Summary())) {
				t.Fatalf("Digest() = %q, want the digest of %q", got.Digest(), got.Summary())
			}
			if again := simulator.Compare(tc.primary, tc.shadow); again != got {
				t.Fatalf("Compare() twice = %+v, then %+v; want the same comparison", got, again)
			}
		})
	}
}

func TestShadowComparisonAgainstAWithheldSimulator(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)

	primary := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})
	if _, err := primary.Send(context.Background(), newRequest(t, config, operation, contracts.MethodStartTurn)); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// A withheld shadow is scripted but never driven: it applies nothing.
	withheld := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		mustID[ids.Operation](t): {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned},
	})
	if applied := withheld.Ledger().Applied(); len(applied) != 0 {
		t.Fatalf("withheld shadow applied %d effects, want 0", len(applied))
	}

	comparison := simulator.Compare(primary.Ledger().Applied(), withheld.Ledger().Applied())
	if comparison != (simulator.Comparison{Missing: 1}) {
		t.Fatalf("Compare() = %+v, want one missing effect", comparison)
	}
	if comparison.Balanced() {
		t.Fatalf("Balanced() = true against a withheld shadow, want false")
	}
	if again := simulator.Compare(primary.Ledger().Applied(), withheld.Ledger().Applied()); again != comparison {
		t.Fatalf("Compare() twice = %+v, then %+v; want the same comparison", comparison, again)
	}
	if comparison.Digest() != identity.DigestOf([]byte(comparison.Summary())) {
		t.Fatalf("Digest() = %q, want the digest of %q", comparison.Digest(), comparison.Summary())
	}
}
