package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// Runtime effect kinds: the external work a runtime applies once per
// operation.
const (
	TurnStarted       EffectKind = "turn_started"
	TurnSteered       EffectKind = "turn_steered"
	TurnInterrupted   EffectKind = "turn_interrupted"
	ArtifactsExported EffectKind = "artifacts_exported"
)

// runtimeEffect maps each runtime method to the effect it applies when its
// scenario applies one.
var runtimeEffect = map[contracts.Method]EffectKind{
	contracts.MethodStartTurn:       TurnStarted,
	contracts.MethodSteerTurn:       TurnSteered,
	contracts.MethodInterrupt:       TurnInterrupted,
	contracts.MethodExportArtifacts: ArtifactsExported,
}

func validRuntimeEffect(kind EffectKind) bool {
	switch kind {
	case TurnStarted, TurnSteered, TurnInterrupted, ArtifactsExported:
		return true
	}
	return false
}

// RuntimeScenario is the script a Runtime follows: one step per operation.
type RuntimeScenario struct {
	Operations map[ids.OperationID]RuntimeOp
	// Decisions script distinct native steers on an existing runtime operation.
	Decisions map[ids.DecisionID]RuntimeOp
}

// RuntimeOp is one operation's script. Reply answers Send. Effect is applied
// once, on the first delivery, and must be the effect of the delivered
// method. SendError replaces the reply after the effect, the way a transport
// loses an acknowledgement. CancelError fails Cancel. Stream is what
// Subscribe serves until the frames end.
type RuntimeOp struct {
	Reply       Reply
	Effect      EffectKind
	SendError   error
	CancelError error
	Stream      *StreamScript
}

// Runtime is a deterministic runtime provider behind contracts.Port.
type Runtime struct {
	artifactExport *artifactExport
	config         Config
	scenario       RuntimeScenario
	ledger         *Ledger
	deliveries     *deliveries
	mu             sync.Mutex
	decisions      map[ids.DecisionID]*deliveries
	fingerprints   map[runtimeWork]identity.Digest
}

type runtimeWork struct {
	operation ids.OperationID
	decision  ids.DecisionID
}

// NewRuntime validates the config and the scenario and returns the simulator.
func NewRuntime(config Config, scenario RuntimeScenario) (*Runtime, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	for _, step := range scenario.Operations {
		if step.Effect != "" && !validRuntimeEffect(step.Effect) {
			return nil, fmt.Errorf("%w: %s is not a runtime effect", ErrInvalidScenario, step.Effect)
		}
	}
	for _, step := range scenario.Decisions {
		if step.Effect != "" && step.Effect != TurnSteered {
			return nil, fmt.Errorf("%w: decision requires a steer effect", ErrInvalidScenario)
		}
	}
	return &Runtime{
		config:       config,
		scenario:     scenario,
		ledger:       newLedger(),
		deliveries:   newDeliveries(),
		decisions:    map[ids.DecisionID]*deliveries{},
		fingerprints: map[runtimeWork]identity.Digest{},
	}, nil
}

// Role returns contracts.RoleRuntime.
func (r *Runtime) Role() contracts.Role {
	return contracts.RoleRuntime
}

// Hello returns the lifecycle hello the simulator advertises.
func (r *Runtime) Hello() contracts.Lifecycle {
	return r.config.Hello
}

// Control returns the pause control pacing this simulator's boundaries.
func (r *Runtime) Control() *Control {
	return r.config.Control
}

// Ledger returns the simulator's record of requests, cancels, subscriptions
// and applied effects.
func (r *Runtime) Ledger() *Ledger {
	return r.ledger
}

// Send answers one delivery. The first delivery of an effectful operation
// applies the scripted effect; a redelivery of a finished operation reuses
// the recorded reply, stamped with the new delivery, and applies nothing. An
// operation with no effect answers every delivery from its script.
//
// Deliveries of one operation are serialized, so a redelivery that races an
// in-flight attempt waits for that attempt only: it then answers from the
// record, or becomes the attempt that applies when the first was abandoned
// before its effect. Deliveries of other operations never block, and no lock
// is held across a pause boundary.
func (r *Runtime) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := checkRequest(r.config, contracts.RoleRuntime, request); err != nil {
		return contracts.Reply{}, err
	}
	if request.Method == contracts.MethodExportArtifacts && r.artifactExport != nil {
		return r.exportArtifact(ctx, request)
	}
	r.ledger.recordRequest(request)
	step, ok := r.scenario.Operations[request.Operation]
	key := runtimeWork{operation: request.Operation}
	if request.Method == contracts.MethodSteerTurn {
		var body struct {
			Decision ids.DecisionID `json:"decision_id"`
		}
		if json.Unmarshal(request.Arguments, &body) != nil || body.Decision.IsZero() {
			return contracts.Reply{}, fmt.Errorf("%w: steer requires its decision identity", ErrMalformedRequest)
		}
		key.decision = body.Decision
		if decision, exists := r.scenario.Decisions[body.Decision]; exists {
			step, ok = decision, true
		}
	}
	if !ok {
		return contracts.Reply{}, fmt.Errorf("%w: %s", ErrUnscripted, request.Operation)
	}
	if step.Effect != "" && runtimeEffect[request.Method] != step.Effect {
		return contracts.Reply{}, fmt.Errorf("%w: %s applies no %s", ErrInvalidScenario, request.Method, step.Effect)
	}

	deliveries, err := r.deliveryScope(key, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	recorded, done, release, err := deliveries.claim(ctx, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if done {
		return recorded, nil
	}
	defer release()

	reply, err := step.Reply.stamp(request.Operation, request.Delivery)
	if err != nil {
		return contracts.Reply{}, err
	}
	if step.Effect != "" {
		if err := r.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
			return contracts.Reply{}, err
		}
		r.ledger.recordEffect(Effect{
			Role: contracts.RoleRuntime, Operation: request.Operation, Method: request.Method, Kind: step.Effect,
		})
		deliveries.finish(request.Operation, request.Method, reply)
		if err := r.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
			return contracts.Reply{}, err
		}
	}
	if step.SendError != nil {
		return contracts.Reply{}, step.SendError
	}
	return reply, nil
}

// deliveryScope keeps decision idempotency separate from the parent turn and
// refuses changed work even when the provider already has a cached reply.
func (r *Runtime) deliveryScope(key runtimeWork, request contracts.Request) (*deliveries, error) {
	if !key.decision.IsZero() {
		// A decision identity cannot be rebound to a different parent operation.
		key.operation = ids.OperationID{}
	}
	raw, err := json.Marshal(struct {
		Operation ids.OperationID
		Tenant    ids.TenantID
		Attempt   ids.AttemptID
		Method    contracts.Method
		Arguments identity.Digest
	}{request.Operation, request.Tenant, request.Attempt, request.Method, identity.DigestOf(request.Arguments)})
	if err != nil {
		return nil, err
	}
	fingerprint := identity.DigestOf(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior, ok := r.fingerprints[key]; ok && prior != fingerprint {
		return nil, fmt.Errorf("%w: operation reused with changed payload", ErrMalformedRequest)
	}
	r.fingerprints[key] = fingerprint
	if key.decision.IsZero() {
		return r.deliveries, nil
	}
	d, ok := r.decisions[key.decision]
	if !ok {
		d = newDeliveries()
		r.decisions[key.decision] = d
	}
	return d, nil
}

// Cancel records the cancellation intent. Cancellation is confirmed by
// observation, never by this call returning; the script may fail the
// transport instead.
func (r *Runtime) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recordCancel(r.ledger, cancel); err != nil {
		return err
	}
	if step, ok := r.scenario.Operations[cancel.Operation]; ok && step.CancelError != nil {
		return step.CancelError
	}
	return nil
}

// Subscribe serves the operation's scripted frames. Frames are not filtered
// by the cursor: the script is the provider's history, and stale or repeated
// delivery is exactly what some scenarios script.
func (r *Runtime) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cursor.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	if cursor.Tenant != r.config.Tenant {
		return nil, fmt.Errorf("%w: %s is not this runtime's stream", ErrForeignRequest, cursor.Operation)
	}
	step, ok := r.scenario.Operations[cursor.Operation]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnscripted, cursor.Operation)
	}
	r.ledger.recordSubscription(cursor)
	var frames []contracts.StreamFrame
	if step.Stream != nil {
		frames = step.Stream.Frames
	}
	return &scriptStream{control: r.config.Control, operation: cursor.Operation, frames: frames}, nil
}

// Event returns the unauthenticated event this provider claims for one of its
// frames. The consumer binds it with contracts.BindEvent, which is where a
// forged claim is refused.
func (r *Runtime) Event(attempt ids.AttemptID, operation ids.OperationID, stream string, generation uint64, frame contracts.StreamFrame) contracts.Event {
	return contracts.Event{
		Tenant:     r.config.Tenant,
		Attempt:    attempt,
		Provider:   r.config.Provider,
		Operation:  operation,
		Stream:     stream,
		Generation: generation,
		Body:       frame,
	}
}
