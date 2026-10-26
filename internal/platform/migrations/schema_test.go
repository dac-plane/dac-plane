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
	want := []string{"00001_product_schema.sql", "00002_dispatch.sql", "00003_command_record.sql", "00004_attention_native.sql", "00005_governed_actions.sql", "00006_artifact_results.sql", "00007_migration_administration.sql", "00008_harness_migration_execution.sql", "00009_hook_stage_evidence.sql", "00010_actor_identity.sql", "00011_surface_events.sql", "00012_adapter_binding.sql", "00013_qualify_harness.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	if CurrentVersion != 13 {
		t.Fatalf("CurrentVersion = %d, want 13", CurrentVersion)
	}
}

func TestCheckServableBoundsTheReleaseRange(t *testing.T) {
	cases := []struct {
		version int64
		ok      bool
	}{{0, false}, {MinServeVersion - 1, false}, {MinServeVersion, true}, {CurrentVersion, true}, {CurrentVersion + 1, false}}
	for _, tc := range cases {
		if err := CheckServable(tc.version); (err == nil) != tc.ok {
			t.Errorf("CheckServable(%d) = %v, want servable %v", tc.version, err, tc.ok)
		}
	}
	if MinServeVersion > CurrentVersion {
		t.Fatal("MinServeVersion is above CurrentVersion")
	}
}
