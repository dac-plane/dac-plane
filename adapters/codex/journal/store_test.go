package journal

import (
	"path/filepath"
	"testing"
)

func TestJournalPersistsMappingsAndStableEventIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, created, err := store.Begin(Operation{ID: "op-1", Tenant: "tenant-1", Attempt: "attempt-1", ArgumentsHash: Hash([]byte("brief"))})
	if err != nil || !created {
		t.Fatalf("begin=%+v created=%t err=%v", op, created, err)
	}
	if err = store.UpdateOperation("op-1", func(op *Operation) error { op.ThreadID = "thread-1"; op.TurnID = "turn-1"; return nil }); err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendEvent("op-1", "native-1", "progress", map[string]any{"step": 1})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.AppendEvent("op-1", "native-1", "progress", map[string]any{"step": 2})
	if err != nil || duplicate != first {
		t.Fatalf("duplicate=%d first=%d err=%v", duplicate, first, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err := store.Operation("op-1")
	if err != nil || restored.ThreadID != "thread-1" || restored.TurnID != "turn-1" {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	events, err := store.EventsAfter("op-1", 0)
	if err != nil || len(events) != 1 || events[0].Sequence != first {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestJournalRejectsChangedOperationPayload(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := Operation{ID: "op", Tenant: "tenant", Attempt: "attempt", ArgumentsHash: "sha256:a"}
	if _, _, err = store.Begin(base); err != nil {
		t.Fatal(err)
	}
	base.ArgumentsHash = "sha256:b"
	if _, _, err = store.Begin(base); err != ErrPayloadMismatch {
		t.Fatalf("err=%v", err)
	}
}
