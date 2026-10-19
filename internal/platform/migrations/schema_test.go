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
	want := []string{"00001_product_schema.sql", "00002_dispatch.sql", "00003_command_record.sql", "00004_attention_native.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	if CurrentVersion != 4 {
		t.Fatalf("CurrentVersion = %d, want 4", CurrentVersion)
	}
}
