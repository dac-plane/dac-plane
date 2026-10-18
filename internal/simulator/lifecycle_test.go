package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// traceIDs fixes every identifier the flagship trace uses, so the same
// scenario run twice produces the same evidence. The trace records
// identifiers; nothing in it generates one.
type traceIDs struct {
	tenant    ids.TenantID
	provider  ids.ProviderID
	store     ids.ProviderID
	task      ids.TaskID
	attempt   ids.AttemptID
	attention ids.AttentionID
	artifact  ids.ArtifactID
	principal ids.PrincipalID

	provision ids.OperationID
	observe   ids.OperationID
	turn      ids.OperationID
	upload    ids.OperationID
	commit    ids.OperationID

	provisionDelivery ids.DeliveryID
	observeDelivery   ids.DeliveryID
	hookDelivery      ids.DeliveryID
	turnDelivery      ids.DeliveryID
	uploadDelivery    ids.DeliveryID
	commitDelivery    ids.DeliveryID
}

func newTraceIDs(t *testing.T) traceIDs {
	t.Helper()
	return traceIDs{
		tenant:    mustID[ids.Tenant](t),
		provider:  mustID[ids.Provider](t),
		store:     mustID[ids.Provider](t),
		task:      mustID[ids.Task](t),
		attempt:   mustID[ids.Attempt](t),
		attention: mustID[ids.Attention](t),
		artifact:  mustID[ids.Artifact](t),
		principal: mustID[ids.Principal](t),

		provision: mustID[ids.Operation](t),
		observe:   mustID[ids.Operation](t),
		turn:      mustID[ids.Operation](t),
		upload:    mustID[ids.Operation](t),
		commit:    mustID[ids.Operation](t),

		provisionDelivery: mustID[ids.Delivery](t),
		observeDelivery:   mustID[ids.Delivery](t),
		hookDelivery:      mustID[ids.Delivery](t),
		turnDelivery:      mustID[ids.Delivery](t),
		uploadDelivery:    mustID[ids.Delivery](t),
		commitDelivery:    mustID[ids.Delivery](t),
	}
}

// config returns the simulator config for one provider identity.
func (l traceIDs) config(provider ids.ProviderID) simulator.Config {
	return simulator.Config{Tenant: l.tenant, Provider: provider, Hello: simulator.DefaultHello()}
}

// request scripts one delivery for the trace's attempt.
func (l traceIDs) request(operation ids.OperationID, delivery ids.DeliveryID, role contracts.Role, method contracts.Method, arguments []byte) contracts.Request {
	return contracts.Request{
		Operation: operation, Delivery: delivery, Tenant: l.tenant, Attempt: l.attempt,
		Role: role, Method: method, Demand: contracts.DemandPerson, Arguments: arguments,
		Deadline: testNow.Add(time.Minute),
	}
}

// traceCounts is what each simulator's ledger recorded for the trace.
type traceCounts struct {
	RuntimeDeliveries     int
	RuntimeEffects        int
	EnvironmentDeliveries int
	EnvironmentEffects    int
	EnforcementDeliveries int
	EnforcementEffects    int
	ArtifactDeliveries    int
	ArtifactEffects       int
	Subscriptions         int
}

// traceOutcome is everything one run of the trace observed: the product
// machines' records, the provider roles' replies and the ledgers' evidence.
type traceOutcome struct {
	Negotiated contracts.Lifecycle
	Decision   contracts.HookDecision
	Turn       contracts.Reply
	Frames     []contracts.StreamFrame
	Entries    []contracts.Entry
	Duplicates int
	TurnEnd    lifecycle.TurnEnd
	Operation  lifecycle.ProviderOperation
	Attempt    lifecycle.Attempt
	Manifest   lifecycle.ArtifactManifest
	Admission  lifecycle.Admission
	Effects    []simulator.Effect
	Counts     traceCounts
}

// runTrace drives one task, attempt and operation through all four provider
// roles: negotiation, provisioning, a governed dispatch, stream ingestion,
// resolution from the outcome entry, evidence and reconciliation. It is pure
// over its identifiers: the same traceIDs always produce the same outcome.
func runTrace(t *testing.T, trace traceIDs) traceOutcome {
	t.Helper()
	ctx := context.Background()
	host := trace.config(trace.provider)
	storeConfig := trace.config(trace.store)
	peer := contracts.Peer{Tenant: trace.tenant, Provider: trace.provider}

	// The evidence the runtime's stream carries: progress, an attention
	// request, the turn end and the operation outcome.
	attention, err := lifecycle.NewAttentionRequest(lifecycle.AttentionRequest{
		ID: trace.attention, Tenant: trace.tenant, Task: trace.task, Attempt: trace.attempt,
		Kind: lifecycle.AttentionQuestion, PolicyGeneration: 1,
		Responders: []ids.PrincipalID{trace.principal}, ExpiresAt: testNow.Add(time.Hour),
	}, testNow)
	if err != nil {
		t.Fatalf("NewAttentionRequest() error = %v", err)
	}
	turnEnd := lifecycle.TurnEnd{
		Tenant: trace.tenant, Attempt: trace.attempt, Provider: trace.provider,
		SessionRef: "session-1", TurnRef: "turn-1", Outcome: lifecycle.TurnCompleted, EndedAt: testNow,
	}
	if err := turnEnd.Validate(); err != nil {
		t.Fatalf("turnEnd.Validate() error = %v", err)
	}
	rows := []simulator.Row{
		{Sequence: 1, Updated: 1, Kind: "progress", Payload: `{"step":1}`},
		{Sequence: 2, Updated: 2, Kind: "attention", Payload: string(marshalArguments(t, attention))},
		{Sequence: 3, Updated: 3, Kind: "turn_end", Payload: string(marshalArguments(t, turnEnd))},
		{Sequence: 4, Updated: 4, Kind: "operation_outcome", Payload: `{"state":"succeeded"}`},
	}
	frames := []contracts.StreamFrame{
		simulator.ConnectedFrame(testNow),
		simulator.RowsFrame(false, rows...),
		simulator.HeartbeatFrame(testNow),
	}

	// 1. One runtime negotiates the protocol before any work is dispatched.
	runtimeSim := newRuntime(t, host, map[ids.OperationID]simulator.RuntimeOp{
		trace.turn: {
			Reply:  simulator.Unknown(scriptedFailure()),
			Effect: simulator.TurnStarted,
			Stream: &simulator.StreamScript{Frames: frames},
		},
	})
	negotiated, failure := contracts.Negotiate(simulator.DefaultHello(), runtimeSim.Hello())
	if failure != nil {
		t.Fatalf("Negotiate() failure = %v", failure)
	}

	// 2. The environment provisions the attempt and reports it running; the
	// observation applies no effect.
	environmentSim := newEnvironment(t, host, map[ids.OperationID]simulator.EnvironmentOp{
		trace.provision: {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned},
		trace.observe:   {Observations: []lifecycle.EnvironmentObservation{observation(host, trace.attempt, 1, lifecycle.RunRunning)}},
	})

	// 3. The attempt: provisioned, started, running.
	attempt, err := lifecycle.NewAttempt(trace.attempt, trace.tenant, trace.task, 1, testNow)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt, _, err = attempt.Apply(lifecycle.AttemptProvision, attempt.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(provision) error = %v", err)
	}
	if _, err := environmentSim.Send(ctx, trace.request(trace.provision, trace.provisionDelivery, contracts.RoleEnvironment, contracts.MethodProvision, nil)); err != nil {
		t.Fatalf("Send(provision) error = %v", err)
	}
	if _, err := environmentSim.Send(ctx, trace.request(trace.observe, trace.observeDelivery, contracts.RoleEnvironment, contracts.MethodObserve, nil)); err != nil {
		t.Fatalf("Send(observe) error = %v", err)
	}
	attempt, _, err = attempt.Apply(lifecycle.AttemptStart, attempt.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}
	attempt, _, err = attempt.Apply(lifecycle.AttemptRuntimeStarted, attempt.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(runtime_started) error = %v", err)
	}

	// 4. The governed action is allowed by the enforcement provider before
	// anything is dispatched: the hook answers the same operation identity
	// the runtime would receive.
	hook := hookRequest(host, trace.turn, trace.attempt, []byte(`{"turn":"start"}`))
	enforcementSim := newEnforcement(t, host, map[ids.OperationID]simulator.EnforcementOp{
		trace.turn: {Response: simulator.AllowHook()},
	})
	hookReply, err := enforcementSim.Send(ctx, trace.request(trace.turn, trace.hookDelivery, contracts.RoleEnforcement, contracts.MethodPreAction, marshalArguments(t, hook)))
	if err != nil {
		t.Fatalf("Send(pre_action) error = %v", err)
	}
	var hookResponse contracts.HookResponse
	if err := json.Unmarshal(hookReply.Result, &hookResponse); err != nil {
		t.Fatalf("hook reply is not a HookResponse: %v", err)
	}
	decision, err := contracts.DecidePreAction(hook, &hookResponse, testNow)
	if err != nil {
		t.Fatalf("DecidePreAction() error = %v", err)
	}
	if !decision.Allowed() {
		t.Fatalf("hook decision = %+v, want allowed", decision.Evidence)
	}

	// 5. The dispatch: the provider did the work, but the product never
	// heard a durable acceptance, so the operation and the attempt both enter
	// reconciliation and the turn is never resent.
	operation, err := lifecycle.NewProviderOperation(trace.turn, trace.tenant, trace.attempt, trace.provider, testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	operation, _, err = operation.Apply(lifecycle.OperationDispatch, operation.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(dispatch) error = %v", err)
	}
	turnReply, err := runtimeSim.Send(ctx, trace.request(trace.turn, trace.turnDelivery, contracts.RoleRuntime, contracts.MethodStartTurn, nil))
	if err != nil {
		t.Fatalf("Send(start_turn) error = %v", err)
	}
	if turnReply.Durability != lifecycle.SendUnknown {
		t.Fatalf("turn reply = %+v, want unknown durability", turnReply)
	}
	operation, _, err = operation.Apply(lifecycle.OperationSendUnknown, operation.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(send_unknown) error = %v", err)
	}
	attempt, _, err = attempt.Apply(lifecycle.AttemptOutcomeUnknown, attempt.Version, "", testNow)
	if err != nil {
		t.Fatalf("Apply(outcome_unknown) error = %v", err)
	}

	// 6. The stream: every frame is bound to the recorded operation and
	// ingested through the contract path, and a replay is deduped.
	cursor := contracts.Cursor{Tenant: trace.tenant, Attempt: trace.attempt, Operation: trace.turn, Stream: "runtime", Generation: 1}
	stream, err := runtimeSim.Subscribe(ctx, cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	var (
		streamed   []contracts.StreamFrame
		entries    []contracts.Entry
		duplicates int
	)
	for {
		frame, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Next() error = %v", err)
		}
		streamed = append(streamed, frame)
		event := contracts.Event{
			Tenant: trace.tenant, Attempt: trace.attempt, Provider: trace.provider,
			Operation: trace.turn, Stream: "runtime", Generation: 1, Body: frame,
		}
		bound, failure := contracts.BindEvent(peer, event, operation)
		if failure != nil {
			t.Fatalf("BindEvent() failure = %v", failure)
		}
		ingestion, failure := contracts.Ingest(cursor, bound)
		if failure != nil {
			t.Fatalf("Ingest() failure = %v", failure)
		}
		cursor = ingestion.Cursor
		entries = append(entries, ingestion.Entries...)
		duplicates += ingestion.Duplicates
	}
	replay := contracts.Event{
		Tenant: trace.tenant, Attempt: trace.attempt, Provider: trace.provider,
		Operation: trace.turn, Stream: "runtime", Generation: 1,
		Body: simulator.RowsFrame(true, rows...),
	}
	bound, failure := contracts.BindEvent(peer, replay, operation)
	if failure != nil {
		t.Fatalf("BindEvent() on the replay failure = %v", failure)
	}
	replayed, failure := contracts.Ingest(cursor, bound)
	if failure != nil {
		t.Fatalf("Ingest() on the replay failure = %v", failure)
	}
	if !replayed.Replay {
		t.Fatalf("replayed ingestion Replay = false, want true")
	}
	duplicates += replayed.Duplicates

	// 7. The outcome entry resolves the uncertain operation: reconciliation
	// lands on the state the provider reported, and the turn end validates.
	var (
		streamedEnd lifecycle.TurnEnd
		reported    struct {
			State lifecycle.OperationState `json:"state"`
		}
	)
	for _, entry := range entries {
		switch entry.Kind {
		case "turn_end":
			if err := json.Unmarshal(entry.Payload, &streamedEnd); err != nil {
				t.Fatalf("turn-end entry payload error = %v", err)
			}
			if err := streamedEnd.Validate(); err != nil {
				t.Fatalf("streamed turn end invalid: %v", err)
			}
		case "operation_outcome":
			if err := json.Unmarshal(entry.Payload, &reported); err != nil {
				t.Fatalf("outcome entry payload error = %v", err)
			}
		}
	}
	operation, _, err = operation.Apply(lifecycle.OperationReconcile, operation.Version, reported.State, testNow)
	if err != nil {
		t.Fatalf("Apply(reconcile) error = %v", err)
	}

	// 8. The evidence: an authorized upload and a verified commit.
	content := []byte("turn transcript")
	manifest := lifecycle.ArtifactManifest{
		Artifact: trace.artifact, Tenant: trace.tenant, Task: trace.task, Attempt: trace.attempt,
		Operation: trace.commit, Producer: trace.provider, Kind: lifecycle.ArtifactCodeChange,
		Digest: identity.DigestOf(content), Size: uint64(len(content)), MediaType: "text/plain",
		WriterEpoch: 1,
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest.Validate() error = %v", err)
	}
	artifactSim := newArtifactStore(t, storeConfig, map[ids.OperationID]simulator.ArtifactOp{
		trace.upload: {Reply: simulator.Accepted(nil), Effect: simulator.UploadAuthorized},
		trace.commit: {
			Reply:  simulator.StoredReply(lifecycle.StoredObject{Present: true, Digest: manifest.Digest, Size: manifest.Size}),
			Effect: simulator.ManifestCommitted,
		},
	})
	if _, err := artifactSim.Send(ctx, trace.request(trace.upload, trace.uploadDelivery, contracts.RoleArtifactStore, contracts.MethodAuthorizeUpload, nil)); err != nil {
		t.Fatalf("Send(authorize_upload) error = %v", err)
	}
	commitReply, err := artifactSim.Send(ctx, trace.request(trace.commit, trace.commitDelivery, contracts.RoleArtifactStore, contracts.MethodCommitManifest, nil))
	if err != nil {
		t.Fatalf("Send(commit_manifest) error = %v", err)
	}
	var stored lifecycle.StoredObject
	if err := json.Unmarshal(commitReply.Result, &stored); err != nil {
		t.Fatalf("commit reply is not a StoredObject: %v", err)
	}
	admission, err := lifecycle.AdmitArtifact(manifest, stored, manifest.WriterEpoch, nil, testNow)
	if err != nil {
		t.Fatalf("AdmitArtifact() error = %v", err)
	}

	// 9. Reconcile the attempt against what the stream established.
	attempt, _, err = attempt.Apply(lifecycle.AttemptReconciled, attempt.Version, lifecycle.AttemptCompleted, testNow)
	if err != nil {
		t.Fatalf("Apply(reconciled) error = %v", err)
	}

	effects := slices.Concat(
		runtimeSim.Ledger().Applied(),
		environmentSim.Ledger().Applied(),
		enforcementSim.Ledger().Applied(),
		artifactSim.Ledger().Applied(),
	)
	return traceOutcome{
		Negotiated: negotiated,
		Decision:   decision,
		Turn:       turnReply,
		Frames:     streamed,
		Entries:    entries,
		Duplicates: duplicates,
		TurnEnd:    streamedEnd,
		Operation:  operation,
		Attempt:    attempt,
		Manifest:   manifest,
		Admission:  admission,
		Effects:    effects,
		Counts: traceCounts{
			RuntimeDeliveries:     runtimeSim.Ledger().Deliveries(trace.turn),
			RuntimeEffects:        runtimeSim.Ledger().Effects(trace.turn),
			EnvironmentDeliveries: environmentSim.Ledger().Deliveries(trace.provision) + environmentSim.Ledger().Deliveries(trace.observe),
			EnvironmentEffects:    environmentSim.Ledger().Effects(trace.provision) + environmentSim.Ledger().Effects(trace.observe),
			EnforcementDeliveries: enforcementSim.Ledger().Deliveries(trace.turn),
			EnforcementEffects:    enforcementSim.Ledger().Effects(trace.turn),
			ArtifactDeliveries:    artifactSim.Ledger().Deliveries(trace.upload) + artifactSim.Ledger().Deliveries(trace.commit),
			ArtifactEffects:       artifactSim.Ledger().Effects(trace.upload) + artifactSim.Ledger().Effects(trace.commit),
			Subscriptions:         len(runtimeSim.Ledger().Subscriptions()),
		},
	}
}

func TestSimulatedLifecycleTrace(t *testing.T) {
	trace := newTraceIDs(t)

	first := runTrace(t, trace)
	second := runTrace(t, trace)

	// One scripted lifecycle: negotiation, provisions, a governed dispatch,
	// stream ingestion, resolution, evidence and reconciliation.
	if first.Negotiated.Phase != contracts.PhaseReady || first.Negotiated.Version != 1 {
		t.Fatalf("Negotiated = %+v, want ready at version 1", first.Negotiated)
	}
	if !first.Decision.Allowed() {
		t.Fatalf("hook decision = %+v, want allowed", first.Decision.Evidence)
	}
	if first.Operation.State != lifecycle.OperationSucceeded || first.Operation.Version != 4 {
		t.Fatalf("Operation = %+v, want succeeded at version 4", first.Operation)
	}
	if first.Attempt.State != lifecycle.AttemptCompleted || first.Attempt.Version != 6 {
		t.Fatalf("Attempt = %+v, want completed at version 6", first.Attempt)
	}
	if first.TurnEnd.Outcome != lifecycle.TurnCompleted {
		t.Fatalf("TurnEnd.Outcome = %q, want completed", first.TurnEnd.Outcome)
	}
	if len(first.Frames) != 3 || len(first.Entries) != 4 || first.Duplicates != 4 {
		t.Fatalf("stream = %d frames, %d entries, %d duplicates; want 3, 4, 4", len(first.Frames), len(first.Entries), first.Duplicates)
	}
	if first.Admission.Outcome != lifecycle.CommitCommitted {
		t.Fatalf("Admission = %+v, want committed", first.Admission)
	}
	if err := first.Admission.Artifact.Validate(); err != nil {
		t.Fatalf("admitted artifact invalid: %v", err)
	}

	kinds := make([]simulator.EffectKind, 0, len(first.Effects))
	for _, effect := range first.Effects {
		kinds = append(kinds, effect.Kind)
	}
	wantKinds := []simulator.EffectKind{
		simulator.TurnStarted,
		simulator.EnvironmentProvisioned,
		simulator.HookDecision,
		simulator.UploadAuthorized,
		simulator.ManifestCommitted,
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("effect kinds = %v, want %v", kinds, wantKinds)
	}
	wantCounts := traceCounts{
		RuntimeDeliveries: 1, RuntimeEffects: 1,
		EnvironmentDeliveries: 2, EnvironmentEffects: 1,
		EnforcementDeliveries: 1, EnforcementEffects: 1,
		ArtifactDeliveries: 2, ArtifactEffects: 2,
		Subscriptions: 1,
	}
	if first.Counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", first.Counts, wantCounts)
	}

	// The whole trace, run again on the same scenario, produces the same
	// evidence: replies, frames, ingested entries, machine records, effects.
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("the trace ran twice with different results:\nfirst  = %+v\nsecond = %+v", first, second)
	}
}
