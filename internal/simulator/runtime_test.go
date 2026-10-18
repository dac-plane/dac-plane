package simulator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// Runtime is the provider port; the assertion is compile-time.
var _ contracts.Port = (*simulator.Runtime)(nil)

func TestRuntimeRoleAndHello(t *testing.T) {
	sim := newRuntime(t, testConfig(t), nil)
	if sim.Role() != contracts.RoleRuntime {
		t.Errorf("Role() = %s, want runtime", sim.Role())
	}
	hello := sim.Hello()
	if hello.Phase != contracts.PhaseHello || len(hello.Versions) != 1 {
		t.Errorf("Hello() = %+v, want a version-1 hello", hello)
	}
	if err := hello.Validate(); err != nil {
		t.Errorf("Hello() Validate() error = %v", err)
	}
}

func TestRuntimeAnswersScriptedReplies(t *testing.T) {
	cases := []struct {
		name       string
		reply      simulator.Reply
		effect     simulator.EffectKind
		outcome    contracts.Outcome
		durability lifecycle.SendStatus
		failed     bool
	}{
		{
			name: "accepted", reply: simulator.Accepted([]byte(`{"turn":1}`)), effect: simulator.TurnStarted,
			outcome: contracts.OutcomeOK, durability: lifecycle.SendAccepted,
		},
		{
			name: "pending", reply: simulator.Pending(nil), effect: simulator.TurnStarted,
			outcome: contracts.OutcomeOK, durability: lifecycle.SendPending,
		},
		{
			name: "rejected", reply: simulator.Rejected(scriptedFailure()),
			outcome: contracts.OutcomeFailed, durability: lifecycle.SendRejected, failed: true,
		},
		{
			name: "unknown durability", reply: simulator.Unknown(scriptedFailure()), effect: simulator.TurnStarted,
			outcome: contracts.OutcomeFailed, durability: lifecycle.SendUnknown, failed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig(t)
			operation := mustID[ids.Operation](t)
			sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
				operation: {Reply: tc.reply, Effect: tc.effect},
			})
			request := newRequest(t, config, operation, contracts.MethodStartTurn)
			reply, err := sim.Send(context.Background(), request)
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			if reply.Outcome != tc.outcome || reply.Durability != tc.durability {
				t.Errorf("reply = %+v, want %s/%s", reply, tc.outcome, tc.durability)
			}
			if (reply.Failure != nil) != tc.failed {
				t.Errorf("failure present = %v, want %v", reply.Failure != nil, tc.failed)
			}
			if err := reply.Validate(); err != nil {
				t.Errorf("reply Validate() error = %v", err)
			}
			wantEffects := 0
			if tc.effect != "" {
				wantEffects = 1
			}
			if got := sim.Ledger().Effects(operation); got != wantEffects {
				t.Errorf("Effects() = %d, want %d", got, wantEffects)
			}
		})
	}
}

func TestRuntimeRefusesMalformedReplyAndEffectMismatch(t *testing.T) {
	config := testConfig(t)
	unnamed := mustID[ids.Operation](t)
	mismatched := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		unnamed:    {Effect: simulator.TurnStarted},
		mismatched: {Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered},
	})

	unnamedRequest := newRequest(t, config, unnamed, contracts.MethodStartTurn)
	if _, err := sim.Send(context.Background(), unnamedRequest); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Errorf("Send() with an outcome-less reply error = %v, want ErrInvalidScenario", err)
	}
	if got := sim.Ledger().Effects(unnamed); got != 0 {
		t.Errorf("Effects() = %d, want no effect from a malformed script", got)
	}

	mismatchedRequest := newRequest(t, config, mismatched, contracts.MethodStartTurn)
	if _, err := sim.Send(context.Background(), mismatchedRequest); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Errorf("Send() with a steer effect on start_turn error = %v, want ErrInvalidScenario", err)
	}
}

func TestRuntimeRefusesUnscriptedAndForeignRequests(t *testing.T) {
	config := testConfig(t)
	sim := newRuntime(t, config, nil)
	operation := mustID[ids.Operation](t)
	request := newRequest(t, config, operation, contracts.MethodStartTurn)

	if _, err := sim.Send(context.Background(), request); !errors.Is(err, simulator.ErrUnscripted) {
		t.Errorf("Send() unscripted error = %v, want ErrUnscripted", err)
	}
	foreignTenant := request
	foreignTenant.Tenant = mustID[ids.Tenant](t)
	if _, err := sim.Send(context.Background(), foreignTenant); !errors.Is(err, simulator.ErrForeignRequest) {
		t.Errorf("Send() foreign tenant error = %v, want ErrForeignRequest", err)
	}
	foreignRole := request
	foreignRole.Role = contracts.RoleEnvironment
	foreignRole.Method = contracts.MethodProvision
	if _, err := sim.Send(context.Background(), foreignRole); !errors.Is(err, simulator.ErrForeignRequest) {
		t.Errorf("Send() foreign role error = %v, want ErrForeignRequest", err)
	}
	malformed := request
	malformed.Delivery = ids.DeliveryID{}
	if _, err := sim.Send(context.Background(), malformed); !errors.Is(err, simulator.ErrMalformedRequest) {
		t.Errorf("Send() malformed request error = %v, want ErrMalformedRequest", err)
	}

	cursor := contracts.Cursor{
		Tenant: config.Tenant, Attempt: request.Attempt, Operation: operation,
		Stream: "runtime", Generation: 1,
	}
	if _, err := sim.Subscribe(context.Background(), cursor); !errors.Is(err, simulator.ErrUnscripted) {
		t.Errorf("Subscribe() unscripted error = %v, want ErrUnscripted", err)
	}
	foreignCursor := cursor
	foreignCursor.Tenant = mustID[ids.Tenant](t)
	if _, err := sim.Subscribe(context.Background(), foreignCursor); !errors.Is(err, simulator.ErrForeignRequest) {
		t.Errorf("Subscribe() foreign cursor error = %v, want ErrForeignRequest", err)
	}
}

func TestRuntimeCancelRecordsIntentAndMayFailTransport(t *testing.T) {
	config := testConfig(t)
	failing := mustID[ids.Operation](t)
	clean := mustID[ids.Operation](t)
	transport := errors.New("cancel transport failed")
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		failing: {Reply: simulator.Accepted(nil), CancelError: transport},
		clean:   {Reply: simulator.Accepted(nil)},
	})

	cancel := contracts.Cancel{Operation: failing, Delivery: mustID[ids.Delivery](t)}
	if err := sim.Cancel(context.Background(), cancel); !errors.Is(err, transport) {
		t.Errorf("Cancel() error = %v, want the scripted transport failure", err)
	}
	if got := sim.Ledger().Cancels(failing); got != 1 {
		t.Errorf("Cancels() = %d, want the failed cancel recorded", got)
	}
	if err := sim.Cancel(context.Background(), contracts.Cancel{Operation: clean, Delivery: mustID[ids.Delivery](t)}); err != nil {
		t.Errorf("Cancel() error = %v, want nil", err)
	}
	if err := sim.Cancel(context.Background(), contracts.Cancel{}); !errors.Is(err, simulator.ErrMalformedRequest) {
		t.Errorf("Cancel() malformed error = %v, want ErrMalformedRequest", err)
	}
	if got := len(sim.Ledger().Cancelled()); got != 2 {
		t.Errorf("Cancelled() = %d cancels, want 2", got)
	}
}

func TestRuntimeNegotiatesProtocolLifecycle(t *testing.T) {
	local := contracts.Lifecycle{
		Phase: contracts.PhaseHello, Versions: []uint32{1, 2},
		Offered: []contracts.Capability{"stream.replay"},
	}
	t.Run("shared version", func(t *testing.T) {
		sim := newRuntime(t, testConfig(t), nil)
		ready, failure := contracts.Negotiate(local, sim.Hello())
		if failure != nil {
			t.Fatalf("Negotiate() failure = %v, want a shared version", failure)
		}
		if ready.Version != 1 {
			t.Errorf("Negotiate() version = %d, want 1, the highest shared", ready.Version)
		}
	})
	t.Run("version mismatch", func(t *testing.T) {
		config := testConfig(t)
		config.Hello = contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{3}}
		sim := newRuntime(t, config, nil)
		_, failure := contracts.Negotiate(local, sim.Hello())
		if failure == nil || failure.Code != contracts.FailureProtocolMismatch {
			t.Fatalf("Negotiate() failure = %v, want protocol_mismatch", failure)
		}
	})
	t.Run("required capability not offered", func(t *testing.T) {
		sim := newRuntime(t, testConfig(t), nil)
		demanding := local
		demanding.Required = []contracts.Capability{"stream.replay"}
		_, failure := contracts.Negotiate(demanding, sim.Hello())
		if failure == nil || failure.Code != contracts.FailureUnsupportedCapability {
			t.Fatalf("Negotiate() failure = %v, want unsupported_capability", failure)
		}
	})
}

func TestRuntimeLostAcknowledgementReconcilesOnRedelivery(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		lost := errors.New("reply lost after the effect")
		config := testConfig(t)
		operation := mustID[ids.Operation](t)
		sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
			operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted, SendError: lost},
		})
		first := newRequest(t, config, operation, contracts.MethodStartTurn)
		if _, err := sim.Send(context.Background(), first); !errors.Is(err, lost) {
			t.Fatalf("Send() error = %v, want the lost acknowledgement", err)
		}
		if got := sim.Ledger().Effects(operation); got != 1 {
			t.Fatalf("Effects() after the lost acknowledgement = %d, want 1", got)
		}
		second := first
		second.Delivery = mustID[ids.Delivery](t)
		reply, err := sim.Send(context.Background(), second)
		if err != nil {
			t.Fatalf("redelivered Send() error = %v", err)
		}
		if reply.Outcome != contracts.OutcomeOK || reply.Delivery != second.Delivery {
			t.Fatalf("redelivered reply = %+v, want the recorded acceptance", reply)
		}
		if got := sim.Ledger().Effects(operation); got != 1 {
			t.Fatalf("Effects() after redelivery = %d, want 1", got)
		}
	})
	t.Run("unknown durability", func(t *testing.T) {
		config := testConfig(t)
		operation := mustID[ids.Operation](t)
		sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
			operation: {Reply: simulator.Unknown(scriptedFailure()), Effect: simulator.TurnStarted},
		})
		first := newRequest(t, config, operation, contracts.MethodStartTurn)
		reply, err := sim.Send(context.Background(), first)
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		if reply.Durability != lifecycle.SendUnknown {
			t.Fatalf("reply durability = %s, want unknown", reply.Durability)
		}
		second := first
		second.Delivery = mustID[ids.Delivery](t)
		again, err := sim.Send(context.Background(), second)
		if err != nil {
			t.Fatalf("redelivered Send() error = %v", err)
		}
		if again.Durability != lifecycle.SendUnknown || again.Failure == nil {
			t.Fatalf("redelivered reply = %+v, want the recorded unknown failure", again)
		}
		if got := sim.Ledger().Effects(operation); got != 1 {
			t.Errorf("Effects() = %d, want 1", got)
		}
	})
}

func TestBoundEventsBindOnlyTheirOwnIdentity(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	attempt := mustID[ids.Attempt](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Stream: &simulator.StreamScript{}},
	})
	record, err := lifecycle.NewProviderOperation(operation, config.Tenant, attempt, config.Provider, testNow)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	peer := contracts.Peer{Tenant: config.Tenant, Provider: config.Provider}
	frame := simulator.HeartbeatFrame(testNow)

	own := sim.Event(attempt, operation, "runtime", 1, frame)
	if _, failure := contracts.BindEvent(peer, own, record); failure != nil {
		t.Fatalf("own event refused: %v", failure)
	}
	forged := own
	forged.Provider = mustID[ids.Provider](t)
	if _, failure := contracts.BindEvent(peer, forged, record); failure == nil || failure.Code != contracts.FailureUnauthenticated {
		t.Errorf("forged provider failure = %v, want unauthenticated", failure)
	}
	wrongOperation := sim.Event(attempt, mustID[ids.Operation](t), "runtime", 1, frame)
	if _, failure := contracts.BindEvent(peer, wrongOperation, record); failure == nil || failure.Code != contracts.FailureUnauthorized {
		t.Errorf("unrecorded operation failure = %v, want unauthorized", failure)
	}
	wrongTenant := own
	wrongTenant.Tenant = mustID[ids.Tenant](t)
	if _, failure := contracts.BindEvent(peer, wrongTenant, record); failure == nil || failure.Code != contracts.FailureUnauthorized {
		t.Errorf("foreign tenant failure = %v, want unauthorized", failure)
	}
}

func TestLedgerRecordsDemandInArrivalOrder(t *testing.T) {
	config := testConfig(t)
	first := mustID[ids.Operation](t)
	second := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		first:  {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
		second: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})
	person := newRequest(t, config, first, contracts.MethodStartTurn)
	background := newRequest(t, config, second, contracts.MethodStartTurn)
	background.Demand = contracts.DemandBackground
	if _, err := sim.Send(context.Background(), person); err != nil {
		t.Fatalf("Send(person) error = %v", err)
	}
	if _, err := sim.Send(context.Background(), background); err != nil {
		t.Fatalf("Send(background) error = %v", err)
	}

	sent := sim.Ledger().Sent()
	if len(sent) != 2 {
		t.Fatalf("Sent() = %d requests, want 2", len(sent))
	}
	if sent[0].Operation != first || sent[0].Demand != contracts.DemandPerson {
		t.Errorf("first request = %+v, want the person operation", sent[0])
	}
	if sent[1].Operation != second || sent[1].Demand != contracts.DemandBackground {
		t.Errorf("second request = %+v, want the background operation", sent[1])
	}
}
