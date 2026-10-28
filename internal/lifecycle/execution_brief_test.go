package lifecycle

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

var executionBriefContract = schematest.GoContract[ExecutionBrief](filepath.Join("../../contracts/jsonschema/lifecycle", "execution-brief.schema.json"))

func TestExecutionBriefSchemaAgreement(t *testing.T) {
	valid := ExecutionBrief{Objective: "Preserve this text exactly", AcceptanceCriteria: []string{"First criterion", "Second criterion"}}
	document := schematest.Marshal(t, valid)
	if err := executionBriefContract.GoAccept(document); err != nil {
		t.Fatalf("Go rejected brief: %v", err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, executionBriefContract.Schema), document); err != nil {
		t.Fatalf("schema rejected brief: %v", err)
	}

	invalid := []ExecutionBrief{
		{},
		{Objective: "   ", AcceptanceCriteria: []string{"done"}},
		{Objective: strings.Repeat("x", MaxObjectiveCharacters+1), AcceptanceCriteria: []string{"done"}},
		{Objective: "work", AcceptanceCriteria: []string{"\t"}},
		{Objective: "work", AcceptanceCriteria: make([]string, MaxAcceptanceCriteria+1)},
	}
	for _, brief := range invalid {
		data := schematest.Marshal(t, brief)
		if executionBriefContract.GoAccept(data) == nil {
			t.Fatalf("Go accepted invalid brief %s", data)
		}
		if schematest.Accepts(t, schematest.Compile(t, executionBriefContract.Schema), data) == nil {
			t.Fatalf("schema accepted invalid brief %s", data)
		}
	}
}
