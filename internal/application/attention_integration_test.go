//go:build integration

package application_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// executing submits, dispatches and reports the runtime session, so the
// Attempt is executing and may await input.
func (f *fixture) executing(t *testing.T, nonce string) (application.Submission, *simulator.Runtime) {
	t.Helper()
	s, rt := f.live(t, nonce)
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
	)))
	return s, rt
}

// question is a well-formed question request.
func question(ref string, expires time.Time) contracts.AttentionRequested {
	return contracts.AttentionRequested{NativeRef: ref, Kind: lifecycle.AttentionQuestion, ExpiresAt: expires}
}

// approval is a well-formed action approval for one exact action.
func approval(ref string, expires time.Time) contracts.AttentionRequested {
	return contracts.AttentionRequested{
		NativeRef: ref, Kind: lifecycle.AttentionActionApproval,
		Action: "message.send", ActionDigest: identity.DigestOf([]byte("send this")), ExpiresAt: expires,
		Scope: identity.Scope{Class: identity.ScopeSubject, Target: "recipient"},
	}
}

// ingestNow ingests at the fixture time, so the rows a later real-time worker
// tick writes to are never stamped in the future.
func (f *fixture) ingestNow(t *testing.T, event contracts.Event) application.Ingestion {
	t.Helper()
	result, err := application.Ingest(f.ctx, f.pool, f.peer(), event, f.at)
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	return result
}

// raise ingests one attention.requested observation on the submission's stream.
func (f *fixture) raise(t *testing.T, rt *simulator.Runtime, s application.Submission, sequence uint64, payload contracts.AttentionRequested) application.Ingestion {
	t.Helper()
	return f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(sequence, contracts.EntryAttentionRequested, payload))))
}

// attention reads the single request raised against the submission's attempt.
func (f *fixture) attention(t *testing.T, s application.Submission) lifecycle.AttentionRequest {
	t.Helper()
	var text string
	if err := f.pool.QueryRow(f.ctx,
		`select attention_id::text from attention_request where tenant_id = $1 and attempt_id = $2`,
		f.tenant.String(), s.Attempt.ID.String()).Scan(&text); err != nil {
		t.Fatalf("attention query: %v", err)
	}
	id, err := ids.Parse[ids.Attention](text)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	request, err := store.GetAttention(f.ctx, f.pool, f.tenant, id)
	if err != nil {
		t.Fatalf("GetAttention() error = %v", err)
	}
	return request
}

func (f *fixture) decideCommand(nonce string, request lifecycle.AttentionRequest, kind lifecycle.DecisionKind) application.DecideAttention {
	return application.DecideAttention{
		Command:   application.Command{Actor: f.actor, Nonce: nonce, ExpectedVersion: request.Version},
		Attention: request.ID, Kind: kind, At: f.at.Add(2 * time.Minute),
	}
}

func (f *fixture) decide(t *testing.T, cmd application.DecideAttention) application.DecisionView {
	t.Helper()
	view, err := application.Decide(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	return view
}

// steer returns a runtime that accepts the decision steer: the delivery reuses
// the attempt's operation, so the steer runs on its own scripted runtime.
func (f *fixture) steer(t *testing.T, s application.Submission) *simulator.Runtime {
	t.Helper()
	rt, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider},
		simulator.RuntimeScenario{Operations: map[ids.OperationID]simulator.RuntimeOp{
			s.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered},
		}})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return rt
}

// deliver runs one dispatch pass with the steer-capable runtime and returns it.
func (f *fixture) deliver(t *testing.T, s application.Submission) *simulator.Runtime {
	t.Helper()
	rt := f.steer(t, s)
	f.tick(t, rt)
	return rt
}

func TestAttentionRequestedCreatesRequestAndWaits(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	expires := f.at.Add(time.Hour)
	got := f.raise(t, rt, s, 2, question("q-1", expires))
	if got.Applied != 1 || got.Refused != 0 {
		t.Fatalf("ingestion = %+v, want the request applied", got)
	}
	request := f.attention(t, s)
	switch {
	case request.Kind != lifecycle.AttentionQuestion || request.State != lifecycle.AttentionPending:
		t.Fatalf("request = %s/%s, want a pending question", request.Kind, request.State)
	case request.Attempt != s.Attempt.ID || request.Task != s.Task.ID:
		t.Fatalf("request = attempt %s task %s, want the submission's", request.Attempt, request.Task)
	case request.Version != 1 || request.PolicyGeneration != 1:
		t.Fatalf("request = v%d generation %d, want v1 generation 1", request.Version, request.PolicyGeneration)
	case !request.ExpiresAt.Equal(expires):
		t.Fatalf("request expires %s, want %s", request.ExpiresAt, expires)
	case len(request.Responders) != 1 || request.Responders[0] != f.principal:
		t.Fatalf("responders = %v, want the task owner %s", request.Responders, f.principal)
	}
	native, err := store.GetAttentionNative(f.ctx, f.pool, f.tenant, f.provider, "q-1")
	if err != nil || native.Attention != request.ID {
		t.Fatalf("correlation = %+v, %v; want the request", native, err)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskWaitingInput || attempt != lifecycle.AttemptWaitingInput {
		t.Fatalf("states = %s/%s, want waiting_input/waiting_input", task, attempt)
	}
}

// A runtime can lose its environment with an approval open. The question is moot
// then, and nothing else would end the wait.
func TestFailedTurnEndsATaskWaitingOnInput(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryTurnFinished, failedTurn("s-1", "t-1", f.at)))))
	if task, attempt := f.states(t, s); task != lifecycle.TaskFailed || attempt != lifecycle.AttemptFailed {
		t.Fatalf("states = %s/%s, want failed/failed", task, attempt)
	}
	if got := f.attention(t, s); got.State != lifecycle.AttentionSuperseded {
		t.Fatalf("request = %s, want superseded", got.State)
	}
}

func TestAttentionRequestedSecondBlockingRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	got := f.raise(t, rt, s, 3, question("q-2", f.at.Add(time.Hour)))
	if got.Applied != 0 || got.Refused != 1 {
		t.Fatalf("second request = %+v, want one refusal", got)
	}
	if n := f.count(t, "attention_request", "true"); n != 1 {
		t.Fatalf("requests = %d, want the first only", n)
	}
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'rejected' and reason = 'wrong_record'", s.Attempt.ID.String()); n != 1 {
		t.Fatalf("refusal evidence = %d, want 1", n)
	}
}

func TestAttentionRequestedNativeReferenceRaceHasOneReachableWait(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	first, firstRuntime := f.live(t, "n-1")
	second, secondRuntime := f.live(t, "n-2")
	f.ingestNow(t, f.event(firstRuntime, first, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
	)))
	f.ingestNow(t, f.event(secondRuntime, second, 1, simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-2"}),
	)))

	events := []contracts.Event{
		f.event(firstRuntime, first, 1, simulator.RowsFrame(false,
			row(2, contracts.EntryAttentionRequested, question("q-shared", f.at.Add(time.Hour))))),
		f.event(secondRuntime, second, 1, simulator.RowsFrame(false,
			row(2, contracts.EntryAttentionRequested, question("q-shared", f.at.Add(time.Hour))))),
	}
	type outcome struct {
		result application.Ingestion
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(events))
	var wg sync.WaitGroup
	for _, event := range events {
		wg.Add(1)
		go func(event contracts.Event) {
			defer wg.Done()
			<-start
			result, err := application.Ingest(f.ctx, f.pool, f.peer(), event, f.at)
			outcomes <- outcome{result: result, err: err}
		}(event)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	applied, refused := 0, 0
	for got := range outcomes {
		if got.err != nil {
			t.Fatalf("Ingest() error = %v", got.err)
		}
		applied += got.result.Applied
		refused += got.result.Refused
	}
	if applied != 1 || refused != 1 {
		t.Fatalf("concurrent ingestion = %d applied, %d refused; want one of each", applied, refused)
	}
	if n := f.count(t, "attention_request", "true"); n != 1 {
		t.Fatalf("requests = %d, want only the correlated request", n)
	}
	native, err := store.GetAttentionNative(f.ctx, f.pool, f.tenant, f.provider, "q-shared")
	if err != nil {
		t.Fatalf("GetAttentionNative() error = %v", err)
	}
	request, err := store.GetAttention(f.ctx, f.pool, f.tenant, native.Attention)
	if err != nil {
		t.Fatalf("GetAttention() error = %v", err)
	}
	states := map[ids.AttemptID]lifecycle.AttemptState{}
	for _, submission := range []application.Submission{first, second} {
		_, state := f.states(t, submission)
		states[submission.Attempt.ID] = state
	}
	if states[request.Attempt] != lifecycle.AttemptWaitingInput {
		t.Fatalf("correlated attempt state = %s, want waiting_input", states[request.Attempt])
	}
	other := first.Attempt.ID
	if other == request.Attempt {
		other = second.Attempt.ID
	}
	if states[other] != lifecycle.AttemptExecuting {
		t.Fatalf("losing attempt state = %s, want executing", states[other])
	}
}

func TestAttentionRequestedKindPayloads(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	// An approval must carry its action and digest: a bare one is refused.
	bare := contracts.AttentionRequested{NativeRef: "a-1", Kind: lifecycle.AttentionActionApproval, ExpiresAt: f.at.Add(time.Hour)}
	if got := f.raise(t, rt, s, 2, bare); got.Applied != 0 || got.Refused != 1 {
		t.Fatalf("bare approval = %+v, want a refusal", got)
	}
	// A non-approval may not carry an action.
	over := contracts.AttentionRequested{NativeRef: "q-1", Kind: lifecycle.AttentionQuestion,
		Action: "message.send", ActionDigest: identity.DigestOf([]byte("x")), ExpiresAt: f.at.Add(time.Hour)}
	if got := f.raise(t, rt, s, 3, over); got.Applied != 0 || got.Refused != 1 {
		t.Fatalf("question with action = %+v, want a refusal", got)
	}
	// Neither refusal left a request or a wait behind.
	if n := f.count(t, "attention_request", "true"); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("states = %s/%s, want running/executing", task, attempt)
	}
}

func TestAttentionRequestedExpiredRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	// The event time is f.at plus a minute, so an expiry at f.at is behind it.
	if got := f.raise(t, rt, s, 2, question("q-1", f.at)); got.Applied != 0 || got.Refused != 1 {
		t.Fatalf("expired request = %+v, want a refusal", got)
	}
	if n := f.count(t, "attention_request", "true"); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
	if n := f.count(t, "audit_event", "event = 'attention.requested' and outcome = 'rejected' and reason = 'request_expired'"); n != 1 {
		t.Fatalf("expiry evidence = %d, want 1", n)
	}
}

func TestDecisionDenyRecordsAndDelivers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, approval("a-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)

	view := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))
	if view.Nonce != "d-1" || view.AttentionID != request.ID.String() || view.Kind != string(lifecycle.DecisionDeny) ||
		view.State != string(lifecycle.AttentionDecided) || view.GrantID != "" {
		t.Fatalf("view = %+v, want a decided deny without a grant", view)
	}
	decided := f.attention(t, s)
	if decided.State != lifecycle.AttentionDecided || decided.Decision.IsZero() || decided.Version != request.Version+1 {
		t.Fatalf("request = %+v, want decided at the next version", decided)
	}
	// The delivery is a durable person-demand row, waiting for a worker.
	rows, err := store.OutboxReadyDispatch(f.ctx, f.pool, f.tenant, f.at.Add(time.Minute), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ready rows = %+v, %v; want the delivery", rows, err)
	}
	if row := rows[0]; row.Record != lifecycle.RecordAttention || row.Event != "decide" ||
		row.Demand != contracts.DemandPerson || row.Attempt != s.Attempt.ID || row.RecordID != request.ID.Untyped() {
		t.Fatalf("delivery row = %+v, want a person decide row for the request", row)
	}

	steered := f.deliver(t, s)
	sent := steered.Ledger().Sent()
	if len(sent) != 1 || sent[0].Method != contracts.MethodSteerTurn || sent[0].Attempt != s.Attempt.ID {
		t.Fatalf("sent = %+v, want one steer for the attempt", sent)
	}
	if delivered := f.attention(t, s); delivered.State != lifecycle.AttentionDelivered {
		t.Fatalf("request = %s, want delivered", delivered.State)
	}

	// The runtime reports the decision consumed: the wait closes, the work resumes.
	got := f.ingestNow(t, f.event(steered, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "a-1"}))))
	if got.Applied != 1 || got.Refused != 0 {
		t.Fatalf("ingestion = %+v, want the consumption applied", got)
	}
	if consumed := f.attention(t, s); consumed.State != lifecycle.AttentionConsumed {
		t.Fatalf("request = %s, want consumed", consumed.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("states = %s/%s, want running/executing", task, attempt)
	}
}

func TestDecisionAnswerCarriesDigest(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	answer := identity.DigestOf([]byte("the answer is 42"))
	cmd := f.decideCommand("d-1", request, lifecycle.DecisionAnswer)
	cmd.AnswerDigest = answer
	view := f.decide(t, cmd)
	if view.Kind != string(lifecycle.DecisionAnswer) || view.State != string(lifecycle.AttentionDecided) {
		t.Fatalf("view = %+v, want a decided answer", view)
	}
	decisionID, err := ids.Parse[ids.Decision](view.DecisionID)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	stored, err := store.GetDecision(f.ctx, f.pool, f.tenant, decisionID)
	if err != nil || stored.AnswerDigest != answer || stored.Principal != f.principal || stored.RequestVersion != request.Version {
		t.Fatalf("decision = %+v, %v; want the answer digest", stored, err)
	}
	// The delivery body carries the digest, so the runtime resolves the answer.
	rows, err := store.OutboxReadyDispatch(f.ctx, f.pool, f.tenant, f.at.Add(time.Minute), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ready rows = %+v, %v", rows, err)
	}
	var body map[string]any
	if err := json.Unmarshal(rows[0].Payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body["answer_digest"] != string(answer) || body["kind"] != string(lifecycle.DecisionAnswer) {
		t.Fatalf("payload = %v, want the answer digest", body)
	}
}

func TestDecisionApproveOnceIssuesAllowOnceGrant(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	// The approval's action is askable, so the grant the decision confers
	// issues under the tenant's current generation.
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, uow, store.PolicyUpdate{
			Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 1,
			Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAsk},
			Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAsk},
			At:       f.at,
		})
		return err
	})
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, approval("a-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)

	view := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionApproveOnce))
	if view.GrantID == "" || view.State != string(lifecycle.AttentionDecided) {
		t.Fatalf("view = %+v, want a decided approval with a grant", view)
	}
	grantID, err := ids.Parse[ids.Grant](view.GrantID)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	grant, err := store.GetGrant(f.ctx, f.pool, f.tenant, grantID)
	if err != nil {
		t.Fatalf("GetGrant() error = %v", err)
	}
	switch {
	case grant.Mode != identity.GrantAllowOnce || grant.Action != identity.ActionClass("message.send"):
		t.Fatalf("grant = %s/%s, want allow_once message.send", grant.Mode, grant.Action)
	case grant.Attempt != s.Attempt.ID || grant.Provider != f.provider || grant.Operation != request.Operation || grant.Operation == s.Operation.ID:
		t.Fatalf("grant = %+v, want the request's attempt, provider and operation", grant)
	case grant.InputDigest != identity.DigestOf([]byte("send this")) || grant.GrantedBy != f.principal:
		t.Fatalf("grant = %+v, want the observed digest and the approver", grant)
	case grant.PolicyGeneration != 1 || !grant.ExpiresAt.Equal(f.at.Add(time.Hour)):
		t.Fatalf("grant = generation %d expires %s, want generation 1 and the request's expiry", grant.PolicyGeneration, grant.ExpiresAt)
	}
	// The replay returns the same grant, and only one row exists.
	replay := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionApproveOnce))
	if replay.GrantID != view.GrantID {
		t.Fatalf("replay grant = %s, want %s", replay.GrantID, view.GrantID)
	}
	if n := f.count(t, "authority_grant", "true"); n != 1 {
		t.Fatalf("grants = %d, want 1", n)
	}
}

func TestDecisionStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))
	// A second decision on the pre-decide version refuses with evidence.
	stale := f.decideCommand("d-2", request, lifecycle.DecisionDeny)
	if _, err := application.Decide(f.ctx, f.pool, stale); err != nil {
		requireRefusal(t, err, lifecycle.RejectVersionConflict)
	}
	if n := f.count(t, "audit_event", "record_id = $2 and outcome = 'rejected' and event = 'decide'", request.ID.String()); n != 1 {
		t.Fatalf("conflict evidence = %d, want 1", n)
	}
}

func TestDecisionWrongResponderForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	// A second principal of the tenant exists but is not a responder.
	other := identity.Principal{Tenant: f.tenant, ID: mustID[ids.Principal](t), Kind: identity.PrincipalHuman}
	otherActor, err := identity.NewAuthorizationContext(other)
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	f.seed(t, func(uow *store.UnitOfWork) error {
		return store.InsertPrincipal(f.ctx, uow, other)
	})
	cmd := f.decideCommand("d-1", request, lifecycle.DecisionDeny)
	cmd.Actor = otherActor
	if _, err := application.Decide(f.ctx, f.pool, cmd); err != nil {
		requireRefusal(t, err, lifecycle.RejectUnauthorizedActor)
	}
	if got := f.attention(t, s); got.State != lifecycle.AttentionPending {
		t.Fatalf("request = %s, want still pending", got.State)
	}
	// The owner answers unaffected.
	if view := f.decide(t, f.decideCommand("d-2", request, lifecycle.DecisionDeny)); view.State != string(lifecycle.AttentionDecided) {
		t.Fatalf("view = %+v, want decided", view)
	}
}

func TestDecisionReplayReturnsRecordedDecision(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	first := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))
	replay := f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))
	if replay.DecisionID != first.DecisionID || replay.State != first.State || replay.GrantID != first.GrantID {
		t.Fatalf("replay = %+v, want the recorded decision %+v", replay, first)
	}
	if n := f.count(t, "decision", "true"); n != 1 {
		t.Fatalf("decisions = %d, want 1", n)
	}
	if n := f.count(t, "outbox", "record_id = $2", request.ID.String()); n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}
}

func TestDecisionStalePolicyForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	// The policy moved on after the request was raised.
	f.seed(t, func(uow *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, uow, store.PolicyUpdate{
			Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 2, At: f.at,
		})
		return err
	})
	if _, err := application.Decide(f.ctx, f.pool, f.decideCommand("d-1", request, lifecycle.DecisionDeny)); err != nil {
		requireRefusal(t, err, lifecycle.RejectStalePolicy)
	}
}

func TestDecisionConsumedBeforeDeliveryRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))
	// The runtime cannot have consumed a decision that was never delivered.
	early := f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "q-1"}))))
	if early.Applied != 0 || early.Refused != 1 {
		t.Fatalf("early consumption = %+v, want a refusal", early)
	}
	// Once delivered, the same observation applies.
	steered := f.deliver(t, s)
	late := f.ingestNow(t, f.event(steered, s, 1, simulator.RowsFrame(false,
		row(4, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "q-1"}))))
	if late.Applied != 1 || late.Refused != 0 {
		t.Fatalf("late consumption = %+v, want applied", late)
	}
	if consumed := f.attention(t, s); consumed.State != lifecycle.AttentionConsumed {
		t.Fatalf("request = %s, want consumed", consumed.State)
	}
}

func TestDecisionConsumedWhileDispatchingProvesDelivery(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	f.decide(t, f.decideCommand("d-1", request, lifecycle.DecisionDeny))

	// The send has started, but its reply has not committed yet. The bound
	// runtime's consumption observation is stronger evidence than that reply:
	// it proves the exact native request received the decision.
	f.seed(t, func(uow *store.UnitOfWork) error {
		current, err := store.GetAttention(f.ctx, uow, f.tenant, request.ID)
		if err != nil {
			return err
		}
		auditID, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		_, _, err = store.ApplyAttention(f.ctx, uow, f.tenant, request.ID, lifecycle.AttentionDispatch, store.Transition{
			ExpectedVersion: current.Version, At: f.at, Actor: lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: f.provider}, AuditID: auditID,
		})
		return err
	})
	event := f.event(rt, s, 1, simulator.RowsFrame(false,
		row(3, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "q-1"})))
	if got := f.ingestNow(t, event); got.Applied != 1 || got.Refused != 0 {
		t.Fatalf("consumption = %+v, want applied", got)
	}
	if consumed := f.attention(t, s); consumed.State != lifecycle.AttentionConsumed {
		t.Fatalf("request = %s, want consumed", consumed.State)
	}
	if task, attempt := f.states(t, s); task != lifecycle.TaskRunning || attempt != lifecycle.AttemptExecuting {
		t.Fatalf("states = %s/%s, want running/executing", task, attempt)
	}
	// Replaying the identical entry advances nothing, and the pending worker
	// row acks without steering a decision that was already consumed.
	if got := f.ingestNow(t, event); got.Duplicates == 0 || got.Applied != 0 {
		t.Fatalf("replay = %+v, want a duplicate", got)
	}
	steered := f.deliver(t, s)
	if sent := steered.Ledger().Sent(); len(sent) != 0 {
		t.Fatalf("Sent() = %d requests, want no duplicate steer", len(sent))
	}
}

func TestDecisionConcurrentDuplicatesConverge(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s, rt := f.executing(t, "n-1")
	f.raise(t, rt, s, 2, question("q-1", f.at.Add(time.Hour)))
	request := f.attention(t, s)
	const racers = 4
	results := make([]application.DecisionView, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = application.Decide(f.ctx, f.pool, f.decideCommand("d-race", request, lifecycle.DecisionDeny))
		}()
	}
	wg.Wait()
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if results[i].DecisionID != results[0].DecisionID {
			t.Fatalf("racers recorded different decisions: %s and %s", results[i].DecisionID, results[0].DecisionID)
		}
	}
	if n := f.count(t, "decision", "true"); n != 1 {
		t.Fatalf("decisions = %d, want 1", n)
	}
	if n := f.count(t, "outbox", "record_id = $2", request.ID.String()); n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}
}
