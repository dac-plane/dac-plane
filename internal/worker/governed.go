package worker

import (
	"context"
	"encoding/json"
	"errors"
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

func (w *Worker) governedRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
	a, err := store.GetGovernedAction(ctx, w.pool, tenant, body.Operation)
	if err != nil {
		return err
	}
	if a.Use.Outcome != identity.UseAllowed || a.Execution.Validate() != nil || body.Operation.String() != row.RecordID.String() || body.Role != contracts.RoleEnforcement || body.Method != contracts.MethodExecuteAction {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	port := w.enforcement[a.Use.Provider]
	if port == nil {
		port = w.ports[a.Use.Provider]
	}
	if port == nil || port.Role() != contracts.RoleEnforcement {
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
	return w.resolveGoverned(ctx, tenant, row, prepared, outcome, receipt, code, time.Now().UTC())
}

func (w *Worker) resolveGoverned(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, p *preparedDispatch, outcome lifecycle.SendStatus, receipt *contracts.EnforcementReceipt, code contracts.FailureCode, now time.Time) error {
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
