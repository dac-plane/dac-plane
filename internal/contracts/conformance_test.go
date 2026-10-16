package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

const schemaDir = "../../contracts/jsonschema/adapter"

var (
	frameContract        = schematest.GoContract[Frame](filepath.Join(schemaDir, "frame.schema.json"))
	eventContract        = schematest.GoContract[Event](filepath.Join(schemaDir, "event.schema.json"))
	streamContract       = schematest.GoContract[StreamFrame](filepath.Join(schemaDir, "stream-frame.schema.json"))
	cursorContract       = schematest.GoContract[Cursor](filepath.Join(schemaDir, "cursor.schema.json"))
	hookRequestContract  = schematest.GoContract[HookRequest](filepath.Join(schemaDir, "hook-request.schema.json"))
	hookResponseContract = schematest.GoContract[HookResponse](filepath.Join(schemaDir, "hook-response.schema.json"))
	evidenceContract     = schematest.GoContract[HookEvidence](filepath.Join(schemaDir, "hook-evidence.schema.json"))
	preconditionContract = schematest.GoContract[ActionPrecondition](filepath.Join(schemaDir, "action-precondition.schema.json"))
)

type fixture struct {
	contract schematest.Contract
	data     []byte
}

// conformanceFixtures covers every frame family and stream outcome the issue
// names, built through the Go contracts so they are valid by construction.
func conformanceFixtures(t *testing.T) map[string]fixture {
	t.Helper()
	request := newRequest(t)
	cancel := Cancel{Operation: request.Operation, Delivery: request.Delivery}
	okReply := Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: OutcomeOK, Durability: lifecycle.SendAccepted, Result: json.RawMessage(`{"turn_ref":"t1"}`)}
	unknownReply := Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: OutcomeFailed, Durability: lifecycle.SendUnknown, Failure: &Failure{Code: FailureTransportUnavailable, Message: "connection reset after send"}}
	shutdown := Lifecycle{Phase: PhaseShutdown, Failure: &Failure{Code: FailureProtocolMismatch, Message: "no common protocol version"}}
	ready, _ := Negotiate(hello([]uint32{1, 2}, []Capability{"hooks", "stream.replay"}, []Capability{"hooks"}), hello([]uint32{2}, []Capability{"hooks"}, nil))

	f := newStreamFixture(t)
	rows := StreamFrame{Kind: StreamRows, Rows: &Rows{
		Entries: []Entry{
			entry(6, 6, `{"text":"hello"}`),
			{Sequence: 7, UpdatedSequence: 7, Kind: "tool_call", PayloadHash: identity.DigestOf([]byte("large")), PayloadOmitted: true},
		},
		Tombstones: []Tombstone{{Sequence: 2, UpdatedSequence: 8}},
		Replay:     true,
	}}
	event := f.event(rows, 1)

	pre := newHookRequest(t, `{"path":"repo/main.go"}`)
	post := pre
	post.Kind, post.Input, post.Deadline, post.Duration, post.OutputDigest = HookPostAction, nil, time.Time{}, 3*time.Second, identity.DigestOf([]byte("out"))
	rewrite := HookResponse{Kind: HookPreAction, Permission: PermissionAllow, UpdatedInput: json.RawMessage(`{"path":"repo/main.go","mode":"0644"}`), UserMessage: "mode set", AdditionalContext: []string{"lint ok"}}
	decision, err := DecidePreAction(pre, &rewrite, testNow)
	if err != nil {
		t.Fatalf("DecidePreAction() error = %v", err)
	}
	denied, _ := DecidePreAction(pre, nil, testNow)

	marshal := func(v any) []byte { return schematest.Marshal(t, v) }
	return map[string]fixture{
		"hello":            {frameContract, marshal(lifecycleFrame(hello([]uint32{1, 2}, []Capability{"hooks"}, []Capability{"hooks"})))},
		"ready":            {frameContract, marshal(lifecycleFrame(ready))},
		"shutdown":         {frameContract, marshal(lifecycleFrame(shutdown))},
		"request":          {frameContract, marshal(Frame{Kind: FrameRequest, Request: &request})},
		"cancel":           {frameContract, marshal(Frame{Kind: FrameCancel, Cancel: &cancel})},
		"reply ok":         {frameContract, marshal(Frame{Kind: FrameReply, Reply: &okReply})},
		"reply unknown":    {frameContract, marshal(Frame{Kind: FrameReply, Reply: &unknownReply})},
		"event frame":      {frameContract, marshal(Frame{Kind: FrameEvent, Event: &event})},
		"event":            {eventContract, marshal(event)},
		"connected":        {streamContract, marshal(StreamFrame{Kind: StreamConnected, ServerTime: testNow})},
		"rows":             {streamContract, marshal(rows)},
		"cleared":          {streamContract, marshal(StreamFrame{Kind: StreamCleared, NewGeneration: 2})},
		"cursor too old":   {streamContract, marshal(StreamFrame{Kind: StreamCursorTooOld})},
		"heartbeat":        {streamContract, marshal(StreamFrame{Kind: StreamHeartbeat, ServerTime: testNow})},
		"cursor":           {cursorContract, marshal(f.cursor)},
		"pre hook":         {hookRequestContract, marshal(pre)},
		"post hook":        {hookRequestContract, marshal(post)},
		"rewrite response": {hookResponseContract, marshal(rewrite)},
		"usage response":   {hookResponseContract, marshal(HookResponse{Kind: HookPostAction, Usage: []UsageMeter{{Unit: "tokens", Amount: 1200}}})},
		"allowed evidence": {evidenceContract, marshal(decision.Evidence)},
		"missing evidence": {evidenceContract, marshal(denied.Evidence)},
		"precondition":     {preconditionContract, marshal(ActionPrecondition{Operation: newID[ids.Operation](t), Resource: "screen", Token: identity.DigestOf([]byte("snapshot"))})},
	}
}

func TestContractConformanceGoFixturesMatchSchemas(t *testing.T) {
	for name, f := range conformanceFixtures(t) {
		t.Run(name, func(t *testing.T) {
			if err := f.contract.GoAccept(f.data); err != nil {
				t.Fatalf("Go rejected its own fixture %s: %v", f.data, err)
			}
			if err := schematest.Accepts(t, schematest.Compile(t, f.contract.Schema), f.data); err != nil {
				t.Fatalf("schema rejected Go fixture %s: %v", f.data, err)
			}
		})
	}
}

func TestContractConformanceInvalidDocuments(t *testing.T) {
	fixtures := conformanceFixtures(t)
	otherID := newID[ids.Operation](t).String()
	inner := func(key string, change func(map[string]any)) func(map[string]any) {
		return func(d map[string]any) { change(d[key].(map[string]any)) }
	}
	cases := []struct {
		name   string
		base   string
		change func(map[string]any)
	}{
		{"frame with two bodies", "request", func(d map[string]any) { d["cancel"] = map[string]any{"operation_id": otherID, "delivery_id": otherID} }},
		{"frame wrong body", "request", func(d map[string]any) { d["kind"] = "cancel" }},
		{"frame unknown kind", "request", func(d map[string]any) { d["kind"] = "notify" }},
		{"hello without versions", "hello", inner("lifecycle", func(l map[string]any) { delete(l, "versions") })},
		{"hello with version zero", "hello", inner("lifecycle", func(l map[string]any) { l["versions"] = []any{0} })},
		{"hello with selection", "hello", inner("lifecycle", func(l map[string]any) { l["selected"] = []any{"hooks"} })},
		{"ready without version", "ready", inner("lifecycle", func(l map[string]any) { delete(l, "version") })},
		{"shutdown with versions", "shutdown", inner("lifecycle", func(l map[string]any) { l["versions"] = []any{1} })},
		{"malformed capability", "hello", inner("lifecycle", func(l map[string]any) { l["offered"] = []any{"Hooks!"} })},
		{"request missing delivery", "request", inner("request", func(r map[string]any) { delete(r, "delivery_id") })},
		{"request method of other role", "request", inner("request", func(r map[string]any) { r["method"] = "provision" })},
		{"request unknown method", "request", inner("request", func(r map[string]any) { r["method"] = "launch_rocket" })},
		{"request unknown demand", "request", inner("request", func(r map[string]any) { r["demand"] = "urgent" })},
		{"request array arguments", "request", inner("request", func(r map[string]any) { r["arguments"] = []any{1} })},
		{"reply ok with unknown durability", "reply ok", inner("reply", func(r map[string]any) { r["durability"] = "unknown" })},
		{"reply failed without failure", "reply unknown", inner("reply", func(r map[string]any) { delete(r, "failure") })},
		{"reply failed accepted", "reply unknown", inner("reply", func(r map[string]any) { r["durability"] = "accepted" })},
		{"reply unknown failure code", "reply unknown", inner("reply", func(r map[string]any) {
			r["failure"] = map[string]any{"code": "gateway_exploded", "message": "x"}
		})},
		{"event missing provider", "event", func(d map[string]any) { delete(d, "provider_id") }},
		{"event zero generation", "event", func(d map[string]any) { d["generation"] = 0 }},
		{"rows frame without rows", "rows", func(d map[string]any) { delete(d, "rows") }},
		{"rows empty", "rows", func(d map[string]any) { d["rows"] = map[string]any{"replay": true} }},
		{"entry payload and omitted", "rows", func(d map[string]any) {
			e := d["rows"].(map[string]any)["entries"].([]any)[1].(map[string]any)
			e["payload"] = map[string]any{"x": 1}
		}},
		{"entry neither payload nor omitted", "rows", func(d map[string]any) {
			e := d["rows"].(map[string]any)["entries"].([]any)[1].(map[string]any)
			delete(e, "payload_omitted")
		}},
		{"entry zero sequence", "rows", func(d map[string]any) {
			d["rows"].(map[string]any)["tombstones"].([]any)[0].(map[string]any)["sequence"] = 0
		}},
		{"cleared without generation", "cleared", func(d map[string]any) { delete(d, "new_generation") }},
		{"heartbeat with rows", "heartbeat", func(d map[string]any) { d["rows"] = map[string]any{"replay": true} }},
		{"heartbeat without time", "heartbeat", func(d map[string]any) { delete(d, "server_time") }},
		{"cursor zero generation", "cursor", func(d map[string]any) { d["generation"] = 0 }},
		{"cursor bad stream name", "cursor", func(d map[string]any) { d["stream"] = "Transcript Log" }},
		{"pre hook without deadline", "pre hook", func(d map[string]any) { delete(d, "deadline") }},
		{"pre hook with duration", "pre hook", func(d map[string]any) { d["duration_ns"] = 5 }},
		{"post hook with input", "post hook", func(d map[string]any) { d["input"] = map[string]any{"a": 1} }},
		{"post hook without output digest", "post hook", func(d map[string]any) { delete(d, "output_digest") }},
		{"hook unknown kind", "pre hook", func(d map[string]any) { d["kind"] = "pre_compact" }},
		{"pre response without permission", "rewrite response", func(d map[string]any) { delete(d, "permission") }},
		{"deny with rewrite", "rewrite response", func(d map[string]any) { d["permission"] = "deny" }},
		{"post response rewriting", "usage response", func(d map[string]any) { d["updated_input"] = map[string]any{} }},
		{"response unknown field", "usage response", func(d map[string]any) { d["raw_secret"] = "x" }},
		{"usage negative amount", "usage response", func(d map[string]any) {
			d["usage"] = []any{map[string]any{"unit": "tokens", "amount": -1}}
		}},
		{"denied evidence with effective digest", "missing evidence", func(d map[string]any) { d["effective_digest"] = d["original_digest"] }},
		{"allowed evidence without effective digest", "allowed evidence", func(d map[string]any) { delete(d, "effective_digest") }},
		{"evidence with raw input", "allowed evidence", func(d map[string]any) { d["input"] = map[string]any{} }},
		{"precondition without token", "precondition", func(d map[string]any) { delete(d, "token") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := fixtures[tc.base]
			if !ok {
				t.Fatalf("unknown base fixture %q", tc.base)
			}
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

func TestContractConformanceEnumsCannotDrift(t *testing.T) {
	path := func(file string) string { return filepath.Join(schemaDir, file) }
	for _, tc := range []struct {
		file string
		keys []string
		want []string
	}{
		{"common.schema.json", []string{"$defs", "failure", "properties", "code", "enum"}, schematest.Strings(FailureCodes())},
		{"frame.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(FrameKinds())},
		{"frame.schema.json", []string{"$defs", "lifecycle", "properties", "phase", "enum"}, schematest.Strings(Phases())},
		{"frame.schema.json", []string{"$defs", "request", "properties", "role", "enum"}, schematest.Strings(Roles())},
		{"frame.schema.json", []string{"$defs", "request", "properties", "method", "enum"}, schematest.Strings(AllMethods())},
		{"frame.schema.json", []string{"$defs", "request", "properties", "demand", "enum"}, schematest.Strings(Demands())},
		{"frame.schema.json", []string{"$defs", "reply", "properties", "outcome", "enum"}, schematest.Strings(Outcomes())},
		{"frame.schema.json", []string{"$defs", "reply", "properties", "durability", "enum"}, schematest.Strings(lifecycle.SendStatuses())},
		{"stream-frame.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(StreamKinds())},
		{"hook-request.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(HookKinds())},
		{"hook-response.schema.json", []string{"properties", "permission", "enum"}, schematest.Strings(Permissions())},
		{"hook-evidence.schema.json", []string{"properties", "outcome", "enum"}, schematest.Strings(HookOutcomes())},
	} {
		t.Run(tc.file+"#"+strings.Join(tc.keys, "/"), func(t *testing.T) {
			if got := schematest.Enum(t, path(tc.file), tc.keys...); !slices.Equal(got, tc.want) {
				t.Fatalf("schema enum = %v, Go constants = %v", got, tc.want)
			}
		})
	}
}

func TestContractConformanceRoleMethodConditionsMatchCatalogue(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(schemaDir, "frame.schema.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var schema struct {
		Defs struct {
			Request struct {
				AllOf []struct {
					If struct {
						Properties struct {
							Role struct {
								Const string `json:"const"`
							} `json:"role"`
						} `json:"properties"`
					} `json:"if"`
					Then struct {
						Properties struct {
							Method struct {
								Enum []string `json:"enum"`
							} `json:"method"`
						} `json:"properties"`
					} `json:"then"`
				} `json:"allOf"`
			} `json:"request"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	conditions := map[string][]string{}
	for _, condition := range schema.Defs.Request.AllOf {
		conditions[condition.If.Properties.Role.Const] = condition.Then.Properties.Method.Enum
	}
	if len(conditions) != len(Roles()) {
		t.Fatalf("schema has method conditions for %d roles, Go has %d", len(conditions), len(Roles()))
	}
	for _, role := range Roles() {
		if got, want := conditions[string(role)], schematest.Strings(Methods(role)); !slices.Equal(got, want) {
			t.Errorf("%s methods: schema %v, Go %v", role, got, want)
		}
	}
}
