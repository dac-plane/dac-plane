package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestEnvironmentProvisionAppliesOneEffect(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		operation: {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned},
	})
	provision := newRoleRequest(t, config, operation, contracts.RoleEnvironment, contracts.MethodProvision, nil)

	reply, err := sim.Send(context.Background(), provision)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		t.Fatalf("reply = %+v, want a durable acceptance", reply)
	}
	redelivery := provision
	redelivery.Delivery = mustID[ids.Delivery](t)
	redelivered, err := sim.Send(context.Background(), redelivery)
	if err != nil {
		t.Fatalf("redelivered Send() error = %v", err)
	}
	if redelivered.Delivery != redelivery.Delivery || redelivered.Operation != operation {
		t.Fatalf("redelivered reply = %+v, want the recorded acceptance for the new delivery", redelivered)
	}
	if got := sim.Ledger().Effects(operation); got != 1 {
		t.Errorf("effects = %d, want 1", got)
	}
	if got := sim.Ledger().Deliveries(operation); got != 2 {
		t.Errorf("deliveries = %d, want 2", got)
	}
	applied := sim.Ledger().Applied()
	if len(applied) != 1 || applied[0].Role != contracts.RoleEnvironment || applied[0].Kind != simulator.EnvironmentProvisioned {
		t.Fatalf("Applied() = %+v, want one environment provision", applied)
	}

	t.Run("lost acknowledgement", func(t *testing.T) {
		config := testConfig(t)
		operation := mustID[ids.Operation](t)
		sim := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
			operation: {
				Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned, SendError: scriptedLoss,
			},
		})
		provision := newRoleRequest(t, config, operation, contracts.RoleEnvironment, contracts.MethodProvision, nil)
		if _, err := sim.Send(context.Background(), provision); !errors.Is(err, scriptedLoss) {
			t.Fatalf("Send() error = %v, want the scripted transport loss", err)
		}
		if got := sim.Ledger().Effects(operation); got != 1 {
			t.Fatalf("effects after the lost acknowledgement = %d, want 1", got)
		}
		redelivery := provision
		redelivery.Delivery = mustID[ids.Delivery](t)
		reply, err := sim.Send(context.Background(), redelivery)
		if err != nil {
			t.Fatalf("redelivered Send() error = %v", err)
		}
		if reply.Outcome != contracts.OutcomeOK || reply.Delivery != redelivery.Delivery {
			t.Fatalf("redelivered reply = %+v, want the recorded acceptance", reply)
		}
		if got := sim.Ledger().Effects(operation); got != 1 {
			t.Errorf("effects after redelivery = %d, want 1", got)
		}
		if got := sim.Ledger().Deliveries(operation); got != 2 {
			t.Errorf("deliveries = %d, want 2", got)
		}
	})
}

func TestEnvironmentDefaultAppliesEachMethodsEffect(t *testing.T) {
	config := testConfig(t)
	sim, err := simulator.NewEnvironment(config, simulator.EnvironmentScenario{
		Default: &simulator.EnvironmentOp{Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentProvisioned},
	})
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	provision, stop := mustID[ids.Operation](t), mustID[ids.Operation](t)
	for _, step := range []struct {
		operation ids.OperationID
		method    contracts.Method
		want      simulator.EffectKind
	}{
		{provision, contracts.MethodProvision, simulator.EnvironmentProvisioned},
		{stop, contracts.MethodStop, simulator.EnvironmentStopped},
	} {
		request := newRoleRequest(t, config, step.operation, contracts.RoleEnvironment, step.method, nil)
		for range 2 {
			request.Delivery = mustID[ids.Delivery](t)
			if reply, err := sim.Send(context.Background(), request); err != nil || reply.Outcome != contracts.OutcomeOK {
				t.Fatalf("%s Send() = %+v, %v; want an acceptance", step.method, reply, err)
			}
		}
		if got := sim.Ledger().Effects(step.operation); got != 1 {
			t.Errorf("%s effects = %d, want 1", step.method, got)
		}
	}
	applied := sim.Ledger().Applied()
	if len(applied) != 2 || applied[0].Kind != simulator.EnvironmentProvisioned || applied[1].Kind != simulator.EnvironmentStopped {
		t.Fatalf("Applied() = %+v, want a provision then a stop", applied)
	}
	// The control plane follows a defaulted provision for its observation.
	stream, err := sim.Subscribe(context.Background(), contracts.Cursor{
		Tenant: config.Tenant, Attempt: mustID[ids.Attempt](t), Operation: provision, Stream: "events", Generation: 1,
	})
	if err != nil {
		t.Fatalf("Subscribe() of a defaulted operation error = %v", err)
	}
	if frame, err := stream.Next(context.Background()); err != nil || frame.Rows == nil || len(frame.Rows.Entries) != 1 || frame.Rows.Entries[0].Kind != string(contracts.EntryEnvironmentObserved) {
		t.Fatalf("defaulted provision stream = %+v, %v; want its observation", frame, err)
	}

	if _, err := simulator.NewEnvironment(testConfig(t), simulator.EnvironmentScenario{
		Default: &simulator.EnvironmentOp{Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted},
	}); !errors.Is(err, simulator.ErrInvalidScenario) {
		t.Fatalf("NewEnvironment() with a runtime default effect error = %v, want ErrInvalidScenario", err)
	}
}

func TestEnvironmentObservationsAdvanceAndConfirmStop(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	attempt := mustID[ids.Attempt](t)
	sim := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		operation: {Observations: []lifecycle.EnvironmentObservation{
			observation(config, attempt, 1, lifecycle.RunStarting),
			observation(config, attempt, 2, lifecycle.RunRunning),
			observation(config, attempt, 4, lifecycle.RunAbsent),
		}},
	})
	observe := newRoleRequest(t, config, operation, contracts.RoleEnvironment, contracts.MethodObserve, nil)
	observe.Attempt = attempt

	// The fourth delivery repeats the last observation.
	wants := []uint64{1, 2, 4, 4}
	seen := make([]lifecycle.EnvironmentObservation, 0, len(wants))
	for _, want := range wants {
		observe.Delivery = mustID[ids.Delivery](t)
		reply, err := sim.Send(context.Background(), observe)
		if err != nil {
			t.Fatalf("observe Send() error = %v", err)
		}
		if reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
			t.Fatalf("observe reply = %+v, want a durable acceptance", reply)
		}
		var got lifecycle.EnvironmentObservation
		if err := json.Unmarshal(reply.Result, &got); err != nil {
			t.Fatalf("observation result %s does not decode: %v", reply.Result, err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("observation = %+v does not validate: %v", got, err)
		}
		if got.Sequence != want {
			t.Fatalf("observation sequence = %d, want %d", got.Sequence, want)
		}
		seen = append(seen, got)
	}

	for i := 1; i < len(seen)-1; i++ {
		if !seen[i].Supersedes(seen[i-1]) {
			t.Errorf("observation %d does not supersede %d: %+v over %+v", i, i-1, seen[i], seen[i-1])
		}
		if seen[i-1].Supersedes(seen[i]) {
			t.Errorf("observation %d supersedes the newer %d", i-1, i)
		}
	}
	last := seen[len(seen)-1]
	if last.State != seen[len(seen)-2].State || last.Sequence != seen[len(seen)-2].Sequence ||
		!last.ObservedAt.Equal(seen[len(seen)-2].ObservedAt) {
		t.Errorf("last observation = %+v, want %+v repeated", last, seen[len(seen)-2])
	}
	if last.Supersedes(seen[len(seen)-2]) {
		t.Errorf("a repeated observation supersedes itself")
	}
	if seen[1].Stopped() {
		t.Errorf("running observation reports stopped: %+v", seen[1])
	}
	if !seen[2].Stopped() || !seen[3].Stopped() {
		t.Errorf("absent observations do not report stopped: %+v", seen[2:])
	}
	if got := sim.Ledger().Effects(operation); got != 0 {
		t.Errorf("observe effects = %d, want 0", got)
	}
	if got := sim.Ledger().Deliveries(operation); got != len(wants) {
		t.Errorf("observe deliveries = %d, want %d", got, len(wants))
	}
}

func TestEnvironmentSubscribeIsInert(t *testing.T) {
	config := testConfig(t)
	operation := mustID[ids.Operation](t)
	sim := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		operation: {Observations: []lifecycle.EnvironmentObservation{
			observation(config, mustID[ids.Attempt](t), 1, lifecycle.RunRunning),
		}},
	})
	cursor := contracts.Cursor{
		Tenant: config.Tenant, Attempt: mustID[ids.Attempt](t), Operation: operation,
		Stream: "environment", Generation: 1,
	}
	stream, err := sim.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want io.EOF", err)
	}
	subscriptions := sim.Ledger().Subscriptions()
	if len(subscriptions) != 1 || subscriptions[0] != cursor {
		t.Fatalf("Subscriptions() = %+v, want the opening cursor", subscriptions)
	}

	unscripted := cursor
	unscripted.Operation = mustID[ids.Operation](t)
	if _, err := sim.Subscribe(context.Background(), unscripted); !errors.Is(err, simulator.ErrUnscripted) {
		t.Fatalf("Subscribe() of an unscripted operation error = %v, want unscripted", err)
	}
}

func TestEnvironmentTerminationIsSeparate(t *testing.T) {
	config := testConfig(t)
	attempt := mustID[ids.Attempt](t)
	interruptOperation := mustID[ids.Operation](t)
	stopOperation := mustID[ids.Operation](t)
	observeOperation := mustID[ids.Operation](t)

	runtime := newRuntime(t, config, map[ids.OperationID]simulator.RuntimeOp{
		interruptOperation: {Reply: simulator.Accepted(nil), Effect: simulator.TurnInterrupted},
	})
	environment := newEnvironment(t, config, map[ids.OperationID]simulator.EnvironmentOp{
		stopOperation: {Reply: simulator.Accepted(nil), Effect: simulator.EnvironmentStopped},
		observeOperation: {Observations: []lifecycle.EnvironmentObservation{
			observation(config, attempt, 1, lifecycle.RunRunning),
			observation(config, attempt, 2, lifecycle.RunAbsent),
		}},
	})

	interrupt := newRequest(t, config, interruptOperation, contracts.MethodInterrupt)
	interrupt.Attempt = attempt
	if _, err := runtime.Send(context.Background(), interrupt); err != nil {
		t.Fatalf("interrupt Send() error = %v", err)
	}
	observe := newRoleRequest(t, config, observeOperation, contracts.RoleEnvironment, contracts.MethodObserve, nil)
	observe.Attempt = attempt

	// The interrupted runtime has not stopped the environment: it keeps
	// running until the environment itself is stopped and observed absent.
	running, err := environment.Send(context.Background(), observe)
	if err != nil {
		t.Fatalf("observe Send() error = %v", err)
	}
	var before lifecycle.EnvironmentObservation
	if err := json.Unmarshal(running.Result, &before); err != nil {
		t.Fatalf("observation result %s does not decode: %v", running.Result, err)
	}
	if before.Stopped() {
		t.Fatalf("observation after the runtime interrupt = %+v, want a running resource", before)
	}

	stop := newRoleRequest(t, config, stopOperation, contracts.RoleEnvironment, contracts.MethodStop, nil)
	stop.Attempt = attempt
	if _, err := environment.Send(context.Background(), stop); err != nil {
		t.Fatalf("stop Send() error = %v", err)
	}
	observe.Delivery = mustID[ids.Delivery](t)
	stopped, err := environment.Send(context.Background(), observe)
	if err != nil {
		t.Fatalf("observe Send() error = %v", err)
	}
	var after lifecycle.EnvironmentObservation
	if err := json.Unmarshal(stopped.Result, &after); err != nil {
		t.Fatalf("observation result %s does not decode: %v", stopped.Result, err)
	}
	if !after.Stopped() {
		t.Fatalf("observation after the environment stop = %+v, want a stopped resource", after)
	}

	runtimeEffects := runtime.Ledger().Applied()
	environmentEffects := environment.Ledger().Applied()
	if len(runtimeEffects) != 1 || runtimeEffects[0].Kind != simulator.TurnInterrupted {
		t.Fatalf("runtime Applied() = %+v, want one turn interruption", runtimeEffects)
	}
	if len(environmentEffects) != 1 || environmentEffects[0].Kind != simulator.EnvironmentStopped {
		t.Fatalf("environment Applied() = %+v, want one environment stop", environmentEffects)
	}
	if runtimeEffects[0].Kind == environmentEffects[0].Kind {
		t.Errorf("runtime and environment termination share the effect kind %q", runtimeEffects[0].Kind)
	}
	if got := environment.Ledger().Effects(interruptOperation); got != 0 {
		t.Errorf("environment applied %d runtime effects", got)
	}
}
