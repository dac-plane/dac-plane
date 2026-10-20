package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestRecoveryRuntimeDistinctDecisionsAndChangedReplay(t *testing.T) {
	cfg := testConfig(t)
	op := mustID[ids.Operation](t)
	first, second := mustID[ids.Decision](t), mustID[ids.Decision](t)
	rt, err := simulator.NewRuntime(cfg, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{op: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}},
		Decisions:  map[ids.DecisionID]simulator.RuntimeOp{first: {Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered}, second: {Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRequest(t, cfg, op, contracts.MethodStartTurn)
	if _, err := rt.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Method = contracts.MethodSteerTurn
	for _, d := range []ids.DecisionID{first, first, second, second} {
		r.Arguments, _ = json.Marshal(map[string]string{"decision_id": d.String(), "native_ref": "question"})
		r.Delivery = mustID[ids.Delivery](t)
		if _, err := rt.Send(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if n := rt.Ledger().Effects(op); n != 3 {
		t.Fatalf("effects=%d want=start+two steers", n)
	}
	r.Arguments, _ = json.Marshal(map[string]string{"decision_id": second.String(), "native_ref": "changed"})
	if _, err := rt.Send(context.Background(), r); !errors.Is(err, simulator.ErrMalformedRequest) {
		t.Fatalf("changed decision replay err=%v", err)
	}
	if n := rt.Ledger().Effects(op); n != 3 {
		t.Fatalf("changed replay effects=%d", n)
	}
}
