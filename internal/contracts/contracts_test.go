package contracts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func newID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func hello(versions []uint32, offered, required []Capability) Lifecycle {
	return Lifecycle{Phase: PhaseHello, Versions: versions, Offered: offered, Required: required}
}

func lifecycleFrame(l Lifecycle) Frame {
	return Frame{Kind: FrameLifecycle, Lifecycle: &l}
}

func newRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		Operation: newID[ids.Operation](t), Delivery: newID[ids.Delivery](t),
		Tenant: newID[ids.Tenant](t), Attempt: newID[ids.Attempt](t),
		Role: RoleRuntime, Method: MethodStartTurn, Arguments: json.RawMessage(`{"prompt_ref":"p1"}`),
		Demand: DemandPerson, Deadline: testNow.Add(time.Minute),
	}
}

func requireFailure(t *testing.T, got *Failure, want FailureCode) {
	t.Helper()
	if got == nil || got.Code != want {
		t.Fatalf("failure = %v, want %s", got, want)
	}
}

func TestNegotiateSelectsHighestCommonVersionWithoutDowngrade(t *testing.T) {
	local := hello([]uint32{1, 2, 3}, []Capability{"stream.replay", "hooks"}, []Capability{"hooks"})
	ready, fail := Negotiate(local, hello([]uint32{2, 3, 4}, []Capability{"hooks", "spend"}, nil))
	if fail != nil || ready.Version != 3 || len(ready.Selected) != 1 || ready.Selected[0] != "hooks" {
		t.Fatalf("Negotiate() = %+v, %v", ready, fail)
	}
	_, fail = Negotiate(local, hello([]uint32{4}, []Capability{"hooks"}, nil))
	requireFailure(t, fail, FailureProtocolMismatch)
	_, fail = Negotiate(local, hello([]uint32{3}, []Capability{"stream.replay"}, nil))
	requireFailure(t, fail, FailureUnsupportedCapability)
	_, fail = Negotiate(local, hello([]uint32{3}, []Capability{"hooks", "spend"}, []Capability{"spend"}))
	requireFailure(t, fail, FailureUnsupportedCapability)
}

func newGate(t *testing.T) *Gate {
	t.Helper()
	gate, err := NewGate(hello([]uint32{1}, []Capability{"hooks"}, nil))
	if err != nil {
		t.Fatalf("NewGate() error = %v", err)
	}
	return gate
}

func TestGateRefusesWorkBeforeTheHandshake(t *testing.T) {
	gate := newGate(t)
	request := newRequest(t)
	response, fail := gate.Receive(Frame{Kind: FrameRequest, Request: &request})
	requireFailure(t, fail, FailureProtocolMismatch)
	if response == nil || response.Lifecycle.Phase != PhaseShutdown || response.Lifecycle.Failure.Code != FailureProtocolMismatch {
		t.Fatalf("response = %+v, want a shutdown with the failure", response)
	}
	_, fail = gate.Receive(lifecycleFrame(hello([]uint32{1}, nil, nil)))
	requireFailure(t, fail, FailureTransportUnavailable)
}

func TestGateFailsFastOnMalformedFramesAndMismatch(t *testing.T) {
	_, fail := newGate(t).Receive(Frame{Kind: FrameRequest})
	requireFailure(t, fail, FailureMalformedFrame)
	_, fail = newGate(t).Receive(lifecycleFrame(hello([]uint32{9}, nil, nil)))
	requireFailure(t, fail, FailureProtocolMismatch)
}

func TestGateOpensOnHelloThenAdmitsWork(t *testing.T) {
	gate := newGate(t)
	response, fail := gate.Receive(lifecycleFrame(hello([]uint32{1}, []Capability{"hooks"}, nil)))
	if fail != nil || response.Lifecycle.Phase != PhaseReady || response.Lifecycle.Version != 1 {
		t.Fatalf("hello = %+v, %v", response, fail)
	}
	if selected, open := gate.Selected(); !open || selected.Version != 1 {
		t.Fatalf("Selected() = %+v, %v", selected, open)
	}
	request := newRequest(t)
	if response, fail := gate.Receive(Frame{Kind: FrameRequest, Request: &request}); response != nil || fail != nil {
		t.Fatalf("request after ready = %+v, %v", response, fail)
	}
	_, fail = gate.Receive(lifecycleFrame(hello([]uint32{1}, nil, nil)))
	requireFailure(t, fail, FailureProtocolMismatch)
}

func TestPeerShutdownClosesTheGate(t *testing.T) {
	gate := newGate(t)
	gate.Receive(lifecycleFrame(hello([]uint32{1}, nil, nil)))
	if _, fail := gate.Receive(lifecycleFrame(Lifecycle{Phase: PhaseShutdown})); fail != nil {
		t.Fatalf("shutdown = %v", fail)
	}
	request := newRequest(t)
	_, fail := gate.Receive(Frame{Kind: FrameRequest, Request: &request})
	requireFailure(t, fail, FailureTransportUnavailable)
}

func TestFrameValidation(t *testing.T) {
	request := newRequest(t)
	cancel := Cancel{Operation: request.Operation, Delivery: request.Delivery}
	cases := map[string]Frame{
		"two bodies":           {Kind: FrameRequest, Request: &request, Cancel: &cancel},
		"wrong body":           {Kind: FrameCancel, Request: &request},
		"unknown kind":         {Kind: "notify"},
		"method of other role": func() Frame { r := request; r.Method = MethodProvision; return Frame{Kind: FrameRequest, Request: &r} }(),
		"unknown role":         func() Frame { r := request; r.Role = "billing"; return Frame{Kind: FrameRequest, Request: &r} }(),
		"array arguments": func() Frame {
			r := request
			r.Arguments = json.RawMessage(`[1]`)
			return Frame{Kind: FrameRequest, Request: &r}
		}(),
		"oversized arguments": func() Frame {
			r := request
			r.Arguments = json.RawMessage(`{"x":"` + strings.Repeat("a", maxArgumentsBytes) + `"}`)
			return Frame{Kind: FrameRequest, Request: &r}
		}(),
		"missing delivery": func() Frame {
			r := request
			r.Delivery = ids.DeliveryID{}
			return Frame{Kind: FrameRequest, Request: &r}
		}(),
		"unknown demand":        func() Frame { r := request; r.Demand = "urgent"; return Frame{Kind: FrameRequest, Request: &r} }(),
		"hello without version": lifecycleFrame(Lifecycle{Phase: PhaseHello}),
		"required not offered":  lifecycleFrame(hello([]uint32{1}, nil, []Capability{"hooks"})),
	}
	for name, frame := range cases {
		if err := frame.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
	if err := (Frame{Kind: FrameRequest, Request: &request}).Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
}

func TestReplyDurabilityMatchesOutcome(t *testing.T) {
	base := Reply{Operation: newID[ids.Operation](t), Delivery: newID[ids.Delivery](t)}
	fail := &Failure{Code: FailureProviderFailed, Message: "provider failed"}
	for _, tc := range []struct {
		outcome    Outcome
		durability lifecycle.SendStatus
		failure    *Failure
		valid      bool
	}{
		{OutcomeOK, lifecycle.SendAccepted, nil, true},
		{OutcomeOK, lifecycle.SendPending, nil, true},
		{OutcomeOK, lifecycle.SendUnknown, nil, false},
		{OutcomeOK, lifecycle.SendAccepted, fail, false},
		{OutcomeFailed, lifecycle.SendRejected, fail, true},
		{OutcomeFailed, lifecycle.SendUnknown, fail, true},
		{OutcomeFailed, lifecycle.SendAccepted, fail, false},
		{OutcomeFailed, lifecycle.SendRejected, nil, false},
	} {
		reply := base
		reply.Outcome, reply.Durability, reply.Failure = tc.outcome, tc.durability, tc.failure
		if err := reply.Validate(); (err == nil) != tc.valid {
			t.Errorf("%s/%s/failure=%v: Validate() = %v, want valid=%v", tc.outcome, tc.durability, tc.failure != nil, err, tc.valid)
		}
	}
}

// streamFixture is one Attempt's recorded provider operation, its
// authenticated peer and a cursor on one of its streams.
type streamFixture struct {
	operation lifecycle.ProviderOperation
	peer      Peer
	cursor    Cursor
}

func newStreamFixture(t *testing.T) streamFixture {
	t.Helper()
	operation, err := lifecycle.NewProviderOperation(newID[ids.Operation](t), newID[ids.Tenant](t), newID[ids.Attempt](t), newID[ids.Provider](t), testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	return streamFixture{
		operation: operation,
		peer:      Peer{Tenant: operation.Tenant, Provider: operation.Provider},
		cursor: Cursor{
			Tenant: operation.Tenant, Attempt: operation.Attempt, Operation: operation.ID,
			Stream: "transcript", Generation: 1, After: 5,
		},
	}
}

func entry(sequence, updated uint64, payload string) Entry {
	return Entry{Sequence: sequence, UpdatedSequence: updated, Kind: "message", Payload: json.RawMessage(payload), PayloadHash: identity.DigestOf([]byte(payload))}
}

func (f streamFixture) event(body StreamFrame, generation uint64) Event {
	return Event{
		Tenant: f.operation.Tenant, Attempt: f.operation.Attempt, Provider: f.operation.Provider,
		Operation: f.operation.ID, Stream: f.cursor.Stream, Generation: generation, Body: body,
	}
}

func (f streamFixture) ingest(t *testing.T, body StreamFrame, generation uint64) Ingestion {
	t.Helper()
	bound, fail := BindEvent(f.peer, f.event(body, generation), f.operation)
	if fail != nil {
		t.Fatalf("BindEvent() = %v", fail)
	}
	result, fail := Ingest(f.cursor, bound)
	if fail != nil {
		t.Fatalf("Ingest() = %v", fail)
	}
	return result
}

func TestIngestCountsDuplicatesAndAdvancesTheCursor(t *testing.T) {
	f := newStreamFixture(t)
	rows := StreamFrame{Kind: StreamRows, Rows: &Rows{
		Entries:    []Entry{entry(3, 4, `{"a":1}`), entry(6, 7, `{"a":2}`)},
		Tombstones: []Tombstone{{Sequence: 2, UpdatedSequence: 5}, {Sequence: 6, UpdatedSequence: 8}},
		Replay:     true,
	}}
	result := f.ingest(t, rows, 1)
	if result.Duplicates != 2 || len(result.Entries) != 1 || len(result.Tombstones) != 1 || result.Cursor.After != 8 || !result.Replay {
		t.Fatalf("Ingest() = %+v", result)
	}
	if result.Cursor.Generation != 1 || result.Rehydrate {
		t.Fatalf("rows must not change generation: %+v", result.Cursor)
	}
}

func TestIngestGenerationsAndRehydration(t *testing.T) {
	f := newStreamFixture(t)
	f.cursor.Generation = 2
	rows := StreamFrame{Kind: StreamRows, Rows: &Rows{Entries: []Entry{entry(9, 9, `{}`)}}}

	if result := f.ingest(t, rows, 1); !result.Stale || len(result.Entries) != 0 || result.Cursor != f.cursor {
		t.Fatalf("older generation = %+v, want stale and ignored", result)
	}
	if result := f.ingest(t, rows, 3); !result.Rehydrate || len(result.Entries) != 0 {
		t.Fatalf("newer generation without clear = %+v, want rehydrate", result)
	}
	cleared := f.ingest(t, StreamFrame{Kind: StreamCleared, NewGeneration: 4}, 2)
	if !cleared.Rehydrate || cleared.Cursor.Generation != 4 || cleared.Cursor.After != 0 {
		t.Fatalf("cleared = %+v", cleared)
	}
	if result := f.ingest(t, StreamFrame{Kind: StreamCursorTooOld}, 2); !result.Rehydrate || result.Cursor != f.cursor {
		t.Fatalf("cursor too old = %+v", result)
	}
	if result := f.ingest(t, StreamFrame{Kind: StreamHeartbeat, ServerTime: testNow}, 2); result.Cursor != f.cursor || result.Rehydrate {
		t.Fatalf("heartbeat changed the cursor: %+v", result)
	}

	bound, _ := BindEvent(f.peer, f.event(StreamFrame{Kind: StreamCleared, NewGeneration: 2}, 2), f.operation)
	_, fail := Ingest(f.cursor, bound)
	requireFailure(t, fail, FailureMalformedFrame)
}

func TestStreamFramesRejectBadPayloadsAndOrdering(t *testing.T) {
	good := entry(1, 1, `{"a":1}`)
	tampered := good
	tampered.Payload = json.RawMessage(`{"a":2}`)
	omittedWithPayload := good
	omittedWithPayload.PayloadOmitted = true
	omitted := Entry{Sequence: 2, UpdatedSequence: 2, Kind: "message", PayloadHash: good.PayloadHash, PayloadOmitted: true}
	for name, frame := range map[string]StreamFrame{
		"tampered payload":     {Kind: StreamRows, Rows: &Rows{Entries: []Entry{tampered}}},
		"omitted with payload": {Kind: StreamRows, Rows: &Rows{Entries: []Entry{omittedWithPayload}}},
		"out of order":         {Kind: StreamRows, Rows: &Rows{Entries: []Entry{entry(2, 3, `{}`), entry(1, 2, `{}`)}}},
		"empty rows":           {Kind: StreamRows, Rows: &Rows{}},
		"cleared without gen":  {Kind: StreamCleared},
		"heartbeat no time":    {Kind: StreamHeartbeat},
		"update before entry":  {Kind: StreamRows, Rows: &Rows{Entries: []Entry{entry(5, 4, `{}`)}}},
	} {
		if err := frame.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
	if err := (StreamFrame{Kind: StreamRows, Rows: &Rows{Entries: []Entry{good, omitted}}}).Validate(); err != nil {
		t.Fatalf("inline and omitted payloads: %v", err)
	}
}

func TestEventsAreAuthenticatedAndBoundBeforeUse(t *testing.T) {
	f := newStreamFixture(t)
	body := StreamFrame{Kind: StreamHeartbeat, ServerTime: testNow}

	impostor := f.peer
	impostor.Provider = newID[ids.Provider](t)
	_, fail := BindEvent(impostor, f.event(body, 1), f.operation)
	requireFailure(t, fail, FailureUnauthenticated)

	foreign := f.event(body, 1)
	foreign.Attempt = newID[ids.Attempt](t)
	_, fail = BindEvent(f.peer, foreign, f.operation)
	requireFailure(t, fail, FailureUnauthorized)

	otherTenant := f.peer
	otherTenant.Tenant = newID[ids.Tenant](t)
	_, fail = BindEvent(otherTenant, f.event(body, 1), f.operation)
	requireFailure(t, fail, FailureUnauthorized)

	_, fail = BindEvent(f.peer, f.event(StreamFrame{Kind: StreamRows}, 1), f.operation)
	requireFailure(t, fail, FailureMalformedFrame)

	bound, fail := BindEvent(f.peer, f.event(body, 1), f.operation)
	if fail != nil || bound.Event().Operation != f.operation.ID {
		t.Fatalf("BindEvent() = %+v, %v", bound, fail)
	}
	otherStream := f.cursor
	otherStream.Stream = "usage"
	_, fail = Ingest(otherStream, bound)
	requireFailure(t, fail, FailureUnauthorized)
}

func newHookRequest(t *testing.T, input string) HookRequest {
	t.Helper()
	return HookRequest{
		Kind: HookPreAction, Operation: newID[ids.Operation](t), Tenant: newID[ids.Tenant](t),
		Attempt: newID[ids.Attempt](t), Provider: newID[ids.Provider](t), Action: "fs.write",
		Scope: identity.Scope{Class: identity.ScopeSubject, Target: "repo/main.go"},
		Input: json.RawMessage(input), InputDigest: identity.DigestOf([]byte(input)),
		Deadline: testNow.Add(time.Second),
	}
}

func TestDecidePreActionFailsClosed(t *testing.T) {
	request := newHookRequest(t, `{"path":"repo/main.go"}`)
	allow := &HookResponse{Kind: HookPreAction, Permission: PermissionAllow}
	for want, call := range map[HookOutcome]func() (HookDecision, error){
		HookMissing:  func() (HookDecision, error) { return DecidePreAction(request, nil, testNow) },
		HookTimedOut: func() (HookDecision, error) { return DecidePreAction(request, allow, request.Deadline) },
		HookMalformed: func() (HookDecision, error) {
			return DecidePreAction(request, &HookResponse{Kind: HookPostAction}, testNow)
		},
		HookDenied: func() (HookDecision, error) {
			return DecidePreAction(request, &HookResponse{Kind: HookPreAction, Permission: PermissionDeny}, testNow)
		},
	} {
		decision, err := call()
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if decision.Allowed() || decision.Evidence.Outcome != want || decision.EffectiveInput != nil {
			t.Fatalf("%s: decision = %+v", want, decision)
		}
		if err := decision.Evidence.Validate(); err != nil {
			t.Fatalf("%s: evidence invalid: %v", want, err)
		}
	}
	deniedRewrite := &HookResponse{Kind: HookPreAction, Permission: PermissionDeny, UpdatedInput: json.RawMessage(`{}`)}
	if decision, _ := DecidePreAction(request, deniedRewrite, testNow); decision.Evidence.Outcome != HookMalformed {
		t.Fatalf("deny with rewrite = %s, want malformed", decision.Evidence.Outcome)
	}
}

func TestHookRewriteIsAuthorizedAsRewritten(t *testing.T) {
	request := newHookRequest(t, `{"path":"repo/main.go"}`)
	passthrough, err := DecidePreAction(request, &HookResponse{Kind: HookPreAction, Permission: PermissionAllow}, testNow)
	if err != nil || !passthrough.Allowed() || passthrough.Evidence.Rewritten || passthrough.Evidence.EffectiveDigest != request.InputDigest {
		t.Fatalf("passthrough = %+v, %v", passthrough, err)
	}

	rewrite := &HookResponse{Kind: HookPreAction, Permission: PermissionAllow, UpdatedInput: json.RawMessage(`{"path":"/etc/passwd"}`), UserMessage: "path normalized"}
	decision, err := DecidePreAction(request, rewrite, testNow)
	if err != nil || !decision.Allowed() || !decision.Evidence.Rewritten || string(decision.EffectiveInput) != `{"path":"/etc/passwd"}` {
		t.Fatalf("rewrite = %+v, %v", decision, err)
	}
	if err := decision.Evidence.Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}

	// A one-time approval of the original input does not authorize the
	// rewritten action: authority is evaluated against the effective input.
	policy := identity.Policy{
		Tenant: request.Tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{"fs.write": identity.PermissionAsk},
		Ceiling:  map[identity.ActionClass]identity.Permission{"fs.write": identity.PermissionAsk},
	}
	grant := identity.AuthorityGrant{
		ID: newID[ids.Grant](t), Tenant: request.Tenant, Attempt: request.Attempt, Provider: request.Provider,
		Action: request.Action, Mode: identity.GrantAllowOnce, Operation: request.Operation,
		InputDigest: request.InputDigest, GrantedBy: newID[ids.Principal](t), PolicyGeneration: 1,
		IssuedAt: testNow.Add(-time.Minute), ExpiresAt: testNow.Add(time.Hour),
	}
	base := identity.ActionRequest{
		Tenant: request.Tenant, Attempt: request.Attempt, Provider: request.Provider, Operation: request.Operation,
		Action: request.Action, Scope: request.Scope, InputDigest: request.InputDigest,
	}
	use, _, err := identity.Authorize(decision.EffectiveRequest(base), &grant, policy, testNow)
	if err != nil || use.Reason != identity.DenialInputMismatch {
		t.Fatalf("Authorize(rewritten) = %s/%s, %v; want input_mismatch", use.Outcome, use.Reason, err)
	}
	if use, _, _ := identity.Authorize(passthrough.EffectiveRequest(base), &grant, policy, testNow); use.Outcome != identity.UseAllowed {
		t.Fatalf("Authorize(passthrough) = %s/%s", use.Outcome, use.Reason)
	}
}

func TestHookRequestAndResponseShapes(t *testing.T) {
	pre := newHookRequest(t, `{"a":1}`)
	post := pre
	post.Kind, post.Input, post.Deadline, post.Duration, post.OutputDigest = HookPostAction, nil, time.Time{}, time.Second, identity.DigestOf([]byte("out"))
	failed := post
	failed.Kind, failed.OutputDigest, failed.Failure = HookActionFailure, "", &Failure{Code: FailureProviderFailed, Message: "tool crashed"}
	for name, request := range map[string]HookRequest{"pre": pre, "post": post, "failure": failed} {
		if err := request.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(r *HookRequest){
		"pre without deadline":   func(r *HookRequest) { r.Deadline = time.Time{} },
		"input digest mismatch":  func(r *HookRequest) { r.InputDigest = identity.DigestOf([]byte("other")) },
		"pre with output digest": func(r *HookRequest) { r.OutputDigest = identity.DigestOf([]byte("x")) },
		"unknown kind":           func(r *HookRequest) { r.Kind = "pre_compact" },
	} {
		r := pre
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
	for name, response := range map[string]HookResponse{
		"post with permission":   {Kind: HookPostAction, Permission: PermissionAllow},
		"pre without permission": {Kind: HookPreAction},
		"post rewriting input":   {Kind: HookPostAction, UpdatedInput: json.RawMessage(`{}`)},
		"too much context":       {Kind: HookPostAction, AdditionalContext: make([]string, maxContextItems+1)},
		"bad usage unit":         {Kind: HookPostAction, Usage: []UsageMeter{{Unit: "Tokens", Amount: 1}}},
	} {
		if err := response.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
	usage := HookResponse{Kind: HookPostAction, AdditionalContext: []string{"lint passed"}, Usage: []UsageMeter{{Unit: "tokens", Amount: 1200}}}
	if err := usage.Validate(); err != nil {
		t.Fatalf("post response with usage: %v", err)
	}
}

func TestPreconditionsRejectStaleStateAndParallelActions(t *testing.T) {
	token := identity.DigestOf([]byte("snapshot-1"))
	p := ActionPrecondition{Operation: newID[ids.Operation](t), Resource: "screen", Token: token}
	if fail := CheckPrecondition(p, "screen", token); fail != nil {
		t.Fatalf("current token: %v", fail)
	}
	requireFailure(t, CheckPrecondition(p, "screen", identity.DigestOf([]byte("snapshot-2"))), FailureStalePrecondition)
	requireFailure(t, CheckPrecondition(p, "browser", token), FailureInvalidArguments)

	other := ActionPrecondition{Operation: newID[ids.Operation](t), Resource: "screen", Token: token}
	requireFailure(t, RequireSerial([]ActionPrecondition{p}, other), FailureResourceBusy)
	if fail := RequireSerial([]ActionPrecondition{p}, p); fail != nil {
		t.Fatalf("the same operation is not a second action: %v", fail)
	}
	elsewhere := other
	elsewhere.Resource = "browser"
	if fail := RequireSerial([]ActionPrecondition{p}, elsewhere); fail != nil {
		t.Fatalf("another resource is not busy: %v", fail)
	}
}

func TestMethodCatalogueIsPerRole(t *testing.T) {
	seen := map[Method]Role{}
	for _, role := range Roles() {
		if len(Methods(role)) == 0 {
			t.Errorf("role %s has no methods", role)
		}
		for _, method := range Methods(role) {
			if owner, dup := seen[method]; dup {
				t.Errorf("method %s belongs to both %s and %s", method, owner, role)
			}
			seen[method] = role
		}
	}
	if Methods("billing") != nil {
		t.Fatal("unknown role has methods")
	}
}
