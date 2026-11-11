//go:build operationstest

package migrations

import (
	"io/fs"
	"slices"
	"testing"
)

func TestOperationsSourceAddsOnlyTheRolloutProbe(t *testing.T) {
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
	if len(names) != 18 || names[16] != "00017_agents.sql" || names[17] != "00018_rollout_probe.sql" {
		t.Fatalf("operations migrations = %v, want released migrations plus probe 18", names)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("operations migrations are not sorted: %v", names)
	}
	if CurrentVersion != 18 || MinServeVersion != 13 {
		t.Fatalf("operations range = %d..%d, want 13..18", MinServeVersion, CurrentVersion)
	}
	for _, version := range []int64{13, 14, 15, 16, 17} {
		if err := CheckServable(version); err != nil {
			t.Errorf("CheckServable(%d) = %v", version, err)
		}
	}
	for _, version := range []int64{12} {
		if err := CheckServable(version); err == nil {
			t.Errorf("CheckServable(%d) succeeded", version)
		}
	}
}
