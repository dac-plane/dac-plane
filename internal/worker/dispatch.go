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
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
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
	reasonRetryBudgetSpent  = "retry_budget_exhausted"
	reasonNoOperation       = "no_operation"
	reasonDecisionRejected  = "decision_rejected"
	reasonAttemptCancelling = "attempt_cancelling"
	reasonSendStarted       = "send_started"
)

func (w *Worker) transition(expectedVersion uint64, at time.Time) (store.Transition, error) {
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return store.Transition{}, err
	}
	return store.Transition{ExpectedVersion: expectedVersion, At: at, Actor: SystemActor(), AuditID: auditID}, nil
}

// handleRow routes one ready row: decided attention deliveries to their
// delivery path, cancel intents to the provider's cancel path, operation
// dispatches through the pre-dispatch checklist and send, and anything else
// to poison. Poison rows are exhausted with a static reason so they stay
// inspectable and never spin.
func (w *Worker) handleRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, now time.Time) error {
	if row.Record == lifecycle.RecordArtifact && row.Event == "stage" {
		return w.artifactRow(ctx, tenant, row)
	}
	if row.Record == lifecycle.RecordAttention {
		if row.Event != string(lifecycle.AttentionDecide) {
			return w.poison(ctx, tenant, row, now, reasonNoHandler)
		}
		return w.decisionRow(ctx, tenant, row, now)
	}
	body, err := ParseDispatchBody(row.Payload)
	if err != nil {
		return w.poison(ctx, tenant, row, now, reasonInvalidBody)
	}
	if body.CancelReason != "" {
		return w.cancelRow(ctx, tenant, row, body, now)
	}
	if body.Role == contracts.RoleEnforcement && (body.Method == contracts.MethodPostAction || body.Method == contracts.MethodActionFailed) {
		return w.hookStageRow(ctx, tenant, row, body, now)
	}
	if row.Record == lifecycle.RecordAuthorityUse {
		return w.governedRow(ctx, tenant, row, body, now)
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
	if reason, spent := w.budgetSpent(row); spent {
		// The budget counts sends; a due row with a spent budget is
		// exhausted without another cancel attempt.
		if err := store.ExhaustOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, reason, now); err != nil {
			return err
		}
		if err := uow.Commit(ctx); err != nil {
			return err
		}
		committed = true
		w.stats.Exhausts.Add(1)
		return nil
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

	port := w.port(tenant, operation.Provider, body.Role)
	if port == nil {
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
	// Missing wiring is known before the durable send marker, not uncertainty.
	if op, err := store.GetOperation(ctx, w.pool, tenant, body.Operation); err == nil {
		if w.port(tenant, op.Provider, body.Role) == nil {
			w.stats.SkippedNoPort.Add(1)
			return nil
		}
	}
	prepared, err := w.prepareDispatch(ctx, tenant, row, body, now)
	if err != nil || prepared == nil {
		return err
	}
	port := w.port(tenant, prepared.operation.Provider, body.Role)
	if port == nil {
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
		return w.exhaustPrepared(ctx, tenant, row, prepared, now, reasonInvalidBody, false)
	}
	sendCtx, cancel := context.WithTimeout(ctx, min(w.cfg.SendTimeout, time.Until(body.Deadline)))
	defer cancel()
	reply, err := port.Send(sendCtx, request)
	if errors.Is(err, contracts.ErrNotDelivered) {
		// The transport wrote nothing, so nothing can have taken effect.
		w.stats.SkippedNoPort.Add(1)
		return w.releasePrepared(ctx, tenant, row, prepared, now)
	}
	if err != nil {
		reply = uncertainReply(request.Operation, delivery)
	}
	return w.resolveSend(ctx, tenant, row, prepared, delivery, reply, time.Now().UTC())
}

func uncertainReply(operation ids.OperationID, delivery ids.DeliveryID) contracts.Reply {
	return contracts.Reply{Operation: operation, Delivery: delivery, Outcome: contracts.OutcomeFailed, Durability: lifecycle.SendUnknown,
		Failure: &contracts.Failure{Code: contracts.FailureTransportUnavailable, Message: "provider outcome requires reconciliation"}}
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
		if err := w.abandonDispatch(ctx, uow, tenant, row, body.Operation, claim.Epoch == 1, now); err != nil {
			return nil, err
		}
		w.stats.Exhausts.Add(1)
		return nil, commit()
	}
	ack := func(claim store.OutboxClaim) (*preparedDispatch, error) {
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
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
	if reason, spent := w.budgetSpent(row); spent {
		// The budget counts sends; a due row with a spent budget is
		// exhausted without another send.
		return exhaust(claim, reason)
	}
	operation, err := store.GetOperation(ctx, uow, tenant, body.Operation)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exhaust(claim, reasonUnknownOperation)
		}
		return nil, err
	}
	if row.Record == lifecycle.RecordOperation {
		if _, _, err = store.LockResultScope(ctx, uow, tenant, operation.Attempt); err != nil {
			return nil, err
		}
	}
	switch operation.State {
	case lifecycle.OperationSucceeded, lifecycle.OperationFailed, lifecycle.OperationRejected:
		return ack(claim)
	case lifecycle.OperationUncertain:
		// The reconciler owns uncertain operations; the intent is recorded.
		return ack(claim)
	case lifecycle.OperationAccepted:
		if row.Record == lifecycle.RecordOperation {
			return ack(claim)
		}
	case lifecycle.OperationRecorded, lifecycle.OperationDispatched:
		// Recorded operations promote to dispatched only at the end, once
		// every checklist gate passed: an exhausted row must never leave
		// behind a dispatched-never-sent operation.
	default:
		return exhaust(claim, reasonUnknownState)
	}
	if row.Record == lifecycle.RecordOperation && operation.State == lifecycle.OperationDispatched && (claim.Row.LastError != "" || row.Status == "processing") {
		// A committed send marker survived its owner. No reply is not proof
		// that the provider did nothing; takeover must inspect, not resend.
		h, err := store.GetHarnessForUpdate(ctx, uow, tenant, operation.Provider)
		if err != nil {
			return nil, err
		}
		if !h.Mode.Authoritative() {
			return release(claim)
		}
		if err := w.abandonOperation(ctx, uow, tenant, operation.ID, false, time.Now().UTC()); err != nil {
			return nil, err
		}
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
			return nil, err
		}
		return nil, commit()
	}
	if row.Record == lifecycle.RecordAuthorityUse && operation.State != lifecycle.OperationRecorded {
		// A previous claim committed the send marker. Its effect is unknown
		// after a crash; a replacement worker must never issue the action again.
		event := lifecycle.OperationSendUnknown
		if operation.State == lifecycle.OperationAccepted {
			event = lifecycle.OperationOutcomeUnknown
		}
		tr, err := w.transition(operation.Version, now)
		if err != nil {
			return nil, err
		}
		if _, _, err = store.ApplyOperation(ctx, uow, tenant, operation.ID, event, "", tr); err != nil {
			return nil, err
		}
		return ack(claim)
	}
	if row.Record == lifecycle.RecordAuthorityUse {
		reason, err := store.CheckGovernedDispatch(ctx, uow, tenant, operation.ID)
		if err != nil {
			return nil, err
		}
		checkedAt := time.Now().UTC()
		claimExpired := !checkedAt.Before(now.Add(w.cfg.ClaimTTL))
		now = checkedAt
		if reason != "" {
			return exhaust(claim, string(reason))
		}
		// Authorization may have waited on a row lock. The remaining gates
		// and attempt lease must use current time as well.
		if claimExpired {
			return release(claim)
		}
	}
	attempt, err := store.GetAttempt(ctx, uow, tenant, operation.Attempt)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return exhaust(claim, reasonUnknownAttempt)
		}
		return nil, err
	}
	if attempt.State == lifecycle.AttemptCancellationRequested || attempt.State == lifecycle.AttemptTerminated {
		// Cancellation supersedes every outstanding start delivery. In
		// particular, a transport-failed start must not become eligible again
		// after the only interrupt has already been acknowledged.
		return exhaust(claim, reasonAttemptCancelling)
	}
	if row.Record == lifecycle.RecordAuthorityUse && attempt.State != lifecycle.AttemptExecuting && attempt.State != lifecycle.AttemptWaitingInput {
		return exhaust(claim, "attempt_not_active")
	}
	if now.After(body.Deadline) {
		return exhaust(claim, reasonDeadlineExceeded)
	}
	harness, err := store.GetHarnessForUpdate(ctx, uow, tenant, operation.Provider)
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
	if row.Record == lifecycle.RecordOperation {
		validationDelivery, err := ids.New[ids.Delivery]()
		if err != nil {
			return nil, err
		}
		if err := (contracts.Request{Operation: operation.ID, Delivery: validationDelivery, Tenant: tenant, Attempt: operation.Attempt,
			Role: body.Role, Method: body.Method, Arguments: body.Arguments, Demand: row.Demand, Deadline: body.Deadline}).Validate(); err != nil {
			return exhaust(claim, reasonInvalidBody)
		}
		// Submission already records OperationDispatched when enqueueing.
		// This static outbox marker distinguishes an actual send attempt from
		// earlier yielded claims without changing the public lifecycle/schema.
		if _, err := uow.Exec(ctx, `update outbox set last_error=$3 where tenant_id=$1 and outbox_id=$2`, tenant.String(), row.ID.String(), reasonSendStarted); err != nil {
			return nil, err
		}
	}
	if err := store.CheckOutboxClaim(ctx, uow, tenant, row.ID, claim.Epoch); err != nil {
		return nil, err
	}
	if err := store.CheckAttemptLease(ctx, uow, tenant, operation.Attempt, lease); err != nil {
		return nil, err
	}
	if !time.Now().Before(body.Deadline) {
		return exhaust(claim, reasonDeadlineExceeded)
	}
	if err := commit(); err != nil {
		return nil, err
	}
	w.stats.Claims.Add(1)
	return &preparedDispatch{claim: claim, operation: operation, lease: lease}, nil
}

// budgetSpent reports whether the row already burned its retry budget, and
// the reason to record if so. The budget counts sends while Classify only
// runs after a failure, so the tick must check before sending: otherwise a
// rescheduled row with a spent budget would send forever.
func (w *Worker) budgetSpent(row store.OutboxRow) (string, bool) {
	if row.Attempts < w.cfg.Policy.MaxAttempts {
		return "", false
	}
	if row.LastError != "" {
		return row.LastError, true
	}
	return reasonRetryBudgetSpent, true
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
		// A hold that originally fit can lose its coverage when another
		// Attempt reports late usage. Equality remains valid: an existing
		// reservation may legitimately fill the ceiling exactly.
		if budget.Consumed+budget.Reserved > budget.Ceiling {
			return &exhaustedError{reason: reasonBudgetExhausted}
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
// uncertain for the reconciler. A pending or invalid reply also proves no
// safe-to-repeat outcome and therefore enters uncertainty.
func (w *Worker) resolveSend(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, delivery ids.DeliveryID, reply contracts.Reply, now time.Time) error {
	if reply.Operation != prepared.operation.ID || reply.Delivery != delivery {
		reply = uncertainReply(prepared.operation.ID, delivery)
	}
	if err := reply.Validate(); err != nil {
		reply = uncertainReply(prepared.operation.ID, delivery)
	}
	if reply.Durability == lifecycle.SendPending {
		reply = uncertainReply(prepared.operation.ID, delivery)
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
		if err := store.LockDispatchResult(ctx, uow, tenant, row.ID); err != nil {
			return err
		}
		if _, _, err := store.LockResultScope(ctx, uow, tenant, prepared.operation.Attempt); err != nil {
			return err
		}
		now = time.Now().UTC()
		h, err := store.GetHarnessForUpdate(ctx, uow, tenant, prepared.operation.Provider)
		if err != nil {
			return err
		}
		if !h.Mode.Authoritative() {
			return nil
		}
		if err := store.CheckAttemptLease(ctx, uow, tenant, prepared.operation.Attempt, prepared.lease); err != nil {
			return err
		}
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
		resolved, _, err := store.ApplyOperation(ctx, uow, tenant, prepared.operation.ID, event, target, tr)
		if err != nil {
			if errors.Is(err, store.ErrStaleVersion) {
				return w.releaseCommitted(ctx, uow, tenant, row, prepared, now, &committed)
			}
			return err
		}
		// The Attempt follows in the same commit: a rejection fails it and
		// its Task, and an unknown outcome stops it for reconciliation; it is
		// never treated as no effect.
		step := progress.Step{UOW: uow, Tenant: tenant, Actor: SystemActor(), At: now}
		if err := step.FollowOperation(ctx, resolved); err != nil {
			return err
		}
		if reply.Durability == lifecycle.SendUnknown {
			// The plane can no longer know the wait is real: the attempt
			// moved to reconciliation, and its open native request
			// supersedes — it never resurrects on a guess.
			if err := step.SupersedeOpenAttention(ctx, prepared.operation.Attempt); err != nil {
				return err
			}
			w.logger.Info("operation entered reconciliation", "operation", prepared.operation.ID.String())
		}
		if err := store.AckOutboxRow(ctx, uow, tenant, row.ID, prepared.claim.Epoch, time.Now().UTC()); err != nil {
			return err
		}
		if err := store.CheckAttemptLease(ctx, uow, tenant, prepared.operation.Attempt, prepared.lease); err != nil {
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

// exhaustPrepared exhausts a claimed dispatch row and drops its lease. sent
// reports whether this claim delivered a request.
func (w *Worker) exhaustPrepared(ctx context.Context, tenant ids.TenantID, row store.OutboxRow, prepared *preparedDispatch, now time.Time, reason string, sent bool) error {
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
	if err := w.abandonOperation(ctx, uow, tenant, prepared.operation.ID, !sent && prepared.claim.Epoch == 1, now); err != nil {
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

// abandonOperation resolves the operation of a dispatch the worker gave up
// on, in the exhausting transaction, so its Attempt and Task never stay live
// with nothing in flight. neverSent holds only when this is the row's first
// claim and no request left in it: the operation is rejected, which fails the
// Attempt and the Task. Otherwise an earlier claim may have delivered it (a
// transport failure, a crash before the reply was stored, a pending reply),
// so the effect is unknown and the operation goes to reconciliation; it is
// never treated as no effect. Only a dispatched operation is resolved: any
// other state already says what happened to it. An unresolved wait follows
// the outcome: uncertainty supersedes the open native request, and a
// never-sent rejection reconciles the stops of an Attempt already awaiting
// them — nothing ran, so nothing else will ever report a stop.
func (w *Worker) abandonOperation(ctx context.Context, uow *store.UnitOfWork, tenant ids.TenantID, id ids.OperationID, neverSent bool, now time.Time) error {
	operation, err := store.GetOperation(ctx, uow, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if operation.State != lifecycle.OperationDispatched {
		return nil
	}
	if _, _, err = store.LockResultScope(ctx, uow, tenant, operation.Attempt); err != nil {
		return err
	}
	event, target := lifecycle.OperationSendUnknown, lifecycle.OperationUncertain
	if neverSent {
		event, target = lifecycle.OperationSendRejected, lifecycle.OperationRejected
	}
	tr, err := w.transition(operation.Version, now)
	if err != nil {
		return err
	}
	resolved, _, err := store.ApplyOperation(ctx, uow, tenant, operation.ID, event, target, tr)
	if err != nil {
		return err
	}
	step := progress.Step{UOW: uow, Tenant: tenant, Actor: SystemActor(), At: now}
	if err := step.FollowOperation(ctx, resolved); err != nil {
		return err
	}
	if target == lifecycle.OperationUncertain {
		// The give-up mirror of a lost reply: the wait supersedes with the
		// operation's uncertainty.
		if err := step.SupersedeOpenAttention(ctx, operation.Attempt); err != nil {
			return err
		}
		return nil
	}
	return w.stopNeverSentCancellation(ctx, step, tenant, operation.Attempt)
}

// stopNeverSentCancellation reconciles the stops of an Attempt awaiting them
// when its operation was never delivered: no request reached the provider, so
// the runtime stop is definite, and with no environment ever bound the
// environment stop is too. The Attempt terminates without inventing a
// provider, releasing its reservation in the same commit.
func (w *Worker) stopNeverSentCancellation(ctx context.Context, step progress.Step, tenant ids.TenantID, attemptID ids.AttemptID) error {
	attempt, err := store.GetAttempt(ctx, step.UOW, tenant, attemptID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if attempt.State != lifecycle.AttemptCancellationRequested {
		return nil
	}
	if !attempt.RuntimeStopped {
		if _, applied, _, err := step.Attempt(ctx, attemptID, lifecycle.AttemptRuntimeStopped, ""); err != nil {
			return err
		} else if !applied {
			return nil
		}
	}
	binding, err := store.HasEnvironmentBinding(ctx, step.UOW, tenant, attemptID)
	if err != nil {
		return err
	}
	if binding {
		return nil
	}
	_, _, _, err = step.Attempt(ctx, attemptID, lifecycle.AttemptEnvironmentStopped, "")
	return err
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
