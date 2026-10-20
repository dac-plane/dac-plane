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

func TestMigrationCleanupRequestsAllowSourceOnlyAbortAndRequireTargetBinding(t *testing.T) {
	source := newID[ids.Attempt](t)
	migration := newID[ids.Migration](t)
	request := MigrationRequest{Migration: migration, Operation: newID[ids.Operation](t), SourceAttempt: source, Stage: MigrationSourceCleaned, ClaimEpoch: 1, EnvironmentRef: "environment:source"}
	contract := schematest.GoContract[MigrationRequest](filepath.Join(schemaDir, "migration-request.schema.json"))
	if err := request.Validate(); err != nil {
		t.Fatalf("source-only abort cleanup: %v", err)
	}
	raw := schematest.Marshal(t, request)
	if err := contract.GoAccept(raw); err != nil {
		t.Fatalf("Go schema agreement for source cleanup: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, contract.Schema), raw); err != nil {
		t.Fatalf("JSON schema for source cleanup: %v", err)
	}
	request.Stage = MigrationTargetEnvironmentCleaned
	if err := request.Validate(); err == nil {
		t.Fatal("target cleanup without target Attempt accepted")
	}
	request.TargetAttempt = newID[ids.Attempt](t)
	if err := request.Validate(); err != nil {
		t.Fatalf("target environment cleanup: %v", err)
	}
	receipt := MigrationReceipt{Migration: migration, Operation: request.Operation, SourceAttempt: source, TargetAttempt: request.TargetAttempt, Stage: request.Stage, ClaimEpoch: request.ClaimEpoch, EnvironmentRef: request.EnvironmentRef, RecordedAt: testNow}
	receiptContract := schematest.GoContract[MigrationReceipt](filepath.Join(schemaDir, "migration-receipt.schema.json"))
	receiptRaw := schematest.Marshal(t, receipt)
	if err := receiptContract.GoAccept(receiptRaw); err != nil {
		t.Fatalf("Go schema agreement for target cleanup receipt: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, receiptContract.Schema), receiptRaw); err != nil {
		t.Fatalf("JSON schema for target cleanup receipt: %v", err)
	}
}
