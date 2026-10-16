package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Static last_error reasons. The column carries typed failure codes or one
// of these; never raw provider text or payloads.
const (
	reasonInvalidBody       = "invalid_dispatch_body"
	reasonNoHandler         = "no_dispatch_handler"
	reasonOperationMismatch = "operation_mismatch"
	reasonUnknownOperation  = "unknown_operation"
	reasonUnknownState      = "unknown_operation_state"
	reasonNoHarness         = "no_harness"
	reasonHarnessNotLive    = "harness_not_live"
	reasonUnknownAttempt    = "unknown_attempt"
	reasonBudgetExhausted   = "budget_exhausted"
	reasonReservationMixup  = "reservation_mismatch"
	reasonDeadlineExceeded  = "deadline_exceeded"
	reasonTransportFailed   = "transport_unavailable"
	reasonMalformedReply    = "malformed_reply"
)

func (w *Worker) transition(expectedVersion uint64, at time.Time) (store.Transition, error) {
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return store.Transition{}, err
	}
	return store.Transition{ExpectedVersion: expectedVersion, At: at, Actor: SystemActor(), AuditID: auditID}, nil
}

// handleRow routes one ready row: cancel intents to the provider's cancel
// path, operation dispatches through the pre-dispatch checklist and send,
// and anything else to poison. Poison rows are exhausted with a static
// reason so they stay inspectable and never spin.
func (w *Worker) handleRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, now time.Time) error {
	body, err := ParseDispatchBody(row.Payload)
	if err != nil {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	if body.CancelReason != "" {
		return w.cancelRow(ctx, tenant, row, body, now)
	}
	if row.Record != lifecycle.RecordOperation {
		return w.poison(ctx, tenant, row, now, reasonNoHandler)
	}
	if body.Operation.String() != row.RecordID.String() {
		return w.poison(ctx, tenant, row, now, reasonOperationMismatch)
	}
	return w.dispatchRow(ctx, tenant, row, body, now)
}

// poison claims and exhausts a row the worker cannot dispatch.
func (w *Worker) poison(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, now time.Time, reason string) error {
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
	claim, err := store.ClaimOutboxRowByID(ctx, uow, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, reason, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	w.stats.Poison.Add(1)
	w.stats.Exhausts.Add(1)
	return nil
}

// cancelRow delivers one protocol cancellation. The attempt stays in
// cancellation_requested until both stop observations land; acknowledgement
// here means the provider received the intent, not that anything stopped.
func (w *Worker) cancelRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
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
	claim, err := store.ClaimOutboxRowByID(ctx, uow, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	operation, err := store.GetOperation(ctx, uow, tenant, body.Operation)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Nothing in flight: idempotent cancel succeeds.
			if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, now); err != nil {
				return err
			}
			if err := uow.Commit(ctx); err != nil {
				return err
			}
			committed = true
			w.stats.Acks.Add(1)
			return nil
		}
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true

	port, ok := w.ports[operation.Provider]
	if !ok {
		w.stats.SkippedNoPort.Add(1)
		return w.release(ctx, tenant, row, claim, now)
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	defer cancel()
	if err := port.Cancel(sendCtx, contracts.Cancel{Operation: body.Operation, Delivery: delivery}); err != nil {
		return w.retryClaim(ctx, tenant, row, claim, now, reasonTransportFailed)
	}
	return w.ackClaim(ctx, tenant, row, claim, now)
}

// dispatchRow drives one provider send: claim and checklist, send outside
// the transaction, then resolve. Every mutation presents the claim epoch;
// losing a race releases the row instead of fighting.
func (w *Worker) dispatchRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) error {
	prepared, err := w.prepareDispatch(ctx, tenant, row, body, now)
	if err != nil || prepared == nil {
		return err
	}
	port, ok := w.ports[prepared.operation.Provider]
	if !ok {
		w.stats.SkippedNoPort.Add(1)
		return w.releasePrepared(ctx, tenant, row, prepared, now)
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return err
	}
	request := contracts.Request{
		Operation: prepared.operation.ID, Delivery: delivery,
		Tenant: tenant, Attempt: prepared.operation.Attempt,
		Role: body.Role, Method: body.Method, Arguments: body.Arguments,
		Demand: row.Demand, Deadline: body.Deadline,
	}
	if err := request.Validate(); err != nil {
		return w.exhaustPrepared(ctx, tenant, row, prepared, now, reasonInvalidBody)
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	defer cancel()
	reply, err := port.Send(sendCtx, request)
	if err != nil {
		return w.retryPrepared(ctx, tenant, row, prepared, now, reasonTransportFailed)
	}
	return w.resolveSend(ctx, tenant, row, prepared, reply, now)
}

// preparedDispatch is a claimed row that passed the checklist, with the
// operation and the attempt lease as of the claim transaction.
type preparedDispatch struct {
	claim     store.OutboxClaim
	operation lifecycle.ProviderOperation
	lease     store.AttemptLease
}

// prepareDispatch claims the row and runs the pre-dispatch checklist in one
// transaction: operation exists and needs sending, harness is live (shadow
// records and acks without sending), budget covers the amount, the deadline
// stands, and the attempt lease is held. A nil return with no error means
// the row was consumed or released inside.
func (w *Worker) prepareDispatch(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, body DispatchBody, now time.Time) (*preparedDispatch, error) {
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
	// commit resolves the transaction one way; every terminal path below
	// goes through it so no mutation is lost to the deferred rollback.
	commit := func() error {
		if err := uow.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	}
	release := func(claim store.OutboxClaim) (*preparedDispatch, error) {
		if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, now); err != nil && !errors.Is(err, store.ErrStaleVersion) {
			return nil, err
		}
		return nil, commit()
	}
	exhaust := func(claim store.OutboxClaim, reason string) (*preparedDispatch, error) {
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, reason, now); err != nil {
			return nil, err
		}
		w.stats.Exhausts.Add(1)
		return nil, commit()
	}
	ack := func(claim store.OutboxClaim) (*preparedDispatch, error) {
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, now); err != nil {
			return nil, err
		}
		w.stats.Acks.Add(1)
		return nil, commit()
	}

	claim, err := store.ClaimOutboxRowByID(ctx, uow, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil, nil
		}
		return nil, err
	}
	operation, err := store.GetOperation(ctx, uow, tenant, body.Operation)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exhaust(claim, reasonUnknownOperation)
		}
		return nil, err
	}
	switch operation.State {
	case lifecycle.OperationSucceeded, lifecycle.OperationFailed, lifecycle.OperationRejected:
		return ack(claim)
	case lifecycle.OperationUncertain:
		// The reconciler owns uncertain operations; the intent is recorded.
		return ack(claim)
	case lifecycle.OperationRecorded, lifecycle.OperationDispatched, lifecycle.OperationAccepted:
		// Recorded operations promote to dispatched only at the end, once
		// every checklist gate passed: an exhausted row must never leave
		// behind a dispatched-never-sent operation.
	default:
		return exhaust(claim, reasonUnknownState)
	}
	if now.After(body.Deadline) {
		return exhaust(claim, reasonDeadlineExceeded)
	}
	harness, err := store.GetHarness(ctx, uow, tenant, operation.Provider)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exhaust(claim, reasonNoHarness)
		}
		return nil, err
	}
	if !harness.Mode.Authoritative() {
		if harness.Mode == lifecycle.HarnessShadow {
			if err := w.recordShadow(ctx, uow, tenant, operation, row, now); err != nil {
				return nil, err
			}
			return ack(claim)
		}
		return exhaust(claim, reasonHarnessNotLive)
	}
	if err := w.checkBudget(ctx, uow, tenant, operation, body, now); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return release(claim)
		}
		var exhausted *exhaustedError
		if errors.As(err, &exhausted) {
			return exhaust(claim, exhausted.reason)
		}
		return nil, err
	}
	lease, err := store.ClaimAttemptLease(ctx, uow, tenant, operation.Attempt, w.owner, w.cfg.ClaimTTL, now)
	if err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return release(claim)
		}
		return nil, err
	}
	if operation.State == lifecycle.OperationRecorded {
		// Promote only now that every gate passed: the row below commits
		// together with a send that follows, so dispatched always means
		// a delivery is in flight or recorded.
		tr, err := w.transition(operation.Version, now)
		if err != nil {
			return nil, err
		}
		operation, _, err = store.ApplyOperation(ctx, uow, tenant, operation.ID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr)
		if err != nil {
			if errors.Is(err, store.ErrStaleVersion) {
				return release(claim)
			}
			return nil, err
		}
	}
	if err := commit(); err != nil {
		return nil, err
	}
	w.stats.Claims.Add(1)
	return &preparedDispatch{claim: claim, operation: operation, lease: lease}, nil
}

// exhaustedError carries a static give-up reason through helpers that
// otherwise return only errors.
type exhaustedError struct {
	reason string
}

func (e *exhaustedError) Error() string {
	return "dispatch exhausted: " + e.reason
}

// checkBudget enforces the ceiling gate for one dispatch. No budget row for
// the unit means unconstrained. The reservation identity derives from the
// attempt, so a reclaimed tick replays instead of double-reserving; an
// amount mismatch with the stored reservation is a producer error.
func (w *Worker) checkBudget(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, operation lifecycle.ProviderOperation, body DispatchBody, now time.Time) error {
	if body.Unit == "" {
		return nil
	}
	attempt, err := store.GetAttempt(ctx, uow, tenant, operation.Attempt)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &exhaustedError{reason: reasonUnknownAttempt}
		}
		return err
	}
	budget, err := store.GetBudgetForUpdateByTaskUnit(ctx, uow, tenant, attempt.Task, body.Unit)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	reservationID, err := ids.Parse[ids.Reservation](attempt.ID.String())
	if err != nil {
		return err
	}
	if stored, err := store.GetReservation(ctx, uow, tenant, reservationID); err == nil {
		if stored.Amount != body.Amount {
			return &exhaustedError{reason: reasonReservationMixup}
		}
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	tr, err := w.transition(budget.Version, now)
	if err != nil {
		return err
	}
	_, _, _, err = store.ReserveBudget(ctx, uow, store.ReservationRequest{
		Tenant: tenant, Budget: budget.ID, Reservation: reservationID,
		Attempt: attempt.ID, Amount: body.Amount,
	}, tr)
	if err != nil {
		if transitionRejected(err, lifecycle.RejectCeilingReached) {
			return &exhaustedError{reason: reasonBudgetExhausted}
		}
		return err
	}
	return nil
}

// transitionRejected reports whether err is a lifecycle rejection with one
// of the given codes.
func transitionRejected(err error, codes ...lifecycle.RejectionCode) bool {
	var transitionErr *lifecycle.TransitionError
	if !errors.As(err, &transitionErr) {
		return false
	}
	for _, code := range codes {
		if transitionErr.Code == code {
			return true
		}
	}
	return false
}

// recordShadow stores the withheld dispatch as shadow comparison evidence.
// Shadow harnesses never cause provider effects; the digest covers the
// dispatch envelope the live path would have sent.
func (w *Worker) recordShadow(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, operation lifecycle.ProviderOperation, row store.OutboxRow, now time.Time) error {
	shadowID, err := ids.New[ids.Any]()
	if err != nil {
		return err
	}
	_, _, err = store.RecordShadowResult(ctx, uow, store.ShadowResult{
		ID: shadowID, Tenant: tenant, Attempt: operation.Attempt, CapturedAt: now,
		Summary: "shadowed dispatch withheld", Comparison: "no live counterpart",
		Digest: identity.DigestOf(row.Payload),
	})
	return err
}

// resolveSend applies a provider reply: accepted and rejected move the
// operation terminal and ack the row; unknown parks the operation in
// uncertain for the reconciler; pending leaves the row processing so the
// lease lapse reclaims it.
func (w *Worker) resolveSend(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, reply contracts.Reply, now time.Time) error {
	if reply.Operation.String() != prepared.operation.ID.String() {
		return w.exhaustPrepared(ctx, tenant, row, prepared, now, reasonMalformedReply)
	}
	if err := reply.Validate(); err != nil {
		return w.exhaustPrepared(ctx, tenant, row, prepared, now, reasonMalformedReply)
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
	switch reply.Durability {
	case lifecycle.SendAccepted, lifecycle.SendRejected, lifecycle.SendUnknown:
		event := reply.Durability.Event()
		target := map[lifecycle.OperationEvent]lifecycle.OperationState{
			lifecycle.OperationSendAccepted: lifecycle.OperationAccepted,
			lifecycle.OperationSendRejected: lifecycle.OperationRejected,
			lifecycle.OperationSendUnknown:  lifecycle.OperationUncertain,
		}[event]
		tr, err := w.transition(prepared.operation.Version, now)
		if err != nil {
			return err
		}
		if _, _, err := store.ApplyOperation(ctx, uow, tenant, prepared.operation.ID, event, target, tr); err != nil {
			if errors.Is(err, store.ErrStaleVersion) {
				return w.releaseCommitted(ctx, uow, tenant, row, prepared, now, &committed)
			}
			return err
		}
		if reply.Durability == lifecycle.SendUnknown {
			w.logger.Info("operation entered reconciliation", "operation", prepared.operation.ID.String())
		}
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now); err != nil {
			return err
		}
		if err := w.releaseLease(ctx, uow, tenant, prepared, now); err != nil {
			return err
		}
		if err := uow.Commit(ctx); err != nil {
			return err
		}
		committed = true
		w.stats.Acks.Add(1)
		return nil
	default: // SendPending: the provider has not decided; leave processing.
		if err := uow.Commit(ctx); err != nil {
			return err
		}
		committed = true
		return nil
	}
}

// retryPrepared reschedules a claimed row with backoff, or exhausts it when
// the budget is gone or the failure can never succeed.
func (w *Worker) retryPrepared(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, now time.Time, code contracts.FailureCode) error {
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
	if exhausted {
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, string(code), now); err != nil {
			return err
		}
	} else if err := store.ScheduleOutboxRetry(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now.Add(retryIn), string(code), now); err != nil {
		return err
	}
	if err := w.releaseLease(ctx, uow, tenant, prepared, now); err != nil {
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

// retryClaim reschedules a row whose claim is still held (cancel path).
func (w *Worker) retryClaim(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, claim store.OutboxClaim, now time.Time, code contracts.FailureCode) error {
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
	if exhausted {
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, string(code), now); err != nil {
			return err
		}
	} else if err := store.ScheduleOutboxRetry(ctx, uow, tenant, row.ID, claim.Epoch, now.Add(retryIn), string(code), now); err != nil {
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

// exhaustPrepared exhausts a claimed dispatch row and drops its lease.
func (w *Worker) exhaustPrepared(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, now time.Time, reason string) error {
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
	if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, reason, now); err != nil {
		return err
	}
	if err := w.releaseLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	w.stats.Exhausts.Add(1)
	return nil
}

// ackClaim acknowledges a row whose claim is still held (cancel path).
func (w *Worker) ackClaim(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, claim store.OutboxClaim, now time.Time) error {
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
	if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	w.stats.Acks.Add(1)
	return nil
}

// release yields a held row without burning the retry budget.
func (w *Worker) release(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, claim store.OutboxClaim, now time.Time) error {
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
	if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, now); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			committed = true
			return nil
		}
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// releaseCommitted releases inside the caller's transaction and commits it,
// for resolve paths that already handled the row another way.
func (w *Worker) releaseCommitted(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, now time.Time, committed *bool) error {
	if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now); err != nil && !errors.Is(err, store.ErrStaleVersion) {
		return err
	}
	if err := w.releaseLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	*committed = true
	return nil
}

// releasePrepared releases a claimed dispatch row and drops its lease.
func (w *Worker) releasePrepared(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, now time.Time) error {
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
	if err := store.ReleaseOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, now); err != nil && !errors.Is(err, store.ErrStaleVersion) {
		return err
	}
	if err := w.releaseLease(ctx, uow, tenant, prepared, now); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// releaseLease drops the attempt lease held for one dispatch. A taken-over
// lease is someone else's now and reports no error.
func (w *Worker) releaseLease(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, prepared *preparedDispatch, now time.Time) error {
	if prepared.lease.Owner == "" {
		return nil
	}
	if err := store.ReleaseAttemptLease(ctx, uow, tenant, prepared.operation.Attempt, prepared.lease, now); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			return nil
		}
		return err
	}
	return nil
}
