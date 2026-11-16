package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/testsupport/schematest"
)

var (
	runtimeStartContract         = schematest.GoContract[RuntimeStart](filepath.Join(schemaDir, "runtime-start.schema.json"))
	environmentReferenceContract = schematest.GoContract[EnvironmentReference](filepath.Join(schemaDir, "environment-reference.schema.json"))
	agentInstructionsContract    = schematest.GoContract[AgentInstructions](filepath.Join(schemaDir, "agent-instructions.schema.json"))
)

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

func TestRuntimeStartCarriesPreparedEnvironment(t *testing.T) {
	provider, _ := ids.New[ids.Provider]()
	ref := EnvironmentReference{Provider: provider, ResourceRef: "sandbox:one"}
	start := RuntimeStart{Brief: lifecycle.ExecutionBrief{Objective: "Update the fixture", AcceptanceCriteria: []string{"The fixture contains the requested value"}}, Environment: &ref}
	document := schematest.Marshal(t, start)
	if err := runtimeStartContract.GoAccept(document); err != nil {
		t.Fatalf("Go rejected runtime environment reference: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), document); err != nil {
		t.Fatalf("schema rejected runtime environment reference: %v", err)
	}
	if err := environmentReferenceContract.GoAccept(schematest.Marshal(t, ref)); err != nil {
		t.Fatalf("Go rejected environment reference: %v", err)
	}
	if runtimeStartContract.GoAccept([]byte(`{"brief":{"objective":"Update the fixture","acceptance_criteria":["The fixture contains the requested value"]},"environment":{"provider_id":"","resource_ref":"sandbox:one"}}`)) == nil {
		t.Fatal("Go accepted environment reference without provider")
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

func TestRuntimeStartCarriesAgentInstructionsAgreement(t *testing.T) {
	brief := lifecycle.ExecutionBrief{Objective: "Review the change", AcceptanceCriteria: []string{"Every finding names a file"}}
	valid := []AgentInstructions{
		{Role: "Reviews pull requests", Instructions: "Read the tests before the code."},
		{Role: "Reviews pull requests"},
	}
	for _, agent := range valid {
		data := schematest.Marshal(t, RuntimeStart{Brief: brief, Agent: &agent})
		if err := runtimeStartContract.GoAccept(data); err != nil {
			t.Fatalf("Go rejected delegated runtime start %s: %v", data, err)
		}
		if err := schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data); err != nil {
			t.Fatalf("schema rejected delegated runtime start %s: %v", data, err)
		}
		if err := agentInstructionsContract.GoAccept(schematest.Marshal(t, agent)); err != nil {
			t.Fatalf("Go rejected agent instructions: %v", err)
		}
	}

	invalid := []AgentInstructions{
		{},
		{Role: "   "},
		{Role: "Reviews pull requests", Instructions: " \n "},
		{Role: strings.Repeat("r", lifecycle.MaxAgentRoleCharacters+1)},
		{Role: "Reviews pull requests", Instructions: strings.Repeat("i", lifecycle.MaxAgentInstructionsCharacters+1)},
	}
	for _, agent := range invalid {
		data := schematest.Marshal(t, RuntimeStart{Brief: brief, Agent: &agent})
		if runtimeStartContract.GoAccept(data) == nil {
			t.Fatalf("Go accepted invalid agent instructions %s", data)
		}
		if schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data) == nil {
			t.Fatalf("schema accepted invalid agent instructions %s", data)
		}
	}
	if runtimeStartContract.GoAccept([]byte(`{"brief":{"objective":"Review the change","acceptance_criteria":["Every finding names a file"]},"agent":{"role":"Reviews","name":"Reviewer"}}`)) == nil {
		t.Fatal("Go accepted agent instructions with an unknown field")
	}
}
