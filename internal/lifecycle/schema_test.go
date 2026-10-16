package lifecycle

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

const schemaDir = "../../contracts/jsonschema/lifecycle"

var (
	taskContract      = schematest.GoContract[Task](filepath.Join(schemaDir, "task.schema.json"))
	attemptContract   = schematest.GoContract[Attempt](filepath.Join(schemaDir, "attempt.schema.json"))
	attentionContract = schematest.GoContract[AttentionRequest](filepath.Join(schemaDir, "attention-request.schema.json"))
	decisionContract  = schematest.GoContract[Decision](filepath.Join(schemaDir, "decision.schema.json"))
	auditContract     = schematest.GoContract[AuditEvent](filepath.Join(schemaDir, "audit-event.schema.json"))
	bindingContract   = schematest.GoContract[Binding](filepath.Join(schemaDir, "binding.schema.json"))
	operationContract = schematest.GoContract[ProviderOperation](filepath.Join(schemaDir, "provider-operation.schema.json"))
	turnEndContract   = schematest.GoContract[TurnEnd](filepath.Join(schemaDir, "turn-end.schema.json"))
	observeContract   = schematest.GoContract[EnvironmentObservation](filepath.Join(schemaDir, "environment-observation.schema.json"))
	harnessContract   = schematest.GoContract[HarnessQualification](filepath.Join(schemaDir, "harness-qualification.schema.json"))
	migrationContract = schematest.GoContract[MigrationPass](filepath.Join(schemaDir, "migration-pass.schema.json"))
	claimContract     = schematest.GoContract[MaintenanceClaim](filepath.Join(schemaDir, "maintenance-claim.schema.json"))
)

type record struct {
	contract schematest.Contract
	data     []byte
}

func validRecords(t *testing.T) map[string]record {
	t.Helper()
	task := newTask(t)
	attempt := driveAttempt(t, newAttempt(t), AttemptProvision, AttemptRequestCancel, AttemptRuntimeStopped)
	f := newAttentionFixture(t)
	decided, change, err := f.request.Decide(f.decision, testGeneration)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	answer := f.decision
	answer.Kind = DecisionAnswer
	answer.AnswerDigest = testDigest
	question := f.request
	question.Kind = AttentionQuestion
	question.Provider = ids.ProviderID{}
	question.Operation = ids.OperationID{}
	question.ActionDigest = ""

	applied := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: f.responder}, testNow)
	_, _, rejection := task.Apply(TaskAccept, task.Version, "", testNow)
	rejected := rejection.(*TransitionError).Audit(newID[ids.Audit](t), Actor{Kind: ActorProvider, Provider: newID[ids.Provider](t)}, testNow)

	turnBinding := newBinding(t)
	turnBinding.Kind, turnBinding.NativeRef, turnBinding.SessionRef = ResourceRuntimeTurn, "turn-1", "session-1"
	uncertain := newOperation(t)
	uncertain, _, _ = uncertain.Apply(OperationDispatch, uncertain.Version, "", testNow)
	uncertain, _, _ = uncertain.Apply(OperationSendUnknown, uncertain.Version, "", testNow)
	awaiting := TurnEnd{
		Tenant: task.Tenant, Attempt: attempt.ID, Provider: newID[ids.Provider](t),
		SessionRef: "session-1", TurnRef: "turn-1", Outcome: TurnAwaitingInput, EndedAt: testNow,
	}
	failedTurn := awaiting
	failedTurn.Outcome = TurnFailed
	failedTurn.Failure = &TurnFailure{Code: TurnFailureTimeout, Summary: "turn timed out", DetailsDigest: testDigest}
	observation := EnvironmentObservation{
		Tenant: task.Tenant, Attempt: attempt.ID, Provider: awaiting.Provider,
		ResourceRef: "box-1", State: RunHibernated, Sequence: 3, ObservedAt: testNow,
	}
	harness := HarnessQualification{Tenant: task.Tenant, Provider: awaiting.Provider, Mode: HarnessShadow, Version: 2, UpdatedAt: testNow}
	claimed, _, err := newClaim(t).Claim("worker-a", time.Minute, testNow)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	return map[string]record{
		"session binding":     {bindingContract, schematest.Marshal(t, newBinding(t))},
		"turn binding":        {bindingContract, schematest.Marshal(t, turnBinding)},
		"uncertain operation": {operationContract, schematest.Marshal(t, uncertain)},
		"awaiting turn":       {turnEndContract, schematest.Marshal(t, awaiting)},
		"failed turn":         {turnEndContract, schematest.Marshal(t, failedTurn)},
		"observation":         {observeContract, schematest.Marshal(t, observation)},
		"harness":             {harnessContract, schematest.Marshal(t, harness)},
		"migration":           {migrationContract, schematest.Marshal(t, newMigration(t))},
		"scheduled claim":     {claimContract, schematest.Marshal(t, newClaim(t))},
		"held claim":          {claimContract, schematest.Marshal(t, claimed)},
		"task":                {taskContract, schematest.Marshal(t, task)},
		"cancelling attempt":  {attemptContract, schematest.Marshal(t, attempt)},
		"pending approval":    {attentionContract, schematest.Marshal(t, f.request)},
		"decided approval":    {attentionContract, schematest.Marshal(t, decided)},
		"pending question":    {attentionContract, schematest.Marshal(t, question)},
		"approval decision":   {decisionContract, schematest.Marshal(t, f.decision)},
		"answer decision":     {decisionContract, schematest.Marshal(t, answer)},
		"applied audit":       {auditContract, schematest.Marshal(t, applied)},
		"rejected audit":      {auditContract, schematest.Marshal(t, rejected)},
		"system audit":        {auditContract, schematest.Marshal(t, change.Audit(newID[ids.Audit](t), testSystem, testNow))},
	}
}

func TestSchemasAcceptValidGoRecords(t *testing.T) {
	for name, r := range validRecords(t) {
		t.Run(name, func(t *testing.T) {
			if err := r.contract.GoAccept(r.data); err != nil {
				t.Fatalf("Go rejected its own record: %v", err)
			}
			if err := schematest.Accepts(t, schematest.Compile(t, r.contract.Schema), r.data); err != nil {
				t.Fatalf("schema rejected Go record %s: %v", r.data, err)
			}
		})
	}
}

func TestSchemaAndGoRejectTheSameInvalidDocuments(t *testing.T) {
	valid := validRecords(t)
	otherID := newID[ids.Operation](t).String()
	cases := []struct {
		name   string
		base   string
		change func(map[string]any)
	}{
		{"task unknown state", "task", func(d map[string]any) { d["state"] = "paused" }},
		{"task zero version", "task", func(d map[string]any) { d["version"] = 0 }},
		{"task missing owner", "task", func(d map[string]any) { delete(d, "owner_id") }},
		{"task unknown field", "task", func(d map[string]any) { d["objective"] = "x" }},
		{"task auto accept field", "task", func(d map[string]any) { d["auto_accept"] = true }},
		{"attempt zero number", "cancelling attempt", func(d map[string]any) { d["number"] = 0 }},
		{"attempt removed lost state", "cancelling attempt", func(d map[string]any) { d["state"] = "lost" }},
		{"attempt stop outside cancellation", "cancelling attempt", func(d map[string]any) { d["state"] = "executing" }},
		{"attempt stop flag not boolean", "cancelling attempt", func(d map[string]any) { d["runtime_stopped"] = "yes" }},
		{"approval without operation", "pending approval", func(d map[string]any) { delete(d, "operation_id") }},
		{"approval without digest", "pending approval", func(d map[string]any) { delete(d, "action_digest") }},
		{"approval without provider", "pending approval", func(d map[string]any) { delete(d, "provider_id") }},
		{"question with provider", "pending question", func(d map[string]any) { d["provider_id"] = otherID }},
		{"attention without policy generation", "pending approval", func(d map[string]any) { delete(d, "policy_generation") }},
		{"question with operation", "pending question", func(d map[string]any) { d["operation_id"] = otherID }},
		{"no responders", "pending approval", func(d map[string]any) { d["responders"] = []any{} }},
		{"pending with decision", "pending approval", func(d map[string]any) { d["decision_id"] = otherID }},
		{"decided without decision", "decided approval", func(d map[string]any) { delete(d, "decision_id") }},
		{"attention unknown kind", "pending approval", func(d map[string]any) { d["kind"] = "survey" }},
		{"decision unknown kind", "approval decision", func(d map[string]any) { d["kind"] = "approve_always" }},
		{"answer without digest", "answer decision", func(d map[string]any) { delete(d, "answer_digest") }},
		{"approval with answer digest", "approval decision", func(d map[string]any) { d["answer_digest"] = testDigest }},
		{"decision zero version", "approval decision", func(d map[string]any) { d["request_version"] = 0 }},
		{"applied audit with reason", "applied audit", func(d map[string]any) { d["reason"] = "version_conflict" }},
		{"applied audit without target", "applied audit", func(d map[string]any) { delete(d, "to_state") }},
		{"rejected audit with target", "rejected audit", func(d map[string]any) { d["to_state"] = "succeeded" }},
		{"rejected audit unknown reason", "rejected audit", func(d map[string]any) { d["reason"] = "nope" }},
		{"audit unknown record", "applied audit", func(d map[string]any) { d["record"] = "budget" }},
		{"audit principal actor without id", "applied audit", func(d map[string]any) { d["actor"] = map[string]any{"kind": "principal"} }},
		{"audit system actor with id", "system audit", func(d map[string]any) {
			d["actor"] = map[string]any{"kind": "system", "provider_id": otherID}
		}},
		{"audit malformed time", "applied audit", func(d map[string]any) { d["occurred_at"] = time.Kitchen }},
		{"binding unknown kind", "session binding", func(d map[string]any) { d["kind"] = "task" }},
		{"binding missing operation", "session binding", func(d map[string]any) { delete(d, "operation_id") }},
		{"session binding with session ref", "session binding", func(d map[string]any) { d["session_ref"] = "s" }},
		{"turn binding without session ref", "turn binding", func(d map[string]any) { delete(d, "session_ref") }},
		{"binding empty native ref", "session binding", func(d map[string]any) { d["native_ref"] = "" }},
		{"operation unknown state", "uncertain operation", func(d map[string]any) { d["state"] = "lost" }},
		{"operation zero version", "uncertain operation", func(d map[string]any) { d["version"] = 0 }},
		{"failed turn without failure", "failed turn", func(d map[string]any) { delete(d, "failure") }},
		{"awaiting turn with failure", "failed turn", func(d map[string]any) { d["outcome"] = "awaiting_input" }},
		{"turn failure unknown code", "failed turn", func(d map[string]any) {
			d["failure"] = map[string]any{"code": "oom", "summary": "x"}
		}},
		{"turn failure raw details", "failed turn", func(d map[string]any) {
			d["failure"] = map[string]any{"code": "timeout", "summary": "x", "details": "stack trace"}
		}},
		{"turn failure empty summary", "failed turn", func(d map[string]any) {
			d["failure"] = map[string]any{"code": "timeout", "summary": ""}
		}},
		{"observation unknown state", "observation", func(d map[string]any) { d["state"] = "paused" }},
		{"observation zero sequence", "observation", func(d map[string]any) { d["sequence"] = 0 }},
		{"harness unknown mode", "harness", func(d map[string]any) { d["mode"] = "canary" }},
		{"migration unknown state", "migration", func(d map[string]any) { d["state"] = "paused" }},
		{"migration missing target", "migration", func(d map[string]any) { delete(d, "to_provider_id") }},
		{"scheduled claim with claimant", "scheduled claim", func(d map[string]any) { d["claimant"] = "worker-a" }},
		{"held claim without expiry", "held claim", func(d map[string]any) { delete(d, "expires_at") }},
		{"held claim without claimant", "held claim", func(d map[string]any) { delete(d, "claimant") }},
		{"held claim zero epoch", "held claim", func(d map[string]any) { d["epoch"] = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := valid[tc.base]
			if !ok {
				t.Fatalf("unknown base record %q", tc.base)
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

func TestSchemaEnumsMatchGoConstants(t *testing.T) {
	for _, tc := range []struct {
		file string
		path []string
		want []string
	}{
		{"task.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(TaskStates())},
		{"task.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(TaskEvents())},
		{"attempt.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(AttemptStates())},
		{"attempt.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(AttemptEvents())},
		{"attention-request.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(AttentionStates())},
		{"attention-request.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(AttentionKinds())},
		{"attention-request.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(AttentionEvents())},
		{"decision.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(DecisionKinds())},
		{"audit-event.schema.json", []string{"properties", "record", "enum"}, schematest.Strings(RecordKinds())},
		{"audit-event.schema.json", []string{"properties", "outcome", "enum"}, schematest.Strings(AuditOutcomes())},
		{"audit-event.schema.json", []string{"properties", "reason", "enum"}, schematest.Strings(RejectionCodes())},
		{"audit-event.schema.json", []string{"$defs", "actor", "properties", "kind", "enum"}, schematest.Strings(ActorKinds())},
		{"binding.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(ResourceKinds())},
		{"provider-operation.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(OperationStates())},
		{"provider-operation.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(OperationEvents())},
		{"provider-operation.schema.json", []string{"$defs", "sendStatus", "enum"}, schematest.Strings(SendStatuses())},
		{"turn-end.schema.json", []string{"properties", "outcome", "enum"}, schematest.Strings(TurnOutcomes())},
		{"turn-end.schema.json", []string{"properties", "failure", "properties", "code", "enum"}, schematest.Strings(TurnFailureCodes())},
		{"environment-observation.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(RunStates())},
		{"harness-qualification.schema.json", []string{"properties", "mode", "enum"}, schematest.Strings(HarnessModes())},
		{"harness-qualification.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(HarnessEvents())},
		{"migration-pass.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(MigrationStates())},
		{"migration-pass.schema.json", []string{"$defs", "event", "enum"}, schematest.Strings(MigrationEvents())},
		{"maintenance-claim.schema.json", []string{"properties", "state", "enum"}, schematest.Strings(ClaimStates())},
	} {
		t.Run(tc.file+"#"+strings.Join(tc.path, "/"), func(t *testing.T) {
			got := schematest.Enum(t, filepath.Join(schemaDir, tc.file), tc.path...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("schema enum = %v, Go constants = %v", got, tc.want)
			}
		})
	}
}
