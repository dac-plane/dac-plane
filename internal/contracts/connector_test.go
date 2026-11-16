package contracts

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/testsupport/schematest"
)

var (
	completeAuthContract  = schematest.GoContract[CompleteAuthorization](filepath.Join(schemaDir, "complete-authorization.schema.json"))
	catalogueContract     = schematest.GoContract[ToolCatalogue](filepath.Join(schemaDir, "tool-catalogue.schema.json"))
	connectionRefContract = schematest.GoContract[ConnectionReference](filepath.Join(schemaDir, "connection-reference.schema.json"))
	beginAuthContract     = schematest.GoContract[BeginAuthorization](filepath.Join(schemaDir, "begin-authorization.schema.json"))
	authStartedContract   = schematest.GoContract[AuthorizationStarted](filepath.Join(schemaDir, "authorization-started.schema.json"))
	authResultContract    = schematest.GoContract[AuthorizationResult](filepath.Join(schemaDir, "authorization-result.schema.json"))
	connExecutionContract = schematest.GoContract[ConnectorExecution](filepath.Join(schemaDir, "connector-execution.schema.json"))
	connResultContract    = schematest.GoContract[ConnectorResult](filepath.Join(schemaDir, "connector-result.schema.json"))
	gatewayContract       = schematest.GoContract[GatewayAccess](filepath.Join(schemaDir, "gateway-access.schema.json"))
)

func newConnectorExecution(t *testing.T) ConnectorExecution {
	t.Helper()
	connection := newID[ids.Connection](t)
	input := json.RawMessage(`{"repository":"dac-plane/dac-plane","number":137}`)
	return ConnectorExecution{
		Request: identity.ActionRequest{
			Tenant: newID[ids.Tenant](t), Attempt: newID[ids.Attempt](t), Provider: newID[ids.Provider](t),
			Operation: newID[ids.Operation](t), Action: lifecycle.ConnectorAction("get_issue"),
			Scope: ConnectionScope(connection), InputDigest: identity.DigestOf(input),
		},
		Grant: newID[ids.Grant](t), Source: identity.SourceStandingGrant, ConnectionGrant: newID[ids.ConnGrant](t),
		Connection: connection, Tool: "get_issue", Input: input,
	}
}

func newConnectorResult(t *testing.T) ConnectorResult {
	t.Helper()
	output := json.RawMessage(`{"title":"Govern connectors","state":"open"}`)
	return ConnectorResult{Operation: newID[ids.Operation](t), Output: output, OutputDigest: identity.DigestOf(output), CompletedAt: testNow}
}

func TestContractConformanceConnectorProtocol(t *testing.T) {
	catalogue := ToolCatalogue{Tools: []ConnectorTool{
		{Name: "get_issue", Description: "Read one issue.", InputSchema: json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"}}}`), ReadOnly: true},
		{Name: "create_comment", Description: "Comment on an issue.", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	approved := newConnectorExecution(t)
	approved.Source, approved.Decision = identity.SourceOneTimeApproval, newID[ids.Decision](t)
	failed := newConnectorResult(t)
	failed.IsError = true
	for name, tc := range map[string]struct {
		contract schematest.Contract
		value    any
	}{
		"complete authorization": {completeAuthContract, CompleteAuthorization{Connection: newID[ids.Connection](t), Confirmation: strings.Repeat("p", 32)}},
		"catalogue":              {catalogueContract, catalogue},
		"reference":              {connectionRefContract, ConnectionReference{Connection: newID[ids.Connection](t)}},
		"begin":                  {beginAuthContract, BeginAuthorization{Connection: newID[ids.Connection](t), ExpiresAt: testNow}},
		"started":                {authStartedContract, AuthorizationStarted{URL: "https://github.com/login/oauth/authorize?client_id=x&state=y", ExpiresAt: testNow}},
		"loopback started":       {authStartedContract, AuthorizationStarted{URL: "http://127.0.0.1:8090/authorize?state=y", ExpiresAt: testNow}},
		"pending":                {authResultContract, AuthorizationResult{Status: AuthorizationPending}},
		"authorized":             {authResultContract, AuthorizationResult{Status: AuthorizationAuthorized, Account: &lifecycle.ConnectionAccount{Account: "octocat", Permissions: []string{"issues:write"}}}},
		"standing execution":     {connExecutionContract, newConnectorExecution(t)},
		"approved execution":     {connExecutionContract, approved},
		"result":                 {connResultContract, newConnectorResult(t)},
		"refused result":         {connResultContract, failed},
		"gateway":                {gatewayContract, GatewayAccess{URL: "https://dac.example/connectors/mcp", Token: "dacg1.abc.def"}},
	} {
		t.Run(name, func(t *testing.T) {
			raw := schematest.Marshal(t, tc.value)
			if err := tc.contract.GoAccept(raw); err != nil {
				t.Fatal(err)
			}
			schema := schematest.Compile(t, tc.contract.Schema)
			if err := schematest.Accepts(t, schema, raw); err != nil {
				t.Fatal(err)
			}
			bad := schematest.Mutate(t, raw, func(d map[string]any) { d["access_token"] = "not permitted" })
			if tc.contract.GoAccept(bad) == nil || schematest.Accepts(t, schema, bad) == nil {
				t.Fatal("unknown field accepted")
			}
		})
	}
}

func TestContractConformanceConnectorDocumentsRejected(t *testing.T) {
	execution := schematest.Marshal(t, newConnectorExecution(t))
	result := schematest.Marshal(t, newConnectorResult(t))
	for name, tc := range map[string]struct {
		contract schematest.Contract
		data     []byte
		goOnly   bool
	}{
		"empty catalogue":             {contract: catalogueContract, data: []byte(`{"tools":[]}`)},
		"bad tool name":               {contract: catalogueContract, data: []byte(`{"tools":[{"name":"Get-Issue","description":"x","input_schema":{"type":"object"}}]}`)},
		"tool schema not object":      {contract: catalogueContract, data: []byte(`{"tools":[{"name":"get_issue","description":"x","input_schema":{"type":"string"}}]}`)},
		"blank description":           {contract: catalogueContract, data: []byte(`{"tools":[{"name":"get_issue","description":" ","input_schema":{"type":"object"}}]}`)},
		"duplicate tool":              {contract: catalogueContract, goOnly: true, data: []byte(`{"tools":[{"name":"a","description":"x","input_schema":{"type":"object"}},{"name":"a","description":"y","input_schema":{"type":"object"}}]}`)},
		"plain http":                  {contract: authStartedContract, goOnly: true, data: []byte(`{"authorization_url":"http://github.com/login","expires_at":"2026-10-16T12:00:00Z"}`)},
		"url with credentials":        {contract: authStartedContract, goOnly: true, data: []byte(`{"authorization_url":"https://a:b@github.com/login","expires_at":"2026-10-16T12:00:00Z"}`)},
		"authorized no account":       {contract: authResultContract, data: []byte(`{"status":"authorized"}`)},
		"account missing permissions": {contract: authResultContract, data: []byte(`{"status":"authorized","account":{"account":"octocat"}}`)},
		"account null permissions":    {contract: authResultContract, data: []byte(`{"status":"authorized","account":{"account":"octocat","permissions":null}}`)},
		"pending with account":        {contract: authResultContract, data: []byte(`{"status":"pending","account":{"account":"octocat","permissions":[]}}`)},
		"unknown status":              {contract: authResultContract, data: []byte(`{"status":"expired"}`)},
		"input not an object":         {contract: connExecutionContract, data: schematest.Mutate(t, execution, func(d map[string]any) { d["input"] = []any{1} })},
		"input not canonical":         {contract: connExecutionContract, goOnly: true, data: schematest.Mutate(t, execution, func(d map[string]any) { d["input"] = json.RawMessage(`{"a": "<b>"}`) })},
		"input digest mismatch":       {contract: connExecutionContract, goOnly: true, data: schematest.Mutate(t, execution, func(d map[string]any) { d["input"] = map[string]any{"number": 1} })},
		"action of another tool":      {contract: connExecutionContract, goOnly: true, data: schematest.Mutate(t, execution, func(d map[string]any) { d["tool"] = "create_comment" })},
		"scope of another target":     {contract: connExecutionContract, goOnly: true, data: schematest.Mutate(t, execution, func(d map[string]any) { d["connection_id"] = newID[ids.Connection](t).String() })},
		"approval no decision":        {contract: connExecutionContract, data: schematest.Mutate(t, execution, func(d map[string]any) { d["source"] = "one_time_approval" })},
		"no connection grant":         {contract: connExecutionContract, data: schematest.Mutate(t, execution, func(d map[string]any) { delete(d, "connection_grant_id") })},
		"output digest mismatch":      {contract: connResultContract, goOnly: true, data: schematest.Mutate(t, result, func(d map[string]any) { d["output"] = "changed" })},
		"gateway token space":         {contract: gatewayContract, data: []byte(`{"url":"https://dac.example/mcp","token":"a b"}`)},
		"gateway not http":            {contract: gatewayContract, data: []byte(`{"url":"ftp://dac.example/mcp","token":"t"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.contract.GoAccept(tc.data) == nil {
				t.Fatalf("Go accepted %s", tc.data)
			}
			if !tc.goOnly && schematest.Accepts(t, schematest.Compile(t, tc.contract.Schema), tc.data) == nil {
				t.Fatalf("schema accepted %s", tc.data)
			}
		})
	}
}

func TestRuntimeStartCarriesTheGatewayAgreement(t *testing.T) {
	brief := lifecycle.ExecutionBrief{Objective: "Triage the issue", AcceptanceCriteria: []string{"The issue has a label"}}
	data := schematest.Marshal(t, RuntimeStart{Brief: brief, Gateway: &GatewayAccess{URL: "https://dac.example/connectors/mcp", Token: "dacg1.abc.def"}})
	if err := runtimeStartContract.GoAccept(data); err != nil {
		t.Fatal(err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), data); err != nil {
		t.Fatal(err)
	}
	bad := schematest.Marshal(t, RuntimeStart{Brief: brief, Gateway: &GatewayAccess{URL: "https://dac.example/mcp", Token: strings.Repeat("t", maxGatewayTokenBytes+1)}})
	if runtimeStartContract.GoAccept(bad) == nil || schematest.Accepts(t, schematest.Compile(t, runtimeStartContract.Schema), bad) == nil {
		t.Fatal("oversized gateway token accepted")
	}
}

func TestConnectorRequestsNeedTheConnectorRole(t *testing.T) {
	for _, method := range []Method{MethodListTools, MethodBeginAuthorization, MethodCompleteAuthorization, MethodRevokeConnection, MethodCallTool} {
		request := Request{Operation: newID[ids.Operation](t), Delivery: newID[ids.Delivery](t), Tenant: newID[ids.Tenant](t), Role: RoleConnector, Method: method, Demand: DemandPerson, Deadline: testNow}
		if method == MethodCallTool {
			request.Attempt = newID[ids.Attempt](t)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		withAttempt := request
		withAttempt.Attempt = newID[ids.Attempt](t)
		withoutAttempt := request
		withoutAttempt.Attempt = ids.AttemptID{}
		if (method == MethodCallTool && withoutAttempt.Validate() == nil) || (method != MethodCallTool && withAttempt.Validate() == nil) {
			t.Fatalf("%s: attempt rule not enforced", method)
		}
		frame := schematest.Marshal(t, Frame{Kind: FrameRequest, Request: &request})
		if err := frameContract.GoAccept(frame); err != nil {
			t.Fatalf("%s frame: %v", method, err)
		}
		if err := schematest.Accepts(t, schematest.Compile(t, frameContract.Schema), frame); err != nil {
			t.Fatalf("%s frame schema: %v", method, err)
		}
		if method != MethodCallTool {
			bad := schematest.Marshal(t, Frame{Kind: FrameRequest, Request: &withAttempt})
			if schematest.Accepts(t, schematest.Compile(t, frameContract.Schema), bad) == nil {
				t.Fatalf("%s frame schema accepted an attempt", method)
			}
		}
		request.Role = RoleEnforcement
		if request.Validate() == nil {
			t.Fatalf("%s accepted for the enforcement role", method)
		}
	}
}

func TestToolInputMustBeCanonical(t *testing.T) {
	for _, raw := range []string{`{"a": 1}`, `{"a":"<b>"}`, "{\n\"a\":1}"} {
		if ValidateToolInput(json.RawMessage(raw)) == nil {
			t.Errorf("accepted non-canonical input %q", raw)
		}
		canonical, err := CanonicalJSON(json.RawMessage(raw))
		if err != nil || ValidateToolInput(canonical) != nil {
			t.Errorf("canonical form of %q = %s, %v", raw, canonical, err)
		}
	}
	if _, err := CanonicalJSON(json.RawMessage(`{"a":`)); err == nil {
		t.Error("CanonicalJSON accepted invalid JSON")
	}
}

func TestConnectorResultSurvivesFrameRoundTrip(t *testing.T) {
	for _, raw := range []string{`{"html":"<p>&</p>"}`, `{ "message": "ok" }`} {
		output, err := CanonicalJSON(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		r := newConnectorResult(t)
		r.Output = output
		r.OutputDigest = identity.DigestOf(output)
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		result, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		reply := Reply{Operation: r.Operation, Delivery: newID[ids.Delivery](t), Outcome: OutcomeOK, Durability: lifecycle.SendAccepted, Result: result}
		frame, err := json.Marshal(Frame{Kind: FrameReply, Reply: &reply})
		if err != nil {
			t.Fatal(err)
		}
		var decoded Frame
		if err := json.Unmarshal(frame, &decoded); err != nil {
			t.Fatal(err)
		}
		var got ConnectorResult
		if err := json.Unmarshal(decoded.Reply.Result, &got); err != nil {
			t.Fatal(err)
		}
		if err := got.Validate(); err != nil {
			t.Fatal(err)
		}
		bad := r
		bad.Output = json.RawMessage(raw)
		bad.OutputDigest = identity.DigestOf(bad.Output)
		if bad.Validate() == nil {
			t.Fatal("unstable output accepted")
		}
	}
}
