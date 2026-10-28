package contracts

import (
	"path/filepath"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

var runtimeStartContract = schematest.GoContract[RuntimeStart](filepath.Join(schemaDir, "runtime-start.schema.json"))

func TestRuntimeStartSchemaAgreement(t *testing.T) {
	valid := RuntimeStart{Brief: lifecycle.ExecutionBrief{Objective: "Update the fixture", AcceptanceCriteria: []string{"The fixture contains the requested value"}}}
	document := schematest.Marshal(t, valid)
	if err := runtimeStartContract.GoAccept(document); err != nil {
		t.Fatalf("Go rejected runtime start: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), document); err != nil {
		t.Fatalf("schema rejected runtime start: %v", err)
	}

	invalid := []RuntimeStart{
		{},
		{Brief: lifecycle.ExecutionBrief{Objective: "   ", AcceptanceCriteria: []string{"done"}}},
		{Brief: lifecycle.ExecutionBrief{Objective: "work", AcceptanceCriteria: []string{""}}},
	}
	for _, start := range invalid {
		data := schematest.Marshal(t, start)
		if runtimeStartContract.GoAccept(data) == nil {
			t.Fatalf("Go accepted invalid runtime start %s", data)
		}
		if schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data) == nil {
			t.Fatalf("schema accepted invalid runtime start %s", data)
		}
	}
}

func TestRuntimeStartMigrationRecoveryAgreement(t *testing.T) {
	migration, _ := ids.New[ids.Migration]()
	operation, _ := ids.New[ids.Operation]()
	source, _ := ids.New[ids.Attempt]()
	target, _ := ids.New[ids.Attempt]()
	brief := lifecycle.ExecutionBrief{Objective: "Continue the fixture task", AcceptanceCriteria: []string{"The original outcome is preserved"}}
	recovery := MigrationRequest{
		Migration: migration, Operation: operation, SourceAttempt: source, TargetAttempt: target,
		Stage: MigrationTargetStarted, ClaimEpoch: 2, Recovery: MigrationRestartFromArtifacts,
		ContextRefs: []string{"artifact:checkpoint"}, EnvironmentRef: "environment:target",
	}
	valid := RuntimeStart{Brief: brief, Recovery: &recovery}
	data := schematest.Marshal(t, valid)
	if err := runtimeStartContract.GoAccept(data); err != nil {
		t.Fatalf("Go rejected migration runtime start: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data); err != nil {
		t.Fatalf("schema rejected migration runtime start: %v", err)
	}

	invalid := valid
	bad := recovery
	bad.Stage = MigrationTargetEnvironmentReady
	bad.EnvironmentRef = ""
	invalid.Recovery = &bad
	data = schematest.Marshal(t, invalid)
	if runtimeStartContract.GoAccept(data) == nil {
		t.Fatal("Go accepted non-runtime recovery stage")
	}
	if schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data) == nil {
		t.Fatal("schema accepted non-runtime recovery stage")
	}
}
