package contracts

import (
	"path/filepath"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

func TestContractConformanceActionEvidence(t *testing.T) {
	hook := newHookRequest(t, `{"resource":"screen","value":1}`)
	execution := ActionExecution{Request: identity.ActionRequest{Tenant: hook.Tenant, Attempt: hook.Attempt, Provider: hook.Provider, Operation: hook.Operation, Action: hook.Action, Scope: hook.Scope, InputDigest: hook.InputDigest}, Grant: newID[ids.Grant](t), Decision: newID[ids.Decision](t), Source: identity.SourceOneTimeApproval}
	receipt := EnforcementReceipt{Execution: execution, OutputDigest: identity.DigestOf([]byte("completed")), CompletedAt: testNow}
	for _, tc := range []struct {
		contract schematest.Contract
		value    any
	}{
		{schematest.GoContract[ActionExecution](filepath.Join(schemaDir, "action-execution.schema.json")), execution},
		{schematest.GoContract[EnforcementReceipt](filepath.Join(schemaDir, "enforcement-receipt.schema.json")), receipt},
	} {
		raw := schematest.Marshal(t, tc.value)
		if err := tc.contract.GoAccept(raw); err != nil {
			t.Fatal(err)
		}
		schema := schematest.Compile(t, tc.contract.Schema)
		if err := schematest.Accepts(t, schema, raw); err != nil {
			t.Fatal(err)
		}
		bad := schematest.Mutate(t, raw, func(d map[string]any) { d["secret"] = "not permitted" })
		if tc.contract.GoAccept(bad) == nil || schematest.Accepts(t, schema, bad) == nil {
			t.Fatal("unknown field accepted")
		}
	}
}
