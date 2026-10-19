package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
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
	Response     contracts.HookResponse
	SendError    error
	Resource     *Resource
	ExecuteError error
}

// Enforcement is a deterministic enforcement provider behind contracts.Port.
// It answers pre-action, post-action and failure hooks with the scripted
// response; what the response means is decided by the caller with
// contracts.DecidePreAction and identity.Authorize, never here.
type Enforcement struct {
	config       Config
	scenario     EnforcementScenario
	ledger       *Ledger
	deliveries   *deliveries
	executions   *deliveries
	mu           sync.Mutex
	inputs       map[ids.OperationID]identity.Digest
	fingerprints map[ids.OperationID]identity.Digest
	hookprints   map[ids.OperationID]identity.Digest
	receipts     map[ids.OperationID]contracts.EnforcementReceipt
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
		executions: newDeliveries(), inputs: make(map[ids.OperationID]identity.Digest), fingerprints: make(map[ids.OperationID]identity.Digest), hookprints: make(map[ids.OperationID]identity.Digest), receipts: make(map[ids.OperationID]contracts.EnforcementReceipt),
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
	if request.Method == contracts.MethodExecuteAction {
		return e.executeAction(ctx, request)
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
	e.mu.Lock()
	digest := identity.DigestOf(request.Arguments)
	previous, seen := e.hookprints[request.Operation]
	if seen && previous != digest {
		e.mu.Unlock()
		return contracts.Reply{}, ErrMalformedRequest
	}
	e.hookprints[request.Operation] = digest
	e.mu.Unlock()

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
	if hook.Kind == contracts.HookPreAction && step.Response.Permission == contracts.PermissionAllow {
		input := hook.Input
		// Authorize the exact bytes delivered in the response, including any
		// JSON encoding performed by the provider's marshaler.
		var sent contracts.HookResponse
		if err := json.Unmarshal(response, &sent); err != nil {
			return contracts.Reply{}, err
		}
		if len(sent.UpdatedInput) > 0 {
			input = sent.UpdatedInput
		}
		e.mu.Lock()
		e.inputs[request.Operation] = identity.DigestOf(input)
		e.mu.Unlock()
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
	e.mu.Lock()
	receipt, ok := e.receipts[cursor.Operation]
	e.mu.Unlock()
	if ok {
		raw, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		if cursor.Validate() != nil || cursor.Tenant != e.config.Tenant {
			return nil, ErrForeignRequest
		}
		e.ledger.recordSubscription(cursor)
		return &scriptStream{control: e.config.Control, operation: cursor.Operation, frames: []contracts.StreamFrame{RowsFrame(false, Row{Sequence: 1, Updated: 1, Kind: "enforcement.receipt", Payload: string(raw)})}}, nil
	}
	return subscribeInert(e.config, e.ledger, scripted, cursor)
}

func (e *Enforcement) executeAction(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	e.ledger.recordRequest(request)
	step, ok := e.scenario.Operations[request.Operation]
	if !ok {
		return contracts.Reply{}, ErrUnscripted
	}
	var execution contracts.ActionExecution
	if json.Unmarshal(request.Arguments, &execution) != nil || execution.Validate() != nil || execution.Request.Operation != request.Operation || execution.Request.Tenant != request.Tenant || execution.Request.Provider != e.config.Provider || execution.Request.Attempt != request.Attempt || execution.Request.Action != "resource.mutate" || execution.Precondition == nil || step.Resource == nil {
		return contracts.Reply{}, ErrMalformedRequest
	}
	e.mu.Lock()
	allowed := e.inputs[request.Operation]
	previous, seen := e.fingerprints[request.Operation]
	digest := identity.DigestOf(request.Arguments)
	if allowed != execution.Request.InputDigest || (seen && previous != digest) {
		e.mu.Unlock()
		return contracts.Reply{}, ErrMalformedRequest
	}
	e.fingerprints[request.Operation] = digest
	e.mu.Unlock()
	recorded, done, release, err := e.executions.claim(ctx, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if done {
		return recorded, nil
	}
	defer release()
	// Check the token at the effect boundary, not during reasoning or hooks.
	ticket, failure := step.Resource.Begin(*execution.Precondition)
	if failure != nil {
		return Rejected(failure).stamp(request.Operation, request.Delivery)
	}
	defer ticket.Abort()
	if err = e.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
		return contracts.Reply{}, err
	}
	if err = ticket.Finish(); err != nil {
		var failure *contracts.Failure
		if errors.As(err, &failure) {
			return Rejected(failure).stamp(request.Operation, request.Delivery)
		}
		return contracts.Reply{}, err
	}
	e.ledger.recordEffect(Effect{Role: contracts.RoleEnforcement, Operation: request.Operation, Method: request.Method, Kind: ResourceAction, Detail: execution.Precondition.Resource})
	receipt := contracts.EnforcementReceipt{Execution: execution, OutputDigest: ticket.OutputDigest(), CompletedAt: time.Now().UTC().Truncate(time.Microsecond)}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return contracts.Reply{}, err
	}
	reply, err := Accepted(raw).stamp(request.Operation, request.Delivery)
	if err != nil {
		return contracts.Reply{}, err
	}
	e.executions.finish(request.Operation, request.Method, reply)
	e.mu.Lock()
	e.receipts[request.Operation] = receipt
	e.mu.Unlock()
	if err = e.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
		return contracts.Reply{}, err
	}
	if step.ExecuteError != nil {
		return contracts.Reply{}, step.ExecuteError
	}
	return reply, nil
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
