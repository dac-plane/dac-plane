package simulator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// HookDecision is the external effect an enforcement provider applies when it
// answers a hook: the decision it recorded, with the permission in Detail for
// a pre-action hook.
const HookDecision EffectKind = "hook_decision"

// hookKind is the hook kind a delivered method answers. The failure method and
// the failure hook kind are not spelled alike, so the mapping is explicit.
var hookKind = map[contracts.Method]contracts.HookKind{
	contracts.MethodPreAction:    contracts.HookPreAction,
	contracts.MethodPostAction:   contracts.HookPostAction,
	contracts.MethodActionFailed: contracts.HookActionFailure,
}

// EnforcementScenario is the script an Enforcement follows: one response per
// operation.
type EnforcementScenario struct {
	Operations map[ids.OperationID]EnforcementOp
}

// EnforcementOp is one operation's script. Response answers the hook request
// and its kind must be the kind of the delivered hook. SendError replaces the
// reply after the decision, the way a transport loses an acknowledgement.
type EnforcementOp struct {
	Response  contracts.HookResponse
	SendError error
}

// Enforcement is a deterministic enforcement provider behind contracts.Port.
// It answers pre-action, post-action and failure hooks with the scripted
// response; what the response means is decided by the caller with
// contracts.DecidePreAction and identity.Authorize, never here.
type Enforcement struct {
	config     Config
	scenario   EnforcementScenario
	ledger     *Ledger
	deliveries *deliveries
}

// NewEnforcement validates the config and the scenario and returns the
// simulator. Every scripted response must validate on its own; the kind match
// is checked when the hook is delivered.
func NewEnforcement(config Config, scenario EnforcementScenario) (*Enforcement, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	for operation, step := range scenario.Operations {
		if err := step.Response.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %s scripts an invalid response: %v", ErrInvalidScenario, operation, err)
		}
	}
	return &Enforcement{
		config:     config,
		scenario:   scenario,
		ledger:     newLedger(),
		deliveries: newDeliveries(),
	}, nil
}

// Role returns contracts.RoleEnforcement.
func (e *Enforcement) Role() contracts.Role {
	return contracts.RoleEnforcement
}

// Hello returns the lifecycle hello the simulator advertises.
func (e *Enforcement) Hello() contracts.Lifecycle {
	return e.config.Hello
}

// Control returns the pause control pacing this simulator's boundaries.
func (e *Enforcement) Control() *Control {
	return e.config.Control
}

// Ledger returns the simulator's record of requests, cancels, subscriptions
// and applied decisions.
func (e *Enforcement) Ledger() *Ledger {
	return e.ledger
}

// Send answers one delivery with the scripted response, marshaled into the
// reply result. The decision is the operation's effect: the first delivery
// applies it and a redelivery of a finished operation answers from the record.
func (e *Enforcement) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := checkRequest(e.config, contracts.RoleEnforcement, request); err != nil {
		return contracts.Reply{}, err
	}
	e.ledger.recordRequest(request)
	step, ok := e.scenario.Operations[request.Operation]
	if !ok {
		return contracts.Reply{}, fmt.Errorf("%w: %s", ErrUnscripted, request.Operation)
	}
	hook, err := decodeHook(e.config, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if step.Response.Kind != hook.Kind {
		return contracts.Reply{}, fmt.Errorf(
			"%w: %s answers %s with %s", ErrInvalidScenario, request.Operation, hook.Kind, step.Response.Kind)
	}

	recorded, done, release, err := e.deliveries.claim(ctx, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if done {
		return recorded, nil
	}
	defer release()

	response, err := json.Marshal(step.Response)
	if err != nil {
		return contracts.Reply{}, fmt.Errorf("%w: %v", ErrInvalidScenario, err)
	}
	reply, err := Accepted(response).stamp(request.Operation, request.Delivery)
	if err != nil {
		return contracts.Reply{}, err
	}
	if err := e.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
		return contracts.Reply{}, err
	}
	e.ledger.recordEffect(Effect{
		Role: contracts.RoleEnforcement, Operation: request.Operation, Method: request.Method,
		Kind: HookDecision, Detail: string(step.Response.Permission),
	})
	e.deliveries.finish(request.Operation, request.Method, reply)
	if err := e.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
		return contracts.Reply{}, err
	}
	if step.SendError != nil {
		return contracts.Reply{}, step.SendError
	}
	return reply, nil
}

// decodeHook decodes the hook request carried in the delivery's arguments and
// checks that it is this provider's hook for this delivery.
func decodeHook(config Config, request contracts.Request) (contracts.HookRequest, error) {
	var hook contracts.HookRequest
	if err := json.Unmarshal(request.Arguments, &hook); err != nil {
		return contracts.HookRequest{}, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	if err := hook.Validate(); err != nil {
		return contracts.HookRequest{}, fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	if hook.Kind != hookKind[request.Method] ||
		hook.Operation != request.Operation || hook.Tenant != request.Tenant ||
		hook.Attempt != request.Attempt || hook.Provider != config.Provider {
		return contracts.HookRequest{}, fmt.Errorf("%w: hook request does not match its delivery", ErrMalformedRequest)
	}
	return hook, nil
}

// Cancel records the cancellation intent. A hook decision is answered by
// reply, so there is no scripted cancel failure.
func (e *Enforcement) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return recordCancel(e.ledger, cancel)
}

// Subscribe opens an empty stream: an enforcement path answers in replies,
// not frames, and the cursor still has to name a scripted operation.
func (e *Enforcement) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, scripted := e.scenario.Operations[cursor.Operation]
	return subscribeInert(e.config, e.ledger, scripted, cursor)
}

// AllowHook is a pre-action response that allows with no rewrite.
func AllowHook() contracts.HookResponse {
	return contracts.HookResponse{Kind: contracts.HookPreAction, Permission: contracts.PermissionAllow}
}

// DenyHook is a pre-action response that denies.
func DenyHook() contracts.HookResponse {
	return contracts.HookResponse{Kind: contracts.HookPreAction, Permission: contracts.PermissionDeny}
}

// HookFailure is a typed failure for an action_failed hook request.
func HookFailure(code contracts.FailureCode) *contracts.Failure {
	return &contracts.Failure{Code: code, Message: "scripted hook failure"}
}
