package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// drain reads a stream until it ends and returns its frame kinds.
func drain(t *testing.T, rt *simulator.Runtime, cursor contracts.Cursor) []contracts.StreamKind {
	t.Helper()
	stream, err := rt.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var kinds []contracts.StreamKind
	for {
		frame, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return kinds
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, frame.Kind)
	}
}

func TestRuntimeDefaultScriptsUnknownOperationsAndDecisions(t *testing.T) {
	cfg := testConfig(t)
	connected := simulator.ConnectedFrame(time.Now().UTC())
	heartbeat := simulator.HeartbeatFrame(time.Now().UTC())
	rt, err := simulator.NewRuntime(cfg, simulator.RuntimeScenario{
		Default: &simulator.RuntimeOp{
			Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted,
			Stream:        &simulator.StreamScript{Frames: []contracts.StreamFrame{connected}},
			AfterDecision: &simulator.StreamScript{Frames: []contracts.StreamFrame{heartbeat}},
		},
		DefaultDecision: &simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := mustID[ids.Operation](t)
	start := newRequest(t, cfg, op, contracts.MethodStartTurn)
	if reply, err := rt.Send(context.Background(), start); err != nil || reply.Durability != lifecycle.SendAccepted {
		t.Fatalf("unscripted start = %+v, %v; want the default's acceptance", reply, err)
	}
	cursor := contracts.Cursor{Tenant: cfg.Tenant, Attempt: start.Attempt, Operation: op, Stream: "events", Generation: 1}
	if kinds := drain(t, rt, cursor); len(kinds) != 1 || kinds[0] != contracts.StreamConnected {
		t.Fatalf("stream before a decision = %v, want connected only", kinds)
	}

	steer := start
	steer.Method, steer.Delivery = contracts.MethodSteerTurn, mustID[ids.Delivery](t)
	steer.Arguments, _ = json.Marshal(map[string]string{"decision_id": mustID[ids.Decision](t).String(), "native_ref": "q-1"})
	if _, err := rt.Send(context.Background(), steer); err != nil {
		t.Fatalf("unscripted decision: %v", err)
	}
	if kinds := drain(t, rt, cursor); len(kinds) != 2 || kinds[1] != contracts.StreamHeartbeat {
		t.Fatalf("stream after a decision = %v, want the continuation appended", kinds)
	}
	if n := rt.Ledger().Effects(op); n != 2 {
		t.Fatalf("effects = %d, want the start and the steer", n)
	}

	other := mustID[ids.Operation](t)
	if kinds := drain(t, rt, contracts.Cursor{Tenant: cfg.Tenant, Attempt: start.Attempt, Operation: other, Stream: "events", Generation: 1}); len(kinds) != 1 {
		t.Fatalf("another operation's stream = %v, want no continuation without its own decision", kinds)
	}
}

func TestRuntimeWithoutDefaultStillRefusesUnscriptedWork(t *testing.T) {
	cfg := testConfig(t)
	rt := newRuntime(t, cfg, map[ids.OperationID]simulator.RuntimeOp{})
	if _, err := rt.Send(context.Background(), newRequest(t, cfg, mustID[ids.Operation](t), contracts.MethodStartTurn)); !errors.Is(err, simulator.ErrUnscripted) {
		t.Fatalf("unscripted start error = %v, want ErrUnscripted", err)
	}
	if _, err := simulator.NewRuntime(cfg, simulator.RuntimeScenario{DefaultDecision: &simulator.RuntimeOp{Effect: simulator.TurnStarted}}); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Fatalf("default decision with a start effect error = %v, want ErrInvalidScenario", err)
	}
}

func TestRuntimeDefaultBindsNativeReferencesToEachOperation(t *testing.T) {
	cfg := testConfig(t)
	row := simulator.Row{Sequence: 1, Updated: 1, Kind: contracts.EntryRuntimeStarted, Payload: `{"native_ref":"session-${operation_id}"}`}
	rt, err := simulator.NewRuntime(cfg, simulator.RuntimeScenario{Default: &simulator.RuntimeOp{
		Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted,
		Stream: &simulator.StreamScript{Frames: []contracts.StreamFrame{simulator.RowsFrame(false, row)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		operation := mustID[ids.Operation](t)
		stream, err := rt.Subscribe(context.Background(), contracts.Cursor{
			Tenant: cfg.Tenant, Attempt: mustID[ids.Attempt](t), Operation: operation, Stream: "events", Generation: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		frame, err := stream.Next(context.Background())
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		entry := frame.Rows.Entries[0]
		want := `{"native_ref":"session-` + operation.String() + `"}`
		if string(entry.Payload) != want || entry.PayloadHash != identity.DigestOf([]byte(want)) {
			t.Fatalf("bound entry = %+v, want payload %s and its digest", entry, want)
		}
	}
}
