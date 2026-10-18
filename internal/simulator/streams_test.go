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

func TestStreamFrameBuildersProduceValidFrames(t *testing.T) {
	frames := []contracts.StreamFrame{
		simulator.ConnectedFrame(testNow),
		simulator.RowsFrame(false, simulator.Row{Sequence: 1, Updated: 1, Kind: "progress", Payload: `{"step":1}`}),
		simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 1, Updated: 2}),
		simulator.ClearedFrame(2),
		simulator.CursorTooOldFrame(),
		simulator.HeartbeatFrame(testNow.Add(time.Second)),
	}
	for i, frame := range frames {
		if err := frame.Validate(); err != nil {
			t.Errorf("frame %d Validate() error = %v", i, err)
		}
	}

	rows := simulator.RowsFrame(false, simulator.Row{Sequence: 1, Updated: 1, Kind: "progress", Payload: `{"step":1}`})
	entry := rows.Rows.Entries[0]
	if entry.PayloadHash != identity.DigestOf(entry.Payload) {
		t.Errorf("entry payload hash = %s, want the payload's digest", entry.PayloadHash)
	}

	outOfOrder := simulator.RowsFrame(false,
		simulator.Row{Sequence: 2, Updated: 2, Kind: "progress", Payload: `{"step":2}`},
		simulator.Row{Sequence: 1, Updated: 1, Kind: "progress", Payload: `{"step":1}`},
	)
	if err := outOfOrder.Validate(); err == nil {
		t.Error("out-of-order rows Validate() error = nil, want a refusal")
	}
}

func TestControlPausesBeforeStreamFrame(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	script := &simulator.StreamScript{Frames: []contracts.StreamFrame{
		simulator.ConnectedFrame(testNow),
		simulator.HeartbeatFrame(testNow.Add(time.Second)),
	}}
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{operation: {Stream: script}})
	cursor := contracts.Cursor{
		Tenant: config.Tenant, Attempt: mustID[ids.Attempt](t), Operation: operation,
		Stream: "runtime", Generation: 1,
	}
	stream, err := sim.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	pause := sim.Control().Pause(operation, simulator.BeforeFrame)
	var frame contracts.StreamFrame
	var nextErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		frame, nextErr = stream.Next(context.Background())
	}()
	<-pause.Reached()
	select {
	case <-done:
		t.Fatal("Next returned while paused before the frame")
	default:
	}
	pause.Release()
	<-done
	if nextErr != nil {
		t.Fatalf("Next() error = %v", nextErr)
	}
	if frame.Kind != contracts.StreamConnected {
		t.Fatalf("first frame kind = %s, want connected", frame.Kind)
	}

	next, err := stream.Next(context.Background())
	if err != nil || next.Kind != contracts.StreamHeartbeat {
		t.Fatalf("second frame = %+v, %v, want a heartbeat", next, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after the script = %v, want io.EOF", err)
	}
	subscriptions := sim.Ledger().Subscriptions()
	if len(subscriptions) != 1 || subscriptions[0] != cursor {
		t.Fatalf("Subscriptions() = %+v, want the opening cursor", subscriptions)
	}
}

func TestScriptedHistoryClassifiesThroughIngest(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	attempt := mustID[ids.Attempt](t)
	const stream = "runtime"
	progress := func(sequence, updated uint64, step string) simulator.Row {
		return simulator.Row{Sequence: sequence, Updated: updated, Kind: "progress", Payload: `{"step":"` + step + `"}`}
	}

	steps := []struct {
		name             string
		generation       uint64
		frame            contracts.StreamFrame
		entries          int
		tombstones       int
		duplicates       int
		replay           bool
		rehydrate        bool
		stale            bool
		after            uint64
		cursorGeneration uint64
	}{
		{
			name: "connected", generation: 1, frame: simulator.ConnectedFrame(testNow),
			after: 0, cursorGeneration: 1,
		},
		{
			name: "rows", generation: 1,
			frame:   simulator.RowsFrame(false, progress(1, 1, "one"), progress(2, 2, "two")),
			entries: 2, after: 2, cursorGeneration: 1,
		},
		{
			name: "heartbeat", generation: 1,
			frame: simulator.HeartbeatFrame(testNow.Add(time.Second)),
			after: 2, cursorGeneration: 1,
		},
		{
			name: "replayed rows", generation: 1,
			frame:   simulator.RowsFrame(true, progress(2, 2, "two"), progress(3, 3, "three")),
			entries: 1, duplicates: 1, replay: true, after: 3, cursorGeneration: 1,
		},
		{
			name: "tombstone", generation: 1,
			frame:      simulator.TombstonesFrame(false, simulator.Tombstone{Sequence: 2, Updated: 4}),
			tombstones: 1, after: 4, cursorGeneration: 1,
		},
		{
			name: "reordered revision", generation: 1,
			frame:      simulator.RowsFrame(false, progress(2, 3, "two-again")),
			duplicates: 1, after: 4, cursorGeneration: 1,
		},
		{
			name: "cleared generation", generation: 1, frame: simulator.ClearedFrame(2),
			rehydrate: true, after: 0, cursorGeneration: 2,
		},
		{
			name: "new generation rows", generation: 2,
			frame:   simulator.RowsFrame(false, progress(1, 1, "four")),
			entries: 1, after: 1, cursorGeneration: 2,
		},
		{
			name: "cursor too old", generation: 2, frame: simulator.CursorTooOldFrame(),
			rehydrate: true, after: 1, cursorGeneration: 2,
		},
		{
			name: "stale generation", generation: 1,
			frame: simulator.HeartbeatFrame(testNow.Add(2 * time.Second)),
			stale: true, after: 1, cursorGeneration: 2,
		},
	}
	frames := make([]contracts.StreamFrame, len(steps))
	for i, step := range steps {
		frames[i] = step.frame
	}

	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Stream: &simulator.StreamScript{Frames: frames}},
	})
	record, err := lifecycle.NewProviderOperation(operation, config.Tenant, attempt, config.Provider, testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	peer := contracts.Peer{Tenant: config.Tenant, Provider: config.Provider}
	cursor := contracts.Cursor{
		Tenant: config.Tenant, Attempt: attempt, Operation: operation,
		Stream: stream, Generation: 1, After: 0,
	}
	subscription, err := sim.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	for _, want := range steps {
		frame, err := subscription.Next(context.Background())
		if err != nil {
			t.Fatalf("step %q: Next() error = %v", want.name, err)
		}
		event := sim.Event(attempt, operation, stream, want.generation, frame)
		bound, failure := contracts.BindEvent(peer, event, record)
		if failure != nil {
			t.Fatalf("step %q: BindEvent() failure = %v", want.name, failure)
		}
		ingestion, failure := contracts.Ingest(cursor, bound)
		if failure != nil {
			t.Fatalf("step %q: Ingest() failure = %v", want.name, failure)
		}
		cursor = ingestion.Cursor
		if len(ingestion.Entries) != want.entries {
			t.Errorf("step %q: entries = %d, want %d", want.name, len(ingestion.Entries), want.entries)
		}
		if len(ingestion.Tombstones) != want.tombstones {
			t.Errorf("step %q: tombstones = %d, want %d", want.name, len(ingestion.Tombstones), want.tombstones)
		}
		if ingestion.Duplicates != want.duplicates {
			t.Errorf("step %q: duplicates = %d, want %d", want.name, ingestion.Duplicates, want.duplicates)
		}
		if ingestion.Replay != want.replay {
			t.Errorf("step %q: replay = %v, want %v", want.name, ingestion.Replay, want.replay)
		}
		if ingestion.Rehydrate != want.rehydrate {
			t.Errorf("step %q: rehydrate = %v, want %v", want.name, ingestion.Rehydrate, want.rehydrate)
		}
		if ingestion.Stale != want.stale {
			t.Errorf("step %q: stale = %v, want %v", want.name, ingestion.Stale, want.stale)
		}
		if cursor.After != want.after || cursor.Generation != want.cursorGeneration {
			t.Errorf("step %q: cursor = generation %d after %d, want generation %d after %d",
				want.name, cursor.Generation, cursor.After, want.cursorGeneration, want.after)
		}
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after the script = %v, want io.EOF", err)
	}
}

func TestMalformedFramesAreRefusedByTheConsumer(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	attempt := mustID[ids.Attempt](t)
	const stream = "runtime"

	badHash := contracts.StreamFrame{Kind: contracts.StreamRows, Rows: &contracts.Rows{Entries: []contracts.Entry{{
		Sequence: 1, UpdatedSequence: 1, Kind: "progress",
		PayloadHash: identity.DigestOf([]byte(`{"step":2}`)), Payload: json.RawMessage(`{"step":1}`),
	}}}}
	outOfOrder := simulator.RowsFrame(false,
		simulator.Row{Sequence: 2, Updated: 2, Kind: "progress", Payload: `{"step":2}`},
		simulator.Row{Sequence: 1, Updated: 1, Kind: "progress", Payload: `{"step":1}`},
	)

	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Stream: &simulator.StreamScript{Frames: []contracts.StreamFrame{badHash, outOfOrder}}},
	})
	record, err := lifecycle.NewProviderOperation(operation, config.Tenant, attempt, config.Provider, testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	peer := contracts.Peer{Tenant: config.Tenant, Provider: config.Provider}
	cursor := contracts.Cursor{
		Tenant: config.Tenant, Attempt: attempt, Operation: operation,
		Stream: stream, Generation: 1,
	}
	subscription, err := sim.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	for i, want := range []string{"payload hash mismatch", "entries out of order"} {
		frame, err := subscription.Next(context.Background())
		if err != nil {
			t.Fatalf("frame %d: Next() error = %v", i, err)
		}
		if err := frame.Validate(); err == nil {
			t.Errorf("frame %d (%s) validates, want a refusal", i, want)
		}
		event := sim.Event(attempt, operation, stream, 1, frame)
		if _, failure := contracts.BindEvent(peer, event, record); failure == nil || failure.Code != contracts.FailureMalformedFrame {
			t.Errorf("frame %d (%s): BindEvent() failure = %v, want malformed_frame", i, want, failure)
		}
	}
}
