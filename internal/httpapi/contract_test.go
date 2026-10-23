package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

const (
	surfaceSchemas = "../../contracts/jsonschema/surface"
	openAPIPath    = "../../contracts/openapi/surface-v1.json"
)

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func schemaPath(name string) string { return filepath.Join(surfaceSchemas, name) }

var (
	submitContract    = schematest.GoContract[SubmitTaskRequest](schemaPath("submit-task-request.schema.json"))
	assertionContract = schematest.GoContract[SpendAssertionRequest](schemaPath("spend-assertion-request.schema.json"))
	decisionContract  = schematest.GoContract[DecisionRequest](schemaPath("decision-request.schema.json"))
	cancelContract    = schematest.GoContract[CancelRequest](schemaPath("cancel-request.schema.json"))
	reviewContract    = schematest.GoContract[ReviewRequest](schemaPath("review-request.schema.json"))
)

func newID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func digestOf(text string) identity.Digest { return identity.DigestOf([]byte(text)) }

func validRequests(t *testing.T) map[string]struct {
	contract schematest.Contract
	data     []byte
} {
	tenant, task := newID[ids.Tenant](t), newID[ids.Task](t)
	assertion := lifecycle.SpendAssertion{
		Challenge: newID[ids.Spend](t),
		Terms: lifecycle.SpendTerms{
			Tenant: tenant, Principal: newID[ids.Principal](t), Task: task, Attempt: newID[ids.Attempt](t),
			Operation: newID[ids.Operation](t), Amount: 10, Unit: "usd_cents", Nonce: digestOf("nonce"),
		},
		Proof: digestOf("proof"), AssertedAt: testNow,
	}
	rework := &application.ReworkTerms{Provider: newID[ids.Provider](t), Demand: contracts.DemandPerson, Deadline: testNow.Add(time.Hour), Unit: "usd_cents", Amount: 5}
	return map[string]struct {
		contract schematest.Contract
		data     []byte
	}{
		"submit": {submitContract, schematest.Marshal(t, SubmitTaskRequest{Provider: newID[ids.Provider](t), Deadline: testNow.Add(time.Hour), Unit: "usd_cents", Ceiling: 100, Amount: 10})},
		"submit governed person": {submitContract, schematest.Marshal(t, SubmitTaskRequest{
			Provider: newID[ids.Provider](t), Demand: contracts.DemandPerson, Deadline: testNow.Add(time.Hour), Unit: "usd_cents", Ceiling: 100, Amount: 10, Spend: true,
		})},
		"assert submission": {assertionContract, schematest.Marshal(t, SpendAssertionRequest{SubmissionKey: "submit-1", Assertion: assertion})},
		"assert review":     {assertionContract, schematest.Marshal(t, SpendAssertionRequest{ReviewKey: "review-1", Assertion: assertion})},
		"deny":              {decisionContract, schematest.Marshal(t, DecisionRequest{Kind: lifecycle.DecisionDeny})},
		"answer":            {decisionContract, schematest.Marshal(t, DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digestOf("answer")})},
		"cancel":            {cancelContract, []byte(`{}`)},
		"accept":            {reviewContract, schematest.Marshal(t, ReviewRequest{Revision: newID[ids.Revision](t), Disposition: lifecycle.DispositionAccept})},
		"request changes":   {reviewContract, schematest.Marshal(t, ReviewRequest{Revision: newID[ids.Revision](t), Disposition: lifecycle.DispositionRequestChanges, Rework: rework})},
	}
}

func TestSurfaceSchemasAcceptValidGoRequests(t *testing.T) {
	for name, request := range validRequests(t) {
		t.Run(name, func(t *testing.T) {
			if err := request.contract.GoAccept(request.data); err != nil {
				t.Fatalf("Go rejected %s: %v", request.data, err)
			}
			if err := schematest.Accepts(t, schematest.Compile(t, request.contract.Schema), request.data); err != nil {
				t.Fatalf("schema rejected %s: %v", request.data, err)
			}
		})
	}
}

func TestSurfaceSchemaAndGoRejectTheSameInvalidRequests(t *testing.T) {
	valid := validRequests(t)
	cases := []struct {
		name, base string
		change     func(map[string]any)
	}{
		{"submit without provider", "submit", func(d map[string]any) { delete(d, "provider_id") }},
		{"submit without deadline", "submit", func(d map[string]any) { delete(d, "deadline") }},
		{"submit unknown demand", "submit", func(d map[string]any) { d["demand"] = "urgent" }},
		{"submit zero ceiling", "submit", func(d map[string]any) { d["ceiling"] = 0 }},
		{"submit zero amount", "submit", func(d map[string]any) { d["amount"] = 0 }},
		{"submit malformed unit", "submit", func(d map[string]any) { d["unit"] = "USD Cents" }},
		{"submit unknown field", "submit", func(d map[string]any) { d["objective"] = "x" }},
		{"assert both keys", "assert submission", func(d map[string]any) { d["review_key"] = "review-1" }},
		{"assert no key", "assert submission", func(d map[string]any) { delete(d, "submission_key") }},
		{"assert without proof", "assert submission", func(d map[string]any) { delete(d["assertion"].(map[string]any), "proof") }},
		{"assert malformed proof", "assert submission", func(d map[string]any) { d["assertion"].(map[string]any)["proof"] = "md5:x" }},
		{"decision unknown kind", "deny", func(d map[string]any) { d["kind"] = "maybe" }},
		{"answer without digest", "answer", func(d map[string]any) { delete(d, "answer_digest") }},
		{"deny with digest", "deny", func(d map[string]any) { d["answer_digest"] = string(digestOf("x")) }},
		{"cancel with field", "cancel", func(d map[string]any) { d["reason"] = "x" }},
		{"review unknown disposition", "accept", func(d map[string]any) { d["disposition"] = "approve" }},
		{"review without revision", "accept", func(d map[string]any) { delete(d, "revision_id") }},
		{"request changes without rework", "request changes", func(d map[string]any) { delete(d, "rework") }},
		{"accept with rework", "request changes", func(d map[string]any) { d["disposition"] = "accept" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := valid[tc.base]
			data := schematest.Mutate(t, base.data, tc.change)
			if err := schematest.Accepts(t, schematest.Compile(t, base.contract.Schema), data); err == nil {
				t.Errorf("schema accepted %s", data)
			}
			if err := base.contract.GoAccept(data); err == nil {
				t.Errorf("Go accepted %s", data)
			}
		})
	}
}

// TestSurfaceSchemasAcceptGoResponses marshals every response body the API
// writes and validates it against its schema.
func TestSurfaceSchemasAcceptGoResponses(t *testing.T) {
	tenant, owner, task := newID[ids.Tenant](t), newID[ids.Principal](t), newID[ids.Task](t)
	taskRecord := lifecycle.Task{ID: task, Tenant: tenant, Owner: owner, State: lifecycle.TaskQueued, Version: 2, CreatedAt: testNow, UpdatedAt: testNow}
	event := store.StreamEvent{Position: 7, Tenant: tenant, Task: task, Record: lifecycle.RecordTask, RecordID: task.Untyped(), Event: "submit", From: "draft", To: "queued", Version: 2, OccurredAt: testNow}
	outside := store.StreamEvent{Position: 8, Tenant: tenant, Record: lifecycle.RecordHarness, RecordID: newID[ids.Any](t), Event: "promote", From: "off", To: "shadow", Version: 1, OccurredAt: testNow}
	attempt := lifecycle.Attempt{ID: newID[ids.Attempt](t), Tenant: tenant, Task: task, Number: 1, State: lifecycle.AttemptExecuting, Version: 3, CreatedAt: testNow, UpdatedAt: testNow}
	budget := lifecycle.Budget{ID: newID[ids.Budget](t), Tenant: tenant, Task: task, Unit: "usd_cents", Ceiling: 100, Reserved: 10, Version: 2, UpdatedAt: testNow}
	submission := application.Submission{
		Task: taskRecord, Attempt: attempt,
		Operation:   lifecycle.ProviderOperation{ID: newID[ids.Operation](t), Tenant: tenant, Attempt: attempt.ID, Provider: newID[ids.Provider](t), State: lifecycle.OperationDispatched, Version: 2, CreatedAt: testNow, UpdatedAt: testNow},
		Budget:      budget,
		Reservation: lifecycle.Reservation{ID: newID[ids.Reservation](t), Tenant: tenant, Budget: budget.ID, Attempt: attempt.ID, Amount: 10, State: lifecycle.ReservationHeld, Version: 1, CreatedAt: testNow, UpdatedAt: testNow},
		Provider:    newID[ids.Provider](t), Mode: lifecycle.HarnessLive, Demand: contracts.DemandBackground, Deadline: testNow.Add(time.Hour), Unit: "usd_cents", Amount: 10,
		Dispatch: &application.Dispatch{Status: "pending", Attempts: 0, LastError: "provider said: secret detail"},
	}
	revision := lifecycle.ResultRevision{ID: newID[ids.Revision](t), Tenant: tenant, Task: task, Attempt: attempt.ID, Number: 1, Artifacts: []ids.ArtifactID{newID[ids.Artifact](t)}, CreatedAt: testNow}
	review := lifecycle.ResultReview{ID: newID[ids.Review](t), Tenant: tenant, Task: task, Revision: revision.ID, Reviewer: owner, Disposition: lifecycle.DispositionRequestChanges, TaskVersion: 4, ReviewedAt: testNow}
	rework := submissionBody(submission)
	if strings.Contains(string(schematest.Marshal(t, rework)), "secret detail") {
		t.Fatal("submission body exposes provider error text")
	}
	for name, response := range map[string]struct {
		schema string
		body   any
	}{
		"submission":      {"submission.schema.json", submissionBody(submission)},
		"revision view":   {"revision-view.schema.json", revisionBody(application.RevisionView{Revision: revision, Reviews: []lifecycle.ResultReview{review}})},
		"review result":   {"review-result.schema.json", ReviewBody{Review: review, Rework: &rework}},
		"accepted review": {"review-result.schema.json", ReviewBody{Review: review}},
		"full task view":  {"task-view.schema.json", taskBody(application.TaskView{Task: taskRecord, Attempt: &attempt, Revisions: []application.RevisionView{{Revision: revision}}})},
		"task view":       {"task-view.schema.json", taskBody(application.TaskView{Task: taskRecord})},
		"attention":       {"attention-list.schema.json", AttentionListBody{Attention: []lifecycle.AttentionRequest{}}},
		"cancel":          {"cancel-result.schema.json", CancelBody{Task: task.String(), State: "cancelling"}},
		"decision":        {"decision-result.schema.json", DecisionBody{Attention: newID[ids.Attention](t).String(), Decision: newID[ids.Decision](t).String(), Kind: "deny", State: "decided"}},
		"event page":      {"event-page.schema.json", eventPageBody(application.EventPage{Events: []store.StreamEvent{event, outside}, Next: application.EventCursor{Generation: 1, Position: 8, Stream: application.StreamTag(tenant, ids.TaskID{})}})},
		"reset page":      {"event-page.schema.json", eventPageBody(application.EventPage{Next: application.EventCursor{Generation: 1, Stream: application.StreamTag(tenant, task)}, Reset: true})},
		"problem":         {"problem.schema.json", problemFor(application.ErrNotFound)},
		"problem 412":     {"problem.schema.json", problemFor(&lifecycle.TransitionError{Code: lifecycle.RejectVersionConflict})},
		"problem 500":     {"problem.schema.json", problemFor(os.ErrClosed)},
		"empty revisions": {"task-view.schema.json", taskBody(application.TaskView{Task: taskRecord, Revisions: nil})},
	} {
		t.Run(name, func(t *testing.T) {
			data := schematest.Marshal(t, response.body)
			if err := schematest.Accepts(t, schematest.Compile(t, schemaPath(response.schema)), data); err != nil {
				t.Fatalf("schema rejected %s: %v", data, err)
			}
		})
	}
}

func TestSurfaceSchemaEnumsMatchGoConstants(t *testing.T) {
	for _, tc := range []struct {
		file string
		path []string
		want []string
	}{
		{"submit-task-request.schema.json", []string{"properties", "demand", "enum"}, schematest.Strings(contracts.Demands())},
		{"decision-request.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(lifecycle.DecisionKinds())},
		{"review-request.schema.json", []string{"properties", "disposition", "enum"}, schematest.Strings(lifecycle.Dispositions())},
		{"submission.schema.json", []string{"properties", "harness_mode", "enum"}, schematest.Strings(lifecycle.HarnessModes())},
		{"event.schema.json", []string{"properties", "record_kind", "enum"}, schematest.Strings(lifecycle.RecordKinds())},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got := schematest.Enum(t, schemaPath(tc.file), tc.path...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("schema enum = %v, Go constants = %v", got, tc.want)
			}
		})
	}
}

type openAPIDocument struct {
	OpenAPI    string                                 `json:"openapi"`
	Security   []map[string][]string                  `json:"security"`
	Paths      map[string]map[string]openAPIOperation `json:"paths"`
	Components struct {
		Responses map[string]json.RawMessage `json:"responses"`
		Params    map[string]json.RawMessage `json:"parameters"`
	} `json:"components"`
}

type openAPIOperation struct {
	OperationID string                     `json:"operationId"`
	Parameters  []map[string]string        `json:"parameters"`
	RequestBody json.RawMessage            `json:"requestBody"`
	Responses   map[string]json.RawMessage `json:"responses"`
}

func loadOpenAPI(t *testing.T) (openAPIDocument, []byte) {
	t.Helper()
	data, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var document openAPIDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("OpenAPI document is not JSON: %v", err)
	}
	return document, data
}

// TestContractConformanceOpenAPIMatchesServer checks the document against the
// server in both directions: every served route is documented and every
// documented route is served.
func TestContractConformanceOpenAPIMatchesServer(t *testing.T) {
	document, _ := loadOpenAPI(t)
	if !strings.HasPrefix(document.OpenAPI, "3.1.") {
		t.Fatalf("openapi = %q, want 3.1.x", document.OpenAPI)
	}
	if len(document.Security) != 1 || document.Security[0]["bearer"] == nil {
		t.Fatalf("document security = %v, want bearer for every operation", document.Security)
	}
	var documented []string
	operationIDs := map[string]bool{}
	for path, operations := range document.Paths {
		for method, operation := range operations {
			documented = append(documented, strings.ToUpper(method)+" "+path)
			if operation.OperationID == "" || operationIDs[operation.OperationID] {
				t.Errorf("%s %s: missing or repeated operationId %q", method, path, operation.OperationID)
			}
			operationIDs[operation.OperationID] = true
			for _, status := range []string{"401", "403", "500"} {
				if operation.Responses[status] == nil {
					t.Errorf("%s %s does not document %s", method, path, status)
				}
			}
			params := map[string]bool{}
			for _, parameter := range operation.Parameters {
				params[parameter["$ref"]] = true
			}
			if method == "post" {
				if !params["#/components/parameters/IdempotencyKey"] || operation.RequestBody == nil {
					t.Errorf("POST %s must require Idempotency-Key and a body", path)
				}
				if params["#/components/parameters/IfMatch"] && (operation.Responses["412"] == nil || operation.Responses["428"] == nil) {
					t.Errorf("POST %s requires If-Match but does not document 412 and 428", path)
				}
			}
		}
	}
	served := (&API{}).Routes()
	sort.Strings(documented)
	sort.Strings(served)
	if !slices.Equal(documented, served) {
		t.Fatalf("documented routes %v\nserved routes     %v", documented, served)
	}
}

// TestContractConformanceOpenAPIReferencesResolve compiles every schema the
// document references and checks every component reference exists.
func TestContractConformanceOpenAPIReferencesResolve(t *testing.T) {
	document, data := loadOpenAPI(t)
	var walk func(any)
	refs := map[string]bool{}
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				refs[ref] = true
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	walk(root)
	for ref := range refs {
		switch {
		case strings.HasPrefix(ref, "#/components/responses/"):
			if document.Components.Responses[strings.TrimPrefix(ref, "#/components/responses/")] == nil {
				t.Errorf("unresolved %s", ref)
			}
		case strings.HasPrefix(ref, "#/components/parameters/"):
			if document.Components.Params[strings.TrimPrefix(ref, "#/components/parameters/")] == nil {
				t.Errorf("unresolved %s", ref)
			}
		case strings.HasPrefix(ref, "#/components/"):
		default:
			schematest.Compile(t, filepath.Join(filepath.Dir(openAPIPath), ref))
		}
	}
}

// TestContractConformanceProblemStatusesAreDocumented ties the error table to
// the documented problem responses.
func TestContractConformanceProblemStatusesAreDocumented(t *testing.T) {
	document, _ := loadOpenAPI(t)
	for _, err := range problemCases() {
		status := problemFor(err.err).Status
		if document.Components.Responses["Problem"+strconv.Itoa(status)] == nil {
			t.Errorf("%s maps to undocumented status %d", err.name, status)
		}
	}
	if http.StatusText(problemFor(nil).Status) == "" {
		t.Fatal("unmapped error has no status")
	}
}
