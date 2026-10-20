package contracts

import (
	"path/filepath"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

func TestContractConformanceMigrationProtocol(t *testing.T) {
	source, target := newID[ids.Attempt](t), newID[ids.Attempt](t)
	request := MigrationRequest{Migration: newID[ids.Migration](t), Operation: newID[ids.Operation](t), SourceAttempt: source, TargetAttempt: target, Stage: MigrationTargetStarted, ClaimEpoch: 2, Recovery: MigrationRestartFromArtifacts, EnvironmentRef: "environment:target"}
	receipt := MigrationReceipt{Migration: request.Migration, Operation: request.Operation, SourceAttempt: source, TargetAttempt: target, Stage: request.Stage, ClaimEpoch: request.ClaimEpoch, Recovery: request.Recovery, EnvironmentRef: request.EnvironmentRef, SessionRef: "session:target", RecordedAt: testNow}
	for _, tc := range []struct {
		c schematest.Contract
		v any
	}{
		{schematest.GoContract[MigrationRequest](filepath.Join(schemaDir, "migration-request.schema.json")), request},
		{schematest.GoContract[MigrationReceipt](filepath.Join(schemaDir, "migration-receipt.schema.json")), receipt},
	} {
		raw := schematest.Marshal(t, tc.v)
		if err := tc.c.GoAccept(raw); err != nil {
			t.Fatal(err)
		}
		if err := schematest.Accepts(t, schematest.Compile(t, tc.c.Schema), raw); err != nil {
			t.Fatal(err)
		}
	}
	request.TargetAttempt = ids.AttemptID{}
	if request.Validate() == nil {
		t.Fatal("target restart without target Attempt accepted")
	}
}
