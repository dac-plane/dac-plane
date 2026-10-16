package migrations

import (
	"io/fs"
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
	if len(names) != 2 || names[0] != "00001_product_schema.sql" || names[1] != "00002_dispatch.sql" {
		t.Fatalf("embedded migrations = %v, want [00001_product_schema.sql 00002_dispatch.sql]", names)
	}
	if CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", CurrentVersion)
	}
}
