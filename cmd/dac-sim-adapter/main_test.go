package main

import (
	"encoding/json"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
)

func TestScriptRowsNumberRowsAndFillFixtureDigests(t *testing.T) {
	rows, err := scriptRows([]scriptRow{
		{Kind: contracts.EntryRuntimeStarted, Payload: json.RawMessage("{\n  \"native_ref\": \"s-1\"\n}")},
		{Kind: contracts.EntryResultProposed, Payload: json.RawMessage(`{"session_ref":"s-1","turn_ref":"t-1","source_revision":"r","artifacts":[{"native_ref":"patch","kind":"code_change","media_type":"text/plain"}]}`)},
	}, 3, map[string]string{"patch": "exact bytes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Sequence != 3 || rows[1].Sequence != 4 || rows[1].Updated != 4 {
		t.Fatalf("rows = %+v, want sequences 3 and 4", rows)
	}
	if rows[0].Payload != `{"native_ref":"s-1"}` {
		t.Fatalf("first payload = %q, want compact JSON so its wire hash remains stable", rows[0].Payload)
	}
	var proposed contracts.ResultProposed
	if err := json.Unmarshal([]byte(rows[1].Payload), &proposed); err != nil {
		t.Fatal(err)
	}
	if got := proposed.Artifacts[0]; got.Digest != identity.DigestOf([]byte("exact bytes")) || got.Size != uint64(len("exact bytes")) {
		t.Fatalf("artifact = %+v, want the fixture's digest and size", got)
	}
}

func TestRuntimeScenarioDefaultsServeAnyOperation(t *testing.T) {
	var s scenarioFile
	if err := json.Unmarshal([]byte(`{"default":{"effect":"turn_started","rows":[{"kind":"runtime.started","payload":{"native_ref":"s-1"}}],"after_decision":[{"kind":"decision.consumed","payload":{"native_ref":"q-1"}}]}}`), &s); err != nil {
		t.Fatal(err)
	}
	scenario, err := s.runtimeScenario()
	if err != nil {
		t.Fatal(err)
	}
	if scenario.Default == nil || scenario.DefaultDecision == nil || scenario.Default.AfterDecision == nil {
		t.Fatalf("scenario = %+v, want a default, a default decision and a continuation", scenario)
	}
	if frames := scenario.Default.Stream.Frames; len(frames) != 2 || frames[0].Kind != contracts.StreamConnected || frames[1].Rows.Entries[0].Sequence != 1 {
		t.Fatalf("default stream = %+v", frames)
	}
	if after := scenario.Default.AfterDecision.Frames[0].Rows.Entries[0]; after.Sequence != 2 {
		t.Fatalf("continuation starts at %d, want 2", after.Sequence)
	}
}
