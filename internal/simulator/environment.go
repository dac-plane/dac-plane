package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Environment effect kinds: the external work an environment applies once per
// operation.
const (
	EnvironmentProvisioned EffectKind = "environment_provisioned"
	EnvironmentStopped     EffectKind = "environment_stopped"
)

// environmentEffect maps each effectful environment method to the effect it
// applies when its scenario applies one.
var environmentEffect = map[contracts.Method]EffectKind{
	contracts.MethodProvision: EnvironmentProvisioned,
	contracts.MethodStop:      EnvironmentStopped,
}

func validEnvironmentEffect(kind EffectKind) bool {
	switch kind {
	case EnvironmentProvisioned, EnvironmentStopped:
		return true
	}
	return false
}

// EnvironmentScenario is the script an Environment follows: one step per
// operation.
type EnvironmentScenario struct {
	Operations map[ids.OperationID]EnvironmentOp
}

// EnvironmentOp is one operation's script. Reply answers provision and stop.
// Effect is applied once, on the first delivery, and must be the effect of
// the delivered method. Observations answer observe deliveries in order, the
// last observation repeating: an observe is not effectful, so a redelivery
// answers with the next observation instead of a recorded reply. SendError
// replaces the reply after the effect, the way a transport loses an
// acknowledgement.
type EnvironmentOp struct {
	Reply        Reply
	Effect       EffectKind
	SendError    error
	Observations []lifecycle.EnvironmentObservation
}

// Environment is a deterministic environment provider behind contracts.Port.
type Environment struct {
	config     Config
	scenario   EnvironmentScenario
	ledger     *Ledger
	deliveries *deliveries
	mu         sync.Mutex
	observed   map[ids.OperationID]int
}

// NewEnvironment validates the config and the scenario and returns the
// simulator. Observations must validate and belong to this environment.
func NewEnvironment(config Config, scenario EnvironmentScenario) (*Environment, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	for operation, step := range scenario.Operations {
		if step.Effect != "" && !validEnvironmentEffect(step.Effect) {
			return nil, fmt.Errorf("%w: %s is not an environment effect", ErrInvalidScenario, step.Effect)
		}
		for _, observation := range step.Observations {
			if err := observation.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidScenario, err)
			}
			if observation.Tenant != config.Tenant || observation.Provider != config.Provider {
				return nil, fmt.Errorf("%w: %s observes another environment", ErrInvalidScenario, operation)
			}
		}
	}
	return &Environment{
		config:     config,
		scenario:   scenario,
		ledger:     newLedger(),
		deliveries: newDeliveries(),
		observed:   map[ids.OperationID]int{},
	}, nil
}

// Role returns contracts.RoleEnvironment.
func (e *Environment) Role() contracts.Role {
	return contracts.RoleEnvironment
}

// Hello returns the lifecycle hello the simulator advertises.
func (e *Environment) Hello() contracts.Lifecycle {
	return e.config.Hello
}

// Control returns the pause control pacing this simulator's boundaries.
func (e *Environment) Control() *Control {
	return e.config.Control
}

// Ledger returns the simulator's record of requests, cancels, subscriptions
// and applied effects.
func (e *Environment) Ledger() *Ledger {
	return e.ledger
}

// Send answers one delivery. Provision and stop follow the runtime rule: the
// first delivery applies the scripted effect and a redelivery of a finished
// operation answers from the record. Observe answers with the next scripted
// observation, so a redelivered observe advances the sequence rather than
// replaying a recorded reply.
func (e *Environment) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := checkRequest(e.config, contracts.RoleEnvironment, request); err != nil {
		return contracts.Reply{}, err
	}
	e.ledger.recordRequest(request)
	step, ok := e.scenario.Operations[request.Operation]
	if !ok {
		return contracts.Reply{}, fmt.Errorf("%w: %s", ErrUnscripted, request.Operation)
	}
	if request.Method == contracts.MethodObserve {
		return e.observe(ctx, request, step)
	}
	if len(step.Observations) > 0 {
		return contracts.Reply{}, fmt.Errorf("%w: %s scripts observations but is not observe", ErrInvalidScenario, request.Method)
	}
	if step.Effect != "" && environmentEffect[request.Method] != step.Effect {
		return contracts.Reply{}, fmt.Errorf("%w: %s applies no %s", ErrInvalidScenario, request.Method, step.Effect)
	}

	recorded, done, release, err := e.deliveries.claim(ctx, request)
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
		if err := e.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
			return contracts.Reply{}, err
		}
		e.ledger.recordEffect(Effect{
			Role: contracts.RoleEnvironment, Operation: request.Operation, Method: request.Method, Kind: step.Effect,
		})
		e.deliveries.finish(request.Operation, request.Method, reply)
		if err := e.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
			return contracts.Reply{}, err
		}
	}
	if step.SendError != nil {
		return contracts.Reply{}, step.SendError
	}
	return reply, nil
}

// observe answers one observe delivery with the next scripted observation,
// marshaled into the reply result. Delivers still serialize, so the sequence
// is deterministic even when observes race.
func (e *Environment) observe(ctx context.Context, request contracts.Request, step EnvironmentOp) (contracts.Reply, error) {
	if len(step.Observations) == 0 || step.Effect != "" {
		return contracts.Reply{}, fmt.Errorf("%w: observe requires observations and no effect", ErrInvalidScenario)
	}
	recorded, done, release, err := e.deliveries.claim(ctx, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if done {
		return recorded, nil
	}
	defer release()

	observation := step.Observations[e.nextObservation(request.Operation, len(step.Observations))]
	result, err := json.Marshal(observation)
	if err != nil {
		return contracts.Reply{}, fmt.Errorf("%w: %v", ErrInvalidScenario, err)
	}
	reply, err := Accepted(result).stamp(request.Operation, request.Delivery)
	if err != nil {
		return contracts.Reply{}, err
	}
	if step.SendError != nil {
		return contracts.Reply{}, step.SendError
	}
	return reply, nil
}

// nextObservation returns the index of the next observation to answer with:
// the sequence advances to the last observation and repeats it.
func (e *Environment) nextObservation(operation ids.OperationID, count int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := e.observed[operation]
	if index < count-1 {
		e.observed[operation] = index + 1
	}
	return index
}

// Cancel records the cancellation intent. Cancellation is confirmed by
// observation, never by this call returning.
func (e *Environment) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return recordCancel(e.ledger, cancel)
}

// Subscribe opens an empty stream: an environment answers in replies, not
// frames, and the cursor still has to name a scripted operation.
func (e *Environment) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, scripted := e.scenario.Operations[cursor.Operation]
	return subscribeInert(e.config, e.ledger, scripted, cursor)
}
