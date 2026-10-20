package contracts

import (
	"path/filepath"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

func TestContractConformanceUsageReported(t *testing.T) {
	c := schematest.GoContract[UsageReported](filepath.Join(schemaDir, "entry-usage-reported.schema.json"))
	schema := schematest.Compile(t, c.Schema)
	for _, raw := range []string{`{"unit":"tokens","cumulative":0}`, `{"unit":"usd_micros","cumulative":9007199254740991}`} {
		if err := c.GoAccept([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := schematest.Accepts(t, schema, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`{}`, `{"unit":"tokens"}`, `{"unit":"tokens","cumulative":null}`,
		`{"unit":"tokens","cumulative":-1}`, `{"unit":"tokens","cumulative":0.5}`,
		`{"unit":"tokens","cumulative":9007199254740992}`, `{"unit":"USD","cumulative":1}`,
		`{"unit":"tokens","cumulative":1,"attempt_id":"forged"}`,
	} {
		if c.GoAccept([]byte(raw)) == nil || schematest.Accepts(t, schema, []byte(raw)) == nil {
			t.Fatalf("invalid usage accepted: %s", raw)
		}
	}
	if ClassifyEntry(EntryUsageReported) != EntryApply || ClassifyEntry("usage.reset") != EntryRefuse {
		t.Fatal("usage bypasses accounting classification")
	}
}
