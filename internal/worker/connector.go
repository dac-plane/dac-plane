package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

func (w *Worker) connectorRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
	call, err := store.GetConnectorCall(ctx, w.pool, tenant, body.Operation)
	if err != nil {
		return err
	}
	if call.Execution == nil || call.Execution.Validate() != nil || body.Operation.String() != row.RecordID.String() || body.Method != contracts.MethodCallTool {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	port := w.port(tenant, call.Provider, contracts.RoleConnector)
	if port == nil {
		return nil
	}
	p, err := w.prepareDispatch(ctx, tenant, row, body, now)
	if err != nil || p == nil {
		return err
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(call.Execution)
	if err != nil {
		return err
	}
	request := contracts.Request{Operation: call.Operation, Delivery: delivery, Tenant: tenant, Attempt: call.Attempt, Role: contracts.RoleConnector, Method: contracts.MethodCallTool, Arguments: raw, Demand: row.Demand, Deadline: body.Deadline}
	if err := request.Validate(); err != nil {
		return w.exhaustPrepared(ctx, tenant, row, p, now, reasonInvalidBody, false)
	}
	sendCtx, cancel := context.WithTimeout(ctx, min(w.cfg.SendTimeout, time.Until(body.Deadline)))
	defer cancel()
	// Grant removal and attempt termination also stop a request already in
	// flight. Cancel only requests stopping; only a reply proves its outcome.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sendCtx.Done():
				return
			case <-ticker.C:
				a, ae := store.GetAttempt(sendCtx, w.pool, tenant, call.Attempt)
				c, ce := store.GetConnection(sendCtx, w.pool, tenant, call.Connection)
				g, ge := store.GetConnectionGrant(sendCtx, w.pool, tenant, call.ConnectionGrant)
				if ae != nil || ce != nil || ge != nil || (a.State != lifecycle.AttemptExecuting && a.State != lifecycle.AttemptWaitingInput) || c.State != lifecycle.ConnectionActive || g.State != lifecycle.ConnectionGrantActive || !time.Now().Before(g.ExpiresAt) {
					cancelCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.SendTimeout)
					_ = port.Cancel(cancelCtx, contracts.Cancel{Operation: call.Operation, Delivery: delivery})
					stop()
					cancel()
					return
				}
			}
		}
	}()
	reply, sendErr := port.Send(sendCtx, request)
	cancel()
	<-done
	if errors.Is(sendErr, contracts.ErrNotDelivered) {
		return w.releasePrepared(ctx, tenant, row, p, now, true)
	}
	outcome := lifecycle.SendUnknown
	var result *contracts.ConnectorResult
	if sendErr == nil && reply.Validate() == nil && reply.Operation == call.Operation && reply.Delivery == delivery {
		if reply.Durability == lifecycle.SendRejected {
			outcome = lifecycle.SendRejected
		}
		if reply.Durability == lifecycle.SendAccepted {
			var r contracts.ConnectorResult
			if json.Unmarshal(reply.Result, &r) == nil && r.Validate() == nil && r.Operation == call.Operation {
				result = &r
				outcome = lifecycle.SendAccepted
			}
		}
	}
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(ctx) }()
	if err := store.LockDispatchResult(ctx, u, tenant, row.ID); err != nil {
		return err
	}
	if err := store.CheckAttemptLease(ctx, u, tenant, call.Attempt, p.lease); err != nil {
		return err
	}
	if err := store.AckOutboxRow(ctx, u, tenant, row.ID, p.claim.Epoch, time.Now().UTC()); err != nil {
		return err
	}
	tr, err := w.transition(p.operation.Version, time.Now().UTC())
	if err != nil {
		return err
	}
	op, _, err := store.ApplyOperation(ctx, u, tenant, call.Operation, outcome.Event(), "", tr)
	if err != nil {
		return err
	}
	if result != nil {
		if _, err := store.RecordConnectorResult(ctx, u, tenant, *result); err != nil {
			return err
		}
		tr, err := w.transition(op.Version, time.Now().UTC())
		if err != nil {
			return err
		}
		if _, _, err := store.ApplyOperation(ctx, u, tenant, call.Operation, lifecycle.OperationSucceed, "", tr); err != nil {
			return err
		}
	}
	if err := w.releaseLease(ctx, u, tenant, p, time.Now().UTC()); err != nil {
		return err
	}
	return u.Commit(ctx)
}

// reconcileConnector reads a durable result by operation identity. read_call
// is an observation, never a retry of call_tool or an external side effect.
func (w *Worker) reconcileConnector(ctx context.Context, call store.ConnectorCall, op lifecycle.ProviderOperation) error {
	port := w.port(call.Tenant, call.Provider, contracts.RoleConnector)
	if port == nil {
		return nil
	}
	// A lost cancel frame is retried from the durable uncertain operation.
	// The provider's completed result, including a pre-effect cancellation,
	// is the evidence that stops this retry responsibility.
	a, ae := store.GetAttempt(ctx, w.pool, call.Tenant, call.Attempt)
	c, ce := store.GetConnection(ctx, w.pool, call.Tenant, call.Connection)
	g, ge := store.GetConnectionGrant(ctx, w.pool, call.Tenant, call.ConnectionGrant)
	row, re := store.GetOutboxRow(ctx, w.pool, call.Tenant, call.Operation.Untyped())
	body, be := ParseDispatchBody(row.Payload)
	if ae != nil || ce != nil || ge != nil || re != nil || be != nil {
		return errors.Join(ae, ce, ge, re, be)
	}
	if (a.State != lifecycle.AttemptExecuting && a.State != lifecycle.AttemptWaitingInput) || c.State != lifecycle.ConnectionActive || g.State != lifecycle.ConnectionGrantActive || !time.Now().Before(g.ExpiresAt) || !time.Now().Before(body.Deadline) {
		id, err := ids.New[ids.Delivery]()
		if err != nil {
			return err
		}
		cancelCtx, stop := context.WithTimeout(ctx, w.cfg.SendTimeout)
		err = port.Cancel(cancelCtx, contracts.Cancel{Operation: op.ID, Delivery: id})
		stop()
		if err != nil {
			return nil
		}
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	deadline := time.Now().UTC().Add(w.cfg.SendTimeout)
	sendCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	reply, err := port.Send(sendCtx, contracts.Request{Operation: op.ID, Delivery: delivery, Tenant: call.Tenant, Attempt: call.Attempt, Role: contracts.RoleConnector, Method: contracts.MethodReadCall, Demand: contracts.DemandBackground, Deadline: deadline})
	if err != nil || reply.Validate() != nil || reply.Operation != op.ID || reply.Delivery != delivery || reply.Durability != lifecycle.SendAccepted {
		return nil
	}
	var result contracts.ConnectorResult
	if json.Unmarshal(reply.Result, &result) != nil || result.Validate() != nil || result.Operation != op.ID {
		return nil
	}
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(ctx) }()
	current, err := store.GetOperation(ctx, u, call.Tenant, op.ID)
	if err != nil {
		return err
	}
	if current.State != lifecycle.OperationUncertain {
		return nil
	}
	tr, err := w.transition(current.Version, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, _, err := store.ApplyOperation(ctx, u, call.Tenant, op.ID, lifecycle.OperationReconcile, lifecycle.OperationSucceeded, tr); err != nil {
		return err
	}
	if _, err := store.RecordConnectorResult(ctx, u, call.Tenant, result); err != nil {
		return err
	}
	return u.Commit(ctx)
}

// migrationUsesGateway carries the original runtime's negotiated gateway
// obligation through migration ancestry. A new attempt gets a new token, but
// restart redelivery cannot change whether the payload contains that token.
func (w *Worker) migrationUsesGateway(ctx context.Context, tenant ids.TenantID, source ids.AttemptID) (bool, error) {
	var enabled bool
	err := w.pool.QueryRow(ctx, `with recursive lineage(attempt_id) as (
 select $2::uuid union select m.source_attempt_id from migration_item m join lineage l on m.target_attempt_id=l.attempt_id where m.tenant_id=$1
 ) select exists(select 1 from lineage l join outbox o on o.attempt_id=l.attempt_id where o.tenant_id=$1 and o.payload->>'gateway_enabled'='true')`, tenant.String(), source.String()).Scan(&enabled)
	return enabled, err
}
