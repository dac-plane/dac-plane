package migrations

import (
	"io/fs"
	"slices"
	"testing"
)

func TestEmbeddedMigrationsHoldCurrentVersion(t *testing.T) {
	entries, err := fs.ReadDir(Source, ".")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	want := []string{"00001_product_schema.sql", "00002_dispatch.sql", "00003_command_record.sql", "00004_attention_native.sql", "00005_governed_actions.sql", "00006_artifact_results.sql", "00007_migration_administration.sql", "00008_harness_migration_execution.sql", "00009_hook_stage_evidence.sql", "00010_actor_identity.sql", "00011_surface_events.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	if CurrentVersion != 11 {
		t.Fatalf("CurrentVersion = %d, want 11", CurrentVersion)
	}
}
