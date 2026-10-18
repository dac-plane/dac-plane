package contracts

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

var (
	runtimeStartedContract = schematest.GoContract[RuntimeStarted](filepath.Join(schemaDir, "entry-runtime-started.schema.json"))
	turnStartedContract    = schematest.GoContract[TurnStarted](filepath.Join(schemaDir, "entry-turn-started.schema.json"))
	turnFinishedContract   = schematest.GoContract[TurnFinished](filepath.Join(schemaDir, "entry-turn-finished.schema.json"))
	observedContract       = schematest.GoContract[EnvironmentObserved](filepath.Join(schemaDir, "entry-environment-observed.schema.json"))
	outcomeContract        = schematest.GoContract[OperationOutcome](filepath.Join(schemaDir, "entry-operation-outcome.schema.json"))
)

func TestClassifyEntryFollowsTheSpecification(t *testing.T) {
	for kind, want := range map[string]EntryClass{
		EntryRuntimeStarted:      EntryApply,
		EntryTurnFinished:        EntryApply,
		EntryEnvironmentObserved: EntryApply,
		EntryOperationOutcome:    EntryApply,
		"progress":               EntryProgress,
		"message.delta":          EntryProgress,
		"tool.finished":          EntryProgress,
		"custom.metric":          EntryProgress,
		"attempt.completed":      EntryRefuse,
		"task.succeeded":         EntryRefuse,
		"action.approved":        EntryRefuse,
		"environment.destroyed":  EntryRefuse,
		"tool.approved":          EntryRefuse,
		"scope.grant":            EntryRefuse,
		"permission.changed":     EntryRefuse,
	} {
		if got := ClassifyEntry(kind); got != want {
			t.Errorf("ClassifyEntry(%q) = %s, want %s", kind, got, want)
		}
	}
}

func entryFor(t *testing.T, kind string, payload any) Entry {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return Entry{Sequence: 1, UpdatedSequence: 1, Kind: kind, Payload: data, PayloadHash: identity.DigestOf(data)}
}

func TestDecodeEntryRejectsSmuggledFieldsAndBadPayloads(t *testing.T) {
	if _, err := DecodeEntry[RuntimeStarted](entryFor(t, EntryRuntimeStarted, RuntimeStarted{NativeRef: "s-1"})); err != nil {
		t.Fatalf("valid runtime.started: %v", err)
	}
	smuggled := entryFor(t, EntryRuntimeStarted, map[string]any{"native_ref": "s-1", "attempt_id": "another"})
	omitted := Entry{Sequence: 1, UpdatedSequence: 1, Kind: EntryRuntimeStarted, PayloadHash: identity.DigestOf([]byte("x")), PayloadOmitted: true}
	huge := entryFor(t, EntryRuntimeStarted, RuntimeStarted{NativeRef: strings.Repeat("s", 10)})
	huge.Payload = []byte(`{"native_ref":"` + strings.Repeat("s", MaxInlinePayload) + `"}`)
	for name, entry := range map[string]Entry{"identity smuggled": smuggled, "payload omitted": omitted, "over the inline limit": huge} {
		if _, err := DecodeEntry[RuntimeStarted](entry); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: DecodeEntry() = %v, want ErrInvalid", name, err)
		}
	}
}

func entryFixtures(t *testing.T) map[string]fixture {
	t.Helper()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	failed := TurnFinished{
		SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnFailed, EndedAt: now,
		Failure: &lifecycle.TurnFailure{Code: lifecycle.TurnFailureToolFailed, Summary: "tool exited 1", DetailsDigest: identity.DigestOf([]byte("trace"))},
	}
	return map[string]fixture{
		"runtime started": {runtimeStartedContract, schematest.Marshal(t, RuntimeStarted{NativeRef: "s-1"})},
		"turn started":    {turnStartedContract, schematest.Marshal(t, TurnStarted{NativeRef: "t-1", SessionRef: "s-1"})},
		"turn completed":  {turnFinishedContract, schematest.Marshal(t, TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: now})},
		"turn failed":     {turnFinishedContract, schematest.Marshal(t, failed)},
		"observed":        {observedContract, schematest.Marshal(t, EnvironmentObserved{ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 2, ObservedAt: now})},
		"outcome":         {outcomeContract, schematest.Marshal(t, OperationOutcome{State: lifecycle.OperationSucceeded})},
	}
}

func TestContractConformanceEntryPayloads(t *testing.T) {
	fixtures := entryFixtures(t)
	for name, f := range fixtures {
		if err := f.contract.GoAccept(f.data); err != nil {
			t.Errorf("%s: Go rejected its own fixture: %v", name, err)
		}
		if err := schematest.Accepts(t, schematest.Compile(t, f.contract.Schema), f.data); err != nil {
			t.Errorf("%s: schema rejected Go fixture %s: %v", name, f.data, err)
		}
	}
	for _, tc := range []struct {
		name, base string
		change     func(map[string]any)
	}{
		{"runtime started smuggling an attempt", "runtime started", func(d map[string]any) { d["attempt_id"] = "x" }},
		{"runtime started empty ref", "runtime started", func(d map[string]any) { d["native_ref"] = "" }},
		{"turn started without session", "turn started", func(d map[string]any) { delete(d, "session_ref") }},
		{"failed turn without failure", "turn failed", func(d map[string]any) { delete(d, "failure") }},
		{"completed turn with failure", "turn failed", func(d map[string]any) { d["outcome"] = "completed" }},
		{"turn unknown outcome", "turn completed", func(d map[string]any) { d["outcome"] = "idle" }},
		{"observed unknown state", "observed", func(d map[string]any) { d["state"] = "paused" }},
		{"observed zero sequence", "observed", func(d map[string]any) { d["sequence"] = 0 }},
		{"outcome uncertain", "outcome", func(d map[string]any) { d["state"] = "uncertain" }},
	} {
		base := fixtures[tc.base]
		data := schematest.Mutate(t, base.data, tc.change)
		if err := schematest.Accepts(t, schematest.Compile(t, base.contract.Schema), data); err == nil {
			t.Errorf("%s: schema accepted %s", tc.name, data)
		}
		if err := base.contract.GoAccept(data); err == nil {
			t.Errorf("%s: Go accepted %s", tc.name, data)
		}
	}
}
