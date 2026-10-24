package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// WithEnforcementPorts binds a second qualified role of a provider identity.
// The provider audience remains identical in approval, hook, use and receipt.
// A runtime hook and its executor can therefore share an audience without
// replacing the runtime's steer/cancel port.
func WithEnforcementPorts(ports map[ids.ProviderID]contracts.Port) func(*Worker) {
	return func(w *Worker) {
		w.enforcement = make(map[ids.ProviderID]contracts.Port, len(ports))
		for id, p := range ports {
			w.enforcement[id] = p
		}
	}
}

// WithEnvironmentPorts registers environment-role ports by harness identity.
// Runtime and environment adapters remain distinct even when they share a
// provider identity.
func WithEnvironmentPorts(ports map[ids.ProviderID]contracts.Port) func(*Worker) {
	return func(w *Worker) {
		w.environments = make(map[ids.ProviderID]contracts.Port, len(ports))
		for id, p := range ports {
			w.environments[id] = p
		}
	}
}

func (w *Worker) governedRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
	a, err := store.GetGovernedAction(ctx, w.pool, tenant, body.Operation)
	if err != nil {
		return err
	}
	if a.Use.Outcome != identity.UseAllowed || a.Execution.Validate() != nil || body.Operation.String() != row.RecordID.String() || body.Role != contracts.RoleEnforcement || body.Method != contracts.MethodExecuteAction {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	port := w.port(tenant, a.Use.Provider, contracts.RoleEnforcement)
	if port == nil {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}
	prepared, err := w.prepareDispatch(ctx, tenant, row, body, now)
	if err != nil || prepared == nil {
		return err
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(a.Execution)
	if err != nil {
		return err
	}
	request := contracts.Request{Operation: body.Operation, Delivery: delivery, Tenant: tenant, Attempt: a.Use.Attempt, Role: contracts.RoleEnforcement, Method: contracts.MethodExecuteAction, Arguments: raw, Demand: row.Demand, Deadline: body.Deadline}
	if err = request.Validate(); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, min(w.cfg.SendTimeout, time.Until(body.Deadline)))
	defer cancel()
	actionStarted := time.Now()
	reply, sendErr := port.Send(sctx, request)
	var receipt *contracts.EnforcementReceipt
	outcome := lifecycle.SendUnknown
	code := contracts.FailureTransportUnavailable
	if sendErr == nil && reply.Validate() == nil && reply.Operation == body.Operation && reply.Delivery == delivery {
		outcome = reply.Durability
		if reply.Failure != nil {
			code = reply.Failure.Code
		}
		if outcome == lifecycle.SendAccepted {
			var r contracts.EnforcementReceipt
			if json.Unmarshal(reply.Result, &r) == nil && r.Validate() == nil {
				expected, _ := json.Marshal(a.Execution)
				actual, _ := json.Marshal(r.Execution)
				if string(expected) == string(actual) {
					receipt = &r
				} else {
					outcome = lifecycle.SendUnknown
				}
			} else {
				outcome = lifecycle.SendUnknown
			}
		}
	}
	// Pending may already have caused an effect. It is reconciled, never sent
	// again through the generic retry path.
	if outcome == lifecycle.SendPending {
		outcome = lifecycle.SendUnknown
	}
	return w.resolveGoverned(ctx, tenant, row, prepared, outcome, receipt, code, time.Since(actionStarted), time.Now().UTC())
}

func (w *Worker) resolveGoverned(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, p *preparedDispatch, outcome lifecycle.SendStatus, receipt *contracts.EnforcementReceipt, code contracts.FailureCode, duration time.Duration, now time.Time) error {
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(ctx) }()
	// Fence before all writes. Failure rolls back receipt, operation and ack.
	if err = store.AckOutboxRow(ctx, u, tenant, row.ID, p.claim.Epoch, now); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	tr, err := w.transition(p.operation.Version, now)
	if err != nil {
		return err
	}
	if outcome == lifecycle.SendRejected {
		action, err := store.GetGovernedAction(ctx, u, tenant, p.operation.ID)
		if err != nil {
			return err
		}
		failure := &contracts.Failure{Code: code, Message: "governed action failed"}
		tr.Outbox, err = hookOutbox(tenant, p.operation.Attempt, p.operation.ID, contracts.HookRequest{Kind: contracts.HookActionFailure, Operation: p.operation.ID, Tenant: tenant, Attempt: p.operation.Attempt, Provider: p.operation.Provider, Action: action.Execution.Request.Action, Scope: action.Execution.Request.Scope, InputDigest: action.Execution.Request.InputDigest, Failure: failure, Duration: positiveDuration(duration)}, now)
		if err != nil {
			return err
		}
	}
	op, _, err := store.ApplyOperation(ctx, u, tenant, p.operation.ID, outcome.Event(), "", tr)
	if err != nil {
		return err
	}
	if receipt != nil {
		if err = store.InsertEnforcementReceipt(ctx, u, *receipt); err != nil {
			return err
		}
		tr, err = w.transition(op.Version, now)
		if err != nil {
			return err
		}
		followUp, err := hookOutbox(tenant, p.operation.Attempt, p.operation.ID, contracts.HookRequest{Kind: contracts.HookPostAction, Operation: p.operation.ID, Tenant: tenant, Attempt: p.operation.Attempt, Provider: p.operation.Provider, Action: receipt.Execution.Request.Action, Scope: receipt.Execution.Request.Scope, InputDigest: receipt.Execution.Request.InputDigest, OutputDigest: receipt.OutputDigest, Duration: positiveDuration(duration)}, now)
		if err != nil {
			return err
		}
		tr.Outbox = followUp
		if _, _, err = store.ApplyOperation(ctx, u, tenant, op.ID, lifecycle.OperationSucceed, "", tr); err != nil {
			return err
		}
	}
	if outcome != lifecycle.SendAccepted {
		if _, err = u.Exec(ctx, `update outbox set last_error=$3 where tenant_id=$1 and outbox_id=$2`, tenant.String(), row.ID.String(), string(code)); err != nil {
			return err
		}
	}
	if err = w.releaseLease(ctx, u, tenant, p, now); err != nil {
		return err
	}
	return u.Commit(ctx)
}

func positiveDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}

func hookOutbox(tenant ids.TenantID, attempt ids.AttemptID, operation ids.OperationID, hook contracts.HookRequest, at time.Time) (*store.OutboxPayload, error) {
	if err := hook.Validate(); err != nil {
		return nil, err
	}
	method := contracts.MethodPostAction
	if hook.Kind == contracts.HookActionFailure {
		method = contracts.MethodActionFailed
	}
	arguments, err := json.Marshal(hook)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Operation ids.OperationID  `json:"operation"`
		Role      contracts.Role   `json:"role"`
		Method    contracts.Method `json:"method"`
		Arguments json.RawMessage  `json:"arguments"`
		Deadline  time.Time        `json:"deadline"`
	}{operation, contracts.RoleEnforcement, method, arguments, at.Add(24 * time.Hour)})
	if err != nil {
		return nil, err
	}
	id, err := ids.New[ids.Any]()
	if err != nil {
		return nil, err
	}
	return &store.OutboxPayload{ID: id, Attempt: attempt, Demand: contracts.DemandBackground, Body: body}, nil
}

func hookResponseEvidence(operation ids.OperationID, request contracts.HookRequest, response contracts.HookResponse) contracts.HookStageEvidence {
	raw, _ := json.Marshal(response)
	e := contracts.HookStageEvidence{Operation: operation, Kind: request.Kind, InputDigest: request.InputDigest, OutputDigest: request.OutputDigest, Duration: request.Duration, ResponseDigest: identity.DigestOf(raw), Usage: response.Usage}
	if request.Failure != nil {
		e.FailureCode = request.Failure.Code
	}
	if response.UserMessage != "" {
		e.UserMessageDigest = identity.DigestOf([]byte(response.UserMessage))
	}
	if response.AgentMessage != "" {
		e.AgentMessageDigest = identity.DigestOf([]byte(response.AgentMessage))
	}
	if len(response.AdditionalContext) > 0 {
		contextBytes, _ := json.Marshal(response.AdditionalContext)
		e.ContextDigest = identity.DigestOf(contextBytes)
	}
	return e
}

func (w *Worker) hookStageRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
	var hook contracts.HookRequest
	decoder := json.NewDecoder(bytes.NewReader(body.Arguments))
	decoder.DisallowUnknownFields()
	postEvent := row.Event == string(lifecycle.OperationSucceed) || row.Event == string(lifecycle.OperationReconcile)
	if row.Record != lifecycle.RecordOperation || body.Operation.String() != row.RecordID.String() || !decodeSingle(decoder, &hook) || hook.Validate() != nil || hook.Operation != body.Operation || hook.Tenant != tenant || body.Method != hookMethod(hook.Kind) || (hook.Kind == contracts.HookPostAction && !postEvent) || (hook.Kind == contracts.HookActionFailure && row.Event != string(lifecycle.OperationSendRejected)) {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	action, err := store.GetGovernedAction(ctx, w.pool, tenant, body.Operation)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return w.poison(ctx, tenant, row, now, reasonUnknownOperation)
		}
		return err
	}
	port := w.port(tenant, action.Use.Provider, contracts.RoleEnforcement)
	if port == nil {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}

	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	claim, err := store.ClaimOutboxRowByID(ctx, u, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		_ = u.Rollback(ctx)
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	_, _, err = store.LockResultScope(ctx, u, tenant, action.Use.Attempt)
	if err != nil {
		_ = u.Rollback(ctx)
		return err
	}
	current, err := store.GetOperation(ctx, u, tenant, body.Operation)
	if err != nil {
		_ = u.Rollback(ctx)
		return err
	}
	if (hook.Kind == contracts.HookPostAction && current.State != lifecycle.OperationSucceeded) || (hook.Kind == contracts.HookActionFailure && current.State != lifecycle.OperationRejected) {
		_ = u.Rollback(ctx)
		return w.poison(ctx, tenant, row, now, reasonUnknownState)
	}
	action, err = store.GetGovernedAction(ctx, u, tenant, body.Operation)
	if err != nil {
		_ = u.Rollback(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return w.poison(ctx, tenant, row, now, reasonUnknownOperation)
		}
		return err
	}
	if !hookMatchesAction(hook, action) {
		_ = u.Rollback(ctx)
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	harness, err := store.GetHarnessForUpdate(ctx, u, tenant, action.Use.Provider)
	if err != nil {
		_ = u.Rollback(ctx)
		return err
	}
	if harness.Mode != lifecycle.HarnessLive {
		_ = u.Rollback(ctx)
		return nil
	}
	if err := store.CheckOutboxClaim(ctx, u, tenant, row.ID, claim.Epoch); err != nil {
		_ = u.Rollback(ctx)
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	if err := u.Commit(ctx); err != nil {
		return err
	}

	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	arguments, err := json.Marshal(hook)
	if err != nil {
		return err
	}
	request := contracts.Request{Operation: body.Operation, Delivery: delivery, Tenant: tenant, Attempt: action.Use.Attempt, Role: contracts.RoleEnforcement, Method: body.Method, Arguments: arguments, Demand: row.Demand, Deadline: body.Deadline}
	if err := request.Validate(); err != nil {
		return w.retryClaim(ctx, tenant, row, claim, now, contracts.FailureInvalidArguments)
	}
	sendCtx, cancel := context.WithTimeout(ctx, min(w.cfg.SendTimeout, time.Until(body.Deadline)))
	reply, sendErr := port.Send(sendCtx, request)
	cancel()
	if sendErr != nil {
		return w.retryHookStage(ctx, tenant, row, claim, now, contracts.FailureTransportUnavailable)
	}
	var response contracts.HookResponse
	responseDecoder := json.NewDecoder(bytes.NewReader(reply.Result))
	responseDecoder.DisallowUnknownFields()
	if reply.Validate() != nil || reply.Operation != body.Operation || reply.Delivery != delivery || reply.Durability != lifecycle.SendAccepted || !decodeSingle(responseDecoder, &response) || response.Validate() != nil || response.Kind != hook.Kind {
		return w.retryHookStage(ctx, tenant, row, claim, now, contracts.FailureMalformedFrame)
	}
	evidence := hookResponseEvidence(body.Operation, hook, response)
	if evidence.Validate() != nil {
		return w.retryHookStage(ctx, tenant, row, claim, now, contracts.FailureMalformedFrame)
	}

	u, err = store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(ctx) }()
	if err = store.LockDispatchResult(ctx, u, tenant, row.ID); err != nil {
		return err
	}
	if _, _, err = store.LockResultScope(ctx, u, tenant, action.Use.Attempt); err != nil {
		return err
	}
	harness, err = store.GetHarnessForUpdate(ctx, u, tenant, action.Use.Provider)
	if err != nil {
		return err
	}
	if harness.Mode != lifecycle.HarnessLive {
		return nil
	}
	if err = store.CheckOutboxClaim(ctx, u, tenant, row.ID, claim.Epoch); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	if _, _, err = store.InsertHookStageEvidence(ctx, u, tenant, evidence); err != nil {
		return err
	}
	if err = store.AckOutboxRow(ctx, u, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	return u.Commit(ctx)
}

func hookMethod(kind contracts.HookKind) contracts.Method {
	if kind == contracts.HookActionFailure {
		return contracts.MethodActionFailed
	}
	return contracts.MethodPostAction
}

func hookMatchesAction(hook contracts.HookRequest, action store.GovernedAction) bool {
	r := action.Execution.Request
	if action.Use.Outcome != identity.UseAllowed || hook.Tenant != r.Tenant || hook.Attempt != r.Attempt || hook.Provider != r.Provider || hook.Action != r.Action || hook.Scope != r.Scope || hook.InputDigest != r.InputDigest {
		return false
	}
	if hook.Kind == contracts.HookPostAction {
		return action.Receipt != nil && hook.OutputDigest == action.Receipt.OutputDigest && hook.Failure == nil
	}
	return hook.Failure != nil && hook.Failure.Message == "governed action failed"
}

func decodeSingle[T any](decoder *json.Decoder, value *T) bool {
	if decoder.Decode(value) != nil {
		return false
	}
	var trailing any
	return decoder.Decode(&trailing) == io.EOF
}

func (w *Worker) retryHookStage(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, claim store.OutboxClaim, now time.Time, code contracts.FailureCode) error {
	err := w.retryClaim(ctx, tenant, row, claim, now, code)
	if errors.Is(err, store.ErrStaleVersion) {
		return nil
	}
	return err
}

func (w *Worker) abandonDispatch(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, row store.OutboxRow, id ids.OperationID, neverSent bool, at time.Time) error {
	if row.Record != lifecycle.RecordAuthorityUse {
		return w.abandonOperation(ctx, u, tenant, id, neverSent, at)
	}
	op, err := store.GetOperation(ctx, u, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if op.State == lifecycle.OperationRecorded {
		// Governed operations persist the send marker only after every gate
		// passes. A yielded claim changes the epoch, not this proof that no
		// request was sent.
		neverSent = true
		tr, err := w.transition(op.Version, at)
		if err != nil {
			return err
		}
		op, _, err = store.ApplyOperation(ctx, u, tenant, id, lifecycle.OperationDispatch, "", tr)
		if err != nil {
			return err
		}
	}
	event := lifecycle.OperationSendUnknown
	if neverSent {
		event = lifecycle.OperationSendRejected
	}
	if op.State != lifecycle.OperationDispatched {
		return nil
	}
	tr, err := w.transition(op.Version, at)
	if err != nil {
		return err
	}
	_, _, err = store.ApplyOperation(ctx, u, tenant, id, event, "", tr)
	return err
}
