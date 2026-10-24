package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// decisionRow delivers one decided attention request to its runtime. The
// request stamps dispatching (a re-claimed row re-stamps it as a redelivery
// of the same decision identity), the steer goes to the attempt's operation,
// and only a provider-accepted reply marks the request delivered; a rejected
// steer supersedes the wait — its native request cannot hear the answer — and
// a transport failure or unknown reply backs off and redelivers under the
// same identity. A spent retry budget leaves the request dispatching and the
// row exhausted: inspectable evidence, never the assumption that the runtime
// resumed.
func (w *Worker) decisionRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, now time.Time) (err error) {
	defer func() {
		// The inner handler has already rolled back. A lost fence is a
		// no-fault race, including expiry at a final settlement check; it
		// must not prevent the next row in this tenant's batch from running.
		if errors.Is(err, store.ErrStaleVersion) {
			err = nil
		}
	}()
	body, err := ParseDecisionBody(row.Payload)
	if err != nil {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	prepared, err := w.prepareDecision(ctx, tenant, row, now)
	if err != nil || prepared == nil {
		return err
	}
	port := w.port(tenant, prepared.operation.Provider, contracts.RoleRuntime)
	if port == nil {
		w.stats.SkippedNoPort.Add(1)
		return w.releaseDecision(ctx, tenant, row, prepared, now)
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	request := contracts.Request{
		Operation: prepared.operation.ID, Delivery: delivery,
		Tenant: tenant, Attempt: prepared.attention.Attempt,
		Role: contracts.RoleRuntime, Method: contracts.MethodSteerTurn,
		Arguments: row.Payload, Demand: row.Demand, Deadline: body.ExpiresAt,
	}
	if err := request.Validate(); err != nil {
		return w.exhaustDecision(ctx, tenant, row, prepared, now, reasonInvalidBody)
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	defer cancel()
	reply, err := port.Send(sendCtx, request)
	if err != nil {
		return w.retryDecision(ctx, tenant, row, prepared, now, contracts.FailureTransportUnavailable)
	}
	return w.resolveDecision(ctx, tenant, row, prepared, delivery, reply, now)
}

// preparedDecision is a claimed delivery row past its checklist: the request
// stamped dispatching, the operation the steer names, and the attempt lease
// as of the claim transaction.
type preparedDecision struct {
	claim     store.OutboxClaim
	attention lifecycle.AttentionRequest
	operation lifecycle.ProviderOperation
	lease     store.AttemptLease
}

// prepareDecision claims a delivery row and runs the delivery checklist in
// one transaction: only a decided request is delivered (dispatching stamps a
// redelivery), the attempt lease is held, and the operation the steer names
// resolves. A nil return with no error means the row was consumed inside.
func (w *Worker) prepareDecision(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, now time.Time) (*preparedDecision, error) {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	commit := func() error {
		if err := uow.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	}
	ack := func(claim store.OutboxClaim) (*preparedDecision, error) {
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
			return nil, err
		}
		w.stats.Acks.Add(1)
		return nil, commit()
	}
	exhaust := func(claim store.OutboxClaim, reason string) (*preparedDecision, error) {
		if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, claim.Epoch); err != nil {
			return nil, err
		}
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, reason, time.Now().UTC()); err != nil {
			return nil, err
		}
		w.stats.Exhausts.Add(1)
		return nil, commit()
	}
	release := func(claim store.OutboxClaim) (*preparedDecision, error) {
		if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, claim.Epoch); err != nil {
			return nil, err
		}
		if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
			return nil, err
		}
		return nil, commit()
	}

	claim, err := store.ClaimOutboxRowByID(ctx, uow, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil, nil
		}
		return nil, err
	}
	if reason, spent := w.budgetSpent(row); spent {
		// Attempts count sends. A due delivery whose budget is already spent
		// exhausts without steering the runtime one more time.
		return exhaust(claim, reason)
	}
	attentionID, err := ids.Parse[ids.Attention](row.RecordID.String())
	if err != nil {
		return nil, err
	}
	attention, err := store.GetAttention(ctx, uow, tenant, attentionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The delivery row outlived its request (a producer error);
			// acking keeps it done and inspectable.
			return ack(claim)
		}
		return nil, err
	}
	// Only a decided request delivers; a re-claimed dispatching one
	// re-stamps the same decision as a redelivery; every other state already
	// has its story, so a duplicate row acks without a second send.
	event := lifecycle.AttentionDispatch
	switch attention.State {
	case lifecycle.AttentionDecided:
	case lifecycle.AttentionDispatching:
		event = lifecycle.AttentionRedeliver
	default:
		return ack(claim)
	}
	if _, _, err := store.LockResultScope(ctx, uow, tenant, attention.Attempt); err != nil {
		return nil, err
	}
	// Steer the parent runtime, never the distinct action operation named
	// by an approval request.
	operationID := ids.OperationID{}
	{
		latest, err := store.GetOperationByAttempt(ctx, uow, tenant, attention.Attempt)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return exhaust(claim, reasonNoOperation)
			}
			return nil, err
		}
		operationID = latest.ID
	}
	operation, err := store.GetOperation(ctx, uow, tenant, operationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exhaust(claim, reasonNoOperation)
		}
		return nil, err
	}
	harness, err := store.GetHarnessForUpdate(ctx, uow, tenant, operation.Provider)
	if errors.Is(err, store.ErrNotFound) {
		return release(claim)
	}
	if err != nil {
		return nil, err
	}
	if !harness.Mode.Authoritative() {
		// Qualification may change after the command was recorded. Retain
		// its delivery intent without changing the native request's state.
		return release(claim)
	}
	if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, claim.Epoch); err != nil {
		return nil, err
	}
	now = time.Now().UTC()
	lease, err := store.ClaimAttemptLease(ctx, uow, tenant, attention.Attempt, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return release(claim)
		}
		return nil, err
	}
	attention, applied, err := w.moveAttention(ctx, uow, tenant, attention, event, now)
	if err != nil {
		return nil, err
	}
	if !applied {
		// A racing ingestion already closed the wait. Roll back the lease
		// and claim; the next tick can acknowledge the current state.
		return nil, nil
	}
	if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, claim.Epoch); err != nil {
		return nil, err
	}
	if err := store.CheckAttemptLease(ctx, uow, tenant, attention.Attempt, lease); err != nil {
		return nil, err
	}
	if err := commit(); err != nil {
		return nil, err
	}
	w.stats.Claims.Add(1)
	return &preparedDecision{claim: claim, attention: attention, operation: operation, lease: lease}, nil
}

// lockDecisionSettlement uses the dispatch lock order for every post-send
// path, including errors. Expiry or demotion cannot authorize a retry write
// or lease release just because the original claim's epoch still matches.
func (w *Worker) lockDecisionSettlement(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDecision) (bool, error) {
	if err := store.LockDispatchResult(ctx, u, tenant, row.ID); err != nil {
		return false, err
	}
	if _, _, err := store.LockResultScope(ctx, u, tenant, prepared.attention.Attempt); err != nil {
		return false, err
	}
	h, err := store.GetHarnessForUpdate(ctx, u, tenant, prepared.operation.Provider)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !h.Mode.Authoritative() {
		return false, err
	}
	if err := store.CheckOutboxClaim(ctx, u, tenant, row.ID, prepared.claim.Epoch); err != nil {
		return false, err
	}
	if err := store.CheckAttemptLease(ctx, u, tenant, prepared.attention.Attempt, prepared.lease); err != nil {
		return false, err
	}
	return true, nil
}

// resolveDecision applies a provider reply to a claimed delivery: accepted
// marks the request delivered and acks the row; rejected supersedes the wait
// and exhausts the row; an unknown reply retries like a transport loss —
// it may have landed, so delivery is never assumed — and pending leaves the
// row processing for the lease lapse.
func (w *Worker) resolveDecision(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDecision, delivery ids.DeliveryID, reply contracts.Reply, now time.Time) error {
	if reply.Operation != prepared.operation.ID || reply.Delivery != delivery {
		return w.exhaustDecision(ctx, tenant, row, prepared, now, reasonMalformedReply)
	}
	if err := reply.Validate(); err != nil {
		return w.exhaustDecision(ctx, tenant, row, prepared, now, reasonMalformedReply)
	}
	switch reply.Durability {
	case lifecycle.SendUnknown:
		return w.retryDecision(ctx, tenant, row, prepared, now, contracts.FailureTransportUnavailable)
	case lifecycle.SendPending:
		// The provider has not decided; the row stays processing and the
		// lease lapse reclaims it.
		return nil
	}
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if live, err := w.lockDecisionSettlement(ctx, uow, tenant, row, prepared); err != nil || !live {
		return err
	}
	now = time.Now().UTC()
	if reply.Durability == lifecycle.SendRejected {
		// The steer cannot reach the native request: the wait supersedes as
		// the only end reachable from a rejected delivery. Work stays as the
		// attempt machinery keeps it.
		if _, _, err := w.moveAttention(ctx, uow, tenant, prepared.attention, lifecycle.AttentionSupersede, now); err != nil {
			return err
		}
		if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, prepared.claim.Epoch); err != nil {
			return err
		}
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, reasonDecisionRejected, now); err != nil {
			return err
		}
	} else {
		if _, _, err := w.moveAttention(ctx, uow, tenant, prepared.attention, lifecycle.AttentionDeliver, now); err != nil {
			return err
		}
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, time.Now().UTC()); err != nil {
			return err
		}
	}
	if err := store.CheckAttemptLease(ctx, uow, tenant, prepared.attention.Attempt, prepared.lease); err != nil {
		return err
	}
	if err := w.releaseDecisionLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if reply.Durability == lifecycle.SendRejected {
		w.stats.Exhausts.Add(1)
	} else {
		w.stats.Acks.Add(1)
	}
	return nil
}

// moveAttention applies one non-decision attention event at the request's
// current version, re-reading once when a racing writer moved it first. It
// reports whether this call's event applied: an event the request's state no
// longer allows — a wait closed concurrently, a delivery already marked —
// applies nothing, which callers treat as the state having its story already.
func (w *Worker) moveAttention(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, request lifecycle.AttentionRequest, event lifecycle.AttentionEvent, now time.Time) (lifecycle.AttentionRequest, bool, error) {
	if _, _, err := store.LockResultScope(ctx, uow, tenant, request.Attempt); err != nil {
		return request, false, err
	}
	current := request
	for try := 0; try < 2; try++ {
		tr, err := w.transition(current.Version, now)
		if err != nil {
			return current, false, err
		}
		moved, _, err := store.ApplyAttention(ctx, uow, tenant, current.ID, event, tr)
		if err == nil {
			return moved, true, nil
		}
		if !errors.Is(err, store.ErrStaleVersion) && !isRefusal(err) {
			return current, false, err
		}
		fresh, readErr := store.GetAttention(ctx, uow, tenant, current.ID)
		if readErr != nil {
			if errors.Is(readErr, store.ErrNotFound) {
				return current, false, nil
			}
			return current, false, readErr
		}
		current = fresh
	}
	return current, false, nil
}

// retryDecision reschedules a delivery whose send failed or whose outcome is
// unknown: the same decision is redelivered under the backoff, never marked
// delivered on a guess. A spent budget leaves the request dispatching and
// exhausts the row.
func (w *Worker) retryDecision(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDecision, now time.Time, code contracts.FailureCode) error {
	retryIn, exhausted := w.cfg.Policy.Classify(row.Attempts, code)
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if live, err := w.lockDecisionSettlement(ctx, uow, tenant, row, prepared); err != nil || !live {
		return err
	}
	now = time.Now().UTC()
	if exhausted {
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, string(code), now); err != nil {
			return err
		}
	} else if err := store.ScheduleOutboxRetry(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now.Add(retryIn), string(code), now); err != nil {
		return err
	}
	if err := w.releaseDecisionLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if exhausted {
		w.stats.Exhausts.Add(1)
	} else {
		w.stats.Retries.Add(1)
	}
	return nil
}

// exhaustDecision exhausts a claimed delivery row and drops its lease.
func (w *Worker) exhaustDecision(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDecision, now time.Time, reason string) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if live, err := w.lockDecisionSettlement(ctx, uow, tenant, row, prepared); err != nil || !live {
		return err
	}
	now = time.Now().UTC()
	if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, reason, now); err != nil {
		return err
	}
	if err := w.releaseDecisionLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	w.stats.Exhausts.Add(1)
	return nil
}

// releaseDecision yields a claimed delivery row without burning the retry
// budget and drops its lease.
func (w *Worker) releaseDecision(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDecision, now time.Time) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if live, err := w.lockDecisionSettlement(ctx, uow, tenant, row, prepared); err != nil || !live {
		return err
	}
	now = time.Now().UTC()
	if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now); err != nil {
		return err
	}
	if err := w.releaseDecisionLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// releaseDecisionLease drops the attempt lease held for one delivery. A
// taken-over lease is someone else's now and reports no error.
func (w *Worker) releaseDecisionLease(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, prepared *preparedDecision, now time.Time) error {
	if prepared.lease.Owner == "" {
		return nil
	}
	if err := store.ReleaseAttemptLease(ctx, uow, tenant, prepared.attention.Attempt, prepared.lease, now); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	return nil
}

// isRefusal reports whether err is a frozen-machine refusal rather than an
// infrastructure failure.
func isRefusal(err error) bool {
	var refusal *lifecycle.TransitionError
	return errors.As(err, &refusal)
}
