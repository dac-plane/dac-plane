package simulator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestNewRuntimeRefusesBadConfigAndScenario(t *testing.T) {
	config := testConfig(t)

	if _, err := simulator.NewRuntime(simulator.Config{}, simulator.RuntimeScenario{}); !errors.Is(err, simulator.ErrInvalidConfig) {
		t.Errorf("NewRuntime(zero config) error = %v, want ErrInvalidConfig", err)
	}

	broken := config
	broken.Hello = contracts.Lifecycle{Phase: contracts.PhaseReady, Version: 1}
	if _, err := simulator.NewRuntime(broken, simulator.RuntimeScenario{}); !errors.Is(err, simulator.ErrInvalidConfig) {
		t.Errorf("NewRuntime(ready hello) error = %v, want ErrInvalidConfig", err)
	}

	operation := mustID[ids.Operation](t)
	badEffect := map[ids.OperationID]simulator.RuntimeOp{
		operation: {Effect: "not_a_runtime_effect"},
	}
	if _, err := simulator.NewRuntime(config, simulator.RuntimeScenario{Operations: badEffect}); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Errorf("NewRuntime(foreign effect) error = %v, want ErrInvalidScenario", err)
	}
}

func TestControlPausesBeforeAndAfterEffect(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})
	request := newRequest(t, config, operation, contracts.MethodStartTurn)

	before := sim.Control().Pause(operation, simulator.BeforeEffect)
	after := sim.Control().Pause(operation, simulator.AfterEffect)
	var reply contracts.Reply
	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		reply, sendErr = sim.Send(context.Background(), request)
	}()

	<-before.Reached()
	if got := sim.Ledger().Effects(operation); got != 0 {
		t.Fatalf("effects while paused before the effect = %d, want 0", got)
	}
	before.Release()
	<-after.Reached()
	select {
	case <-done:
		t.Fatal("Send returned while paused at the effect boundary")
	default:
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Fatalf("effects while paused after the effect = %d, want 1", got)
	}
	after.Release()
	<-done
	if sendErr != nil {
		t.Fatalf("Send() error = %v", sendErr)
	}
	if reply.Operation != operation || reply.Delivery != request.Delivery {
		t.Fatalf("Send() reply answers %s/%s, want %s/%s", reply.Operation, reply.Delivery, operation, request.Delivery)
	}
	if reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		t.Fatalf("Send() reply = %+v, want accepted", reply)
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Fatalf("effects after completion = %d, want 1", got)
	}
}

func TestControlCancelledAfterEffectLeavesOneEffect(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})
	request := newRequest(t, config, operation, contracts.MethodStartTurn)

	pause := sim.Control().Pause(operation, simulator.AfterEffect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abandoned := make(chan error, 1)
	go func() {
		_, err := sim.Send(ctx, request)
		abandoned <- err
	}()
	<-pause.Reached()
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error after abandon = %v, want context.Canceled", err)
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Fatalf("effects after the abandoned call = %d, want 1", got)
	}

	redelivery := request
	redelivery.Delivery = mustID[ids.Delivery](t)
	reply, err := sim.Send(context.Background(), redelivery)
	if err != nil {
		t.Fatalf("redelivered Send() error = %v", err)
	}
	if reply.Outcome != contracts.OutcomeOK || reply.Delivery != redelivery.Delivery {
		t.Fatalf("redelivered reply = %+v, want an accepted answer to the new delivery", reply)
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Fatalf("effects after redelivery = %d, want 1", got)
	}
	if got := sim.Ledger().Deliveries(operation); got != 2 {
		t.Fatalf("deliveries after redelivery = %d, want 2", got)
	}
}

func TestControlCanPauseTheNextOperation(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	})
	pause := sim.Control().Pause(ids.OperationID{}, simulator.BeforeEffect)
	request := newRequest(t, config, operation, contracts.MethodStartTurn)
	done := make(chan error, 1)
	go func() {
		_, err := sim.Send(context.Background(), request)
		done <- err
	}()
	select {
	case <-pause.Reached():
	case <-time.After(time.Second):
		t.Fatal("the next-operation pause was not reached")
	}
	if got := sim.Ledger().Effects(operation); got != 0 {
		t.Fatalf("effects before release = %d, want 0", got)
	}
	pause.Release()
	if err := <-done; err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

func TestLedgerCountsDeliveriesApartFromEffects(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Accepted([]byte(`{"turn":1}`)), Effect: simulator.TurnStarted},
	})
	first := newRequest(t, config, operation, contracts.MethodStartTurn)
	if _, err := sim.Send(context.Background(), first); err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	second := first
	second.Delivery = mustID[ids.Delivery](t)
	reply, err := sim.Send(context.Background(), second)
	if err != nil {
		t.Fatalf("second Send() error = %v", err)
	}
	if string(reply.Result) != `{"turn":1}` {
		t.Fatalf("redelivered result = %s, want the recorded result", reply.Result)
	}

	if got := sim.Ledger().Deliveries(operation); got != 2 {
		t.Errorf("Deliveries() = %d, want 2", got)
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Errorf("Effects() = %d, want 1", got)
	}
	sent := sim.Ledger().Sent()
	if len(sent) != 2 || sent[0].Delivery == sent[1].Delivery {
		t.Errorf("Sent() deliveries = %v, want two distinct deliveries", sent)
	}
	applied := sim.Ledger().Applied()
	if len(applied) != 1 || applied[0].Kind != simulator.TurnStarted || applied[0].Operation != operation {
		t.Errorf("Applied() = %+v, want one turn_started effect", applied)
	}
}

func TestNoEffectOperationAnswersEveryDelivery(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		operation: {Reply: simulator.Rejected(scriptedFailure())},
	})
	first := newRequest(t, config, operation, contracts.MethodStartTurn)
	if _, err := sim.Send(context.Background(), first); err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	second := first
	second.Delivery = mustID[ids.Delivery](t)
	reply, err := sim.Send(context.Background(), second)
	if err != nil {
		t.Fatalf("second Send() error = %v", err)
	}
	if reply.Outcome != contracts.OutcomeFailed || reply.Durability != lifecycle.SendRejected {
		t.Fatalf("second reply = %+v, want the scripted rejection again", reply)
	}
	if got := sim.Ledger().Effects(operation); got != 0 {
		t.Errorf("Effects() = %d, want 0", got)
	}
	if got := sim.Ledger().Deliveries(operation); got != 2 {
		t.Errorf("Deliveries() = %d, want 2", got)
	}
}
