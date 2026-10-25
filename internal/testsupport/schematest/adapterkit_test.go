package schematest_test

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot is the repository root for schema paths; the test runs from its
// package directory.
const repoRoot = "../../../"

// TestEveryContractSchemaGuardsTheAdapterKit guards the adapter kit's embedded
// frame schemas byte-for-byte against contracts/jsonschema: the kit is the
// published ControlFrame contract for external adapters, and a silent drift
// between the two copies would let the kit and the control plane disagree
// about valid frames without any test noticing.
func TestEveryContractSchemaGuardsTheAdapterKit(t *testing.T) {
	pairs := []struct{ embedded, canonical string }{
		{repoRoot + "adapterkit/wire/schemas/adapter/cursor.schema.json", repoRoot + "contracts/jsonschema/adapter/cursor.schema.json"},
		{repoRoot + "adapterkit/wire/schemas/adapter/event.schema.json", repoRoot + "contracts/jsonschema/adapter/event.schema.json"},
		{repoRoot + "adapterkit/wire/schemas/adapter/frame.schema.json", repoRoot + "contracts/jsonschema/adapter/frame.schema.json"},
		{repoRoot + "adapterkit/wire/schemas/adapter/stream-frame.schema.json", repoRoot + "contracts/jsonschema/adapter/stream-frame.schema.json"},
		{repoRoot + "adapterkit/wire/schemas/adapter/common.schema.json", repoRoot + "contracts/jsonschema/adapter/common.schema.json"},
		{repoRoot + "adapterkit/wire/schemas/identity/common.schema.json", repoRoot + "contracts/jsonschema/identity/common.schema.json"},
	}
	for _, pair := range pairs {
		embedded, err := os.ReadFile(pair.embedded)
		if err != nil {
			t.Fatalf("reading %s: %v", pair.embedded, err)
		}
		canonical, err := os.ReadFile(pair.canonical)
		if err != nil {
			t.Fatalf("reading %s: %v", pair.canonical, err)
		}
		if string(embedded) != string(canonical) {
			t.Errorf("%s drifted from %s; copy the canonical schema and update the kit's embedded copy together", pair.embedded, pair.canonical)
		}
	}
	// No orphan copies: every file under the kit's schema directories is one
	// of the guarded pairs, so a new schema cannot ship unguarded.
	guarded := map[string]bool{
		"adapter/cursor.schema.json": true, "adapter/event.schema.json": true, "adapter/frame.schema.json": true,
		"adapter/stream-frame.schema.json": true, "adapter/common.schema.json": true, "identity/common.schema.json": true,
	}
	for _, dir := range []string{repoRoot + "adapterkit/wire/schemas/adapter", repoRoot + "adapterkit/wire/schemas/identity"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("listing %s: %v", dir, err)
		}
		for _, entry := range entries {
			rel := filepath.Base(dir) + "/" + entry.Name()
			if !guarded[rel] {
				t.Errorf("%s has no byte-identical guard; add it to TestEveryContractSchemaGuardsTheAdapterKit", dir+"/"+entry.Name())
			}
		}
	}
}
