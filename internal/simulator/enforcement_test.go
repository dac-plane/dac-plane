package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestEnforcementDecisionsComposeWithDecidePreAction(t *testing.T) {
	config := testConfig(t)
	input := []byte(`{"path":"a.txt"}`)
	rewritten := []byte(`{"path":"b.txt"}`)

	cases := []struct {
		name      string
		response  contracts.HookResponse
		want      contracts.HookOutcome
		rewritten bool
	}{
		{name: "allow", response: simulator.AllowHook(), want: contracts.HookAllowed},
		{name: "deny", response: simulator.DenyHook(), want: contracts.HookDenied},
		{name: "rewrite with usage and messages", response: contracts.HookResponse{
			Kind: contracts.HookPreAction, Permission: contracts.PermissionAllow, UpdatedInput: rewritten,
			UserMessage: "reviewed", AgentMessage: "use the safe path",
			Usage: []contracts.UsageMeter{{Unit: "tokens", Amount: 5}},
		}, want: contracts.HookAllowed, rewritten: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			operation := mustID[ids.Operation](t)
			attempt := mustID[ids.Attempt](t)
			sim := newEnforcement(t, config, map[ids.OperationID]simulator.EnforcementOp{
				operation: {Response: tc.response},
			})
			hook := hookRequest(config, operation, attempt, input)
			delivery := newRoleRequest(t, config, operation, contracts.RoleEnforcement, contracts.MethodPreAction, marshalArguments(t, hook))
			delivery.Attempt = attempt

			reply, err := sim.Send(context.Background(), delivery)
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			var response contracts.HookResponse
			if err := json.Unmarshal(reply.Result, &response); err != nil {
				t.Fatalf("response result %s does not decode: %v", reply.Result, err)
			}
			if !reflect.DeepEqual(response, tc.response) {
				t.Fatalf("response = %+v, want the scripted %+v", response, tc.response)
			}

			decision, err := contracts.DecidePreAction(hook, &response, testNow)
			if err != nil {
				t.Fatalf("DecidePreAction() error = %v", err)
			}
			if decision.Evidence.Outcome != tc.want {
				t.Fatalf("decision = %+v, want %s", decision.Evidence, tc.want)
			}
			if err := decision.Evidence.Validate(); err != nil {
				t.Fatalf("decision evidence does not validate: %v", err)
			}
			if decision.Allowed() {
				if decision.Evidence.Rewritten != tc.rewritten {
					t.Errorf("rewritten = %v, want %v", decision.Evidence.Rewritten, tc.rewritten)
				}
				effective := input
				if tc.rewritten {
					effective = rewritten
				}
				if got := string(decision.EffectiveInput); got != string(effective) {
					t.Errorf("effective input = %s, want %s", got, effective)
				}
				base := identity.ActionRequest{
					Tenant: config.Tenant, Attempt: attempt, Provider: config.Provider,
					Operation: operation, Action: hook.Action, Scope: hook.Scope, InputDigest: hook.InputDigest,
				}
				want := base
				want.InputDigest = identity.DigestOf(effective)
				if got := decision.EffectiveRequest(base); got != want {
					t.Errorf("effective request = %+v, want %+v", got, want)
				}
			}

			if got := sim.Ledger().Effects(operation); got != 1 {
				t.Errorf("effects = %d, want 1", got)
			}
			applied := sim.Ledger().Applied()
			if len(applied) != 1 || applied[0].Role != contracts.RoleEnforcement || applied[0].Kind != simulator.HookDecision {
				t.Fatalf("Applied() = %+v, want one enforcement decision", applied)
			}
			redelivery := delivery
			redelivery.Delivery = mustID[ids.Delivery](t)
			redelivered, err := sim.Send(context.Background(), redelivery)
			if err != nil {
				t.Fatalf("redelivered Send() error = %v", err)
			}
			if redelivered.Delivery != redelivery.Delivery || redelivered.Operation != operation {
				t.Errorf("redelivered reply = %+v, want the recorded decision for the new delivery", redelivered)
			}
			if got := sim.Ledger().Effects(operation); got != 1 {
				t.Errorf("effects after redelivery = %d, want 1", got)
			}
			if got := sim.Ledger().Deliveries(operation); got != 2 {
				t.Errorf("deliveries = %d, want 2", got)
			}
		})
	}
}

func TestEnforcementAnswersEveryHookKind(t *testing.T) {
	config := testConfig(t)
	input := []byte(`{"path":"a.txt"}`)

	postOperation := mustID[ids.Operation](t)
	postAttempt := mustID[ids.Attempt](t)
	postHook := contracts.HookRequest{
		Kind: contracts.HookPostAction, Operation: postOperation, Tenant: config.Tenant,
		Attempt: postAttempt, Provider: config.Provider, Action: "fs.write",
		Scope: identity.Scope{Class: identity.ScopeGlobal}, InputDigest: identity.DigestOf(input),
		OutputDigest: identity.DigestOf([]byte("written")), Duration: time.Second,
	}
	failureOperation := mustID[ids.Operation](t)
	failureAttempt := mustID[ids.Attempt](t)
	failureHook := contracts.HookRequest{
		Kind: contracts.HookActionFailure, Operation: failureOperation, Tenant: config.Tenant,
		Attempt: failureAttempt, Provider: config.Provider, Action: "fs.write",
		Scope: identity.Scope{Class: identity.ScopeGlobal}, InputDigest: identity.DigestOf(input),
		Failure: simulator.HookFailure(contracts.FailureProviderFailed), Duration: time.Second,
	}

	sim, err := simulator.NewEnforcement(config, simulator.EnforcementScenario{
		Operations: map[ids.OperationID]simulator.EnforcementOp{},
		Hooks: map[simulator.HookStageKey]simulator.EnforcementOp{
			{Operation: postOperation, Kind: contracts.HookPostAction}:       {Response: contracts.HookResponse{Kind: contracts.HookPostAction, AgentMessage: "recorded"}},
			{Operation: failureOperation, Kind: contracts.HookActionFailure}: {Response: contracts.HookResponse{Kind: contracts.HookActionFailure}},
		},
	})
	if err != nil {
		t.Fatalf("NewEnforcement() error = %v", err)
	}

	post := newRoleRequest(t, config, postOperation, contracts.RoleEnforcement, contracts.MethodPostAction, marshalArguments(t, postHook))
	post.Attempt = postAttempt
	reply, err := sim.Send(context.Background(), post)
	if err != nil {
		t.Fatalf("post-action Send() error = %v", err)
	}
	var postResponse contracts.HookResponse
	if err := json.Unmarshal(reply.Result, &postResponse); err != nil {
		t.Fatalf("post-action result %s does not decode: %v", reply.Result, err)
	}
	if postResponse.Kind != contracts.HookPostAction || postResponse.AgentMessage != "recorded" {
		t.Errorf("post-action response = %+v, want the scripted one", postResponse)
	}

	failure := newRoleRequest(t, config, failureOperation, contracts.RoleEnforcement, contracts.MethodActionFailed, marshalArguments(t, failureHook))
	failure.Attempt = failureAttempt
	reply, err = sim.Send(context.Background(), failure)
	if err != nil {
		t.Fatalf("action-failed Send() error = %v", err)
	}
	var failureResponse contracts.HookResponse
	if err := json.Unmarshal(reply.Result, &failureResponse); err != nil {
		t.Fatalf("action-failed result %s does not decode: %v", reply.Result, err)
	}
	if failureResponse.Kind != contracts.HookActionFailure || failureResponse.Permission != "" {
		t.Errorf("action-failed response = %+v, want the scripted one", failureResponse)
	}

	applied := sim.Ledger().Applied()
	if len(applied) != 2 {
		t.Fatalf("Applied() = %+v, want one decision per hook", applied)
	}
	for _, effect := range applied {
		if effect.Kind != simulator.HookDecision || effect.Detail != "" {
			t.Errorf("effect = %+v, want a decision with no permission detail", effect)
		}
	}
}

func TestEnforcementRefusesMismatchedDeliveries(t *testing.T) {
	config := testConfig(t)
	input := []byte(`{"path":"a.txt"}`)

	t.Run("scripted response kind", func(t *testing.T) {
		operation := mustID[ids.Operation](t)
		attempt := mustID[ids.Attempt](t)
		sim := newEnforcement(t, config, map[ids.OperationID]simulator.EnforcementOp{
			operation: {Response: contracts.HookResponse{Kind: contracts.HookPostAction}},
		})
		hook := hookRequest(config, operation, attempt, input)
		delivery := newRoleRequest(t, config, operation, contracts.RoleEnforcement, contracts.MethodPreAction, marshalArguments(t, hook))
		delivery.Attempt = attempt
		if _, err := sim.Send(context.Background(), delivery); !errors.Is(err, simulator.ErrInvalidScenario) {
			t.Fatalf("Send() error = %v, want an invalid scenario", err)
		}
		if got := sim.Ledger().Effects(operation); got != 0 {
			t.Errorf("effects = %d, want 0: a mismatched script applies nothing", got)
		}
	})

	t.Run("arguments are not a hook", func(t *testing.T) {
		operation := mustID[ids.Operation](t)
		sim := newEnforcement(t, config, map[ids.OperationID]simulator.EnforcementOp{
			operation: {Response: simulator.AllowHook()},
		})
		delivery := newRoleRequest(t, config, operation, contracts.RoleEnforcement, contracts.MethodPreAction, []byte(`{"unexpected":true}`))
		if _, err := sim.Send(context.Background(), delivery); !errors.Is(err, simulator.ErrMalformedRequest) {
			t.Fatalf("Send() error = %v, want a malformed request", err)
		}
	})

	t.Run("hook of another delivery", func(t *testing.T) {
		operation := mustID[ids.Operation](t)
		other := mustID[ids.Operation](t)
		attempt := mustID[ids.Attempt](t)
		sim := newEnforcement(t, config, map[ids.OperationID]simulator.EnforcementOp{
			operation: {Response: simulator.AllowHook()},
		})
		hook := hookRequest(config, other, attempt, input)
		delivery := newRoleRequest(t, config, operation, contracts.RoleEnforcement, contracts.MethodPreAction, marshalArguments(t, hook))
		delivery.Attempt = attempt
		if _, err := sim.Send(context.Background(), delivery); !errors.Is(err, simulator.ErrMalformedRequest) {
			t.Fatalf("Send() error = %v, want a malformed request", err)
		}
	})
}

// TestEnforcementDenialLeavesNoEffect composes the hook path with the action
// it guards: only an allowed, on-time decision reaches the endpoint, so the
// endpoint's ledger proves a denial or timeout dispatched nothing.
func TestEnforcementDenialLeavesNoEffect(t *testing.T) {
	config := testConfig(t)
	input := []byte(`{"path":"a.txt"}`)

	cases := []struct {
		name     string
		response contracts.HookResponse
		late     bool
		want     contracts.HookOutcome
	}{
		{name: "allow", response: simulator.AllowHook(), want: contracts.HookAllowed},
		{name: "deny", response: simulator.DenyHook(), want: contracts.HookDenied},
		{name: "timeout", response: simulator.AllowHook(), late: true, want: contracts.HookTimedOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hookOperation := mustID[ids.Operation](t)
			actionOperation := mustID[ids.Operation](t)
			attempt := mustID[ids.Attempt](t)
			hooks := newEnforcement(t, config, map[ids.OperationID]simulator.EnforcementOp{
				hookOperation: {Response: tc.response},
			})
			endpoint := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
				actionOperation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
			})

			hook := hookRequest(config, hookOperation, attempt, input)
			delivery := newRoleRequest(t, config, hookOperation, contracts.RoleEnforcement, contracts.MethodPreAction, marshalArguments(t, hook))
			delivery.Attempt = attempt
			reply, err := hooks.Send(context.Background(), delivery)
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			var response contracts.HookResponse
			if err := json.Unmarshal(reply.Result, &response); err != nil {
				t.Fatalf("response result %s does not decode: %v", reply.Result, err)
			}
			respondedAt := testNow
			if tc.late {
				respondedAt = hook.Deadline
			}
			decision, err := contracts.DecidePreAction(hook, &response, respondedAt)
			if err != nil {
				t.Fatalf("DecidePreAction() error = %v", err)
			}
			if decision.Evidence.Outcome != tc.want {
				t.Fatalf("decision = %+v, want %s", decision.Evidence, tc.want)
			}

			if decision.Allowed() {
				action := newRequest(t, config, actionOperation, contracts.MethodStartTurn)
				action.Attempt = attempt
				if _, err := endpoint.Send(context.Background(), action); err != nil {
					t.Fatalf("dispatch Send() error = %v", err)
				}
			}
			want := 0
			if decision.Allowed() {
				want = 1
			}
			if got := endpoint.Ledger().Effects(actionOperation); got != want {
				t.Errorf("endpoint effects = %d, want %d", got, want)
			}
			if got := endpoint.Ledger().Deliveries(actionOperation); got != want {
				t.Errorf("endpoint deliveries = %d, want %d", got, want)
			}
			if got := hooks.Ledger().Effects(hookOperation); got != 1 {
				t.Errorf("hook decision effects = %d, want 1", got)
			}
		})
	}
}
