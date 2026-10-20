// Package postgres Outbox dispatch helpers own the claim lifecycle the
// dispatch loop drives: exactly one worker holds a row while it is
// 'processing', and the lease epoch fences a stale holder's ack or retry.
// All mutations run inside the caller's UnitOfWork.
package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// OutboxClaim is one claimed dispatch row: the row plus the lease epoch the
// claim granted. Mutations of the row must present the epoch; a mismatch
// means another worker took the row first.
type OutboxClaim struct {
	Row   OutboxRow
	Epoch int64
	Owner string
}

// dispatchableRows matches the outbox rows the dispatch loop owns: provider
// operation dispatches, attempt cancellation intents and decided attention
// deliveries. It mirrors the prototype's command-only outbox (StartAttempt,
// CancelAttempt, ...): every other row belongs to future surfaces and stays
// pending. The lifecycle names are lifecycle.RecordOperation,
// lifecycle.RecordAttempt, lifecycle.AttemptRequestCancel and
// lifecycle.RecordAttention with lifecycle.AttentionDecide; SQL spells them
// out because placeholders cannot carry identifiers.
const dispatchableRows = `((record_kind = 'provider_operation') or (record_kind = 'attempt' and event = 'request_cancel') or (record_kind = 'attention_request' and event = 'decide') or (record_kind='authority_use' and event='authorize') or (record_kind='artifact' and event='stage'))`

// LockDispatchResult keeps resolution in the same Outbox -> Attempt -> Task
// lock order as claim preparation. Mutation predicates still fence epochs.
func LockDispatchResult(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID) error {
	return lockRow(ctx, u, "outbox", "outbox_id", tenant.String(), id.String())
}

// CheckOutboxClaim validates a still-locked claim after preparation lock waits.
func CheckOutboxClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64) error {
	var current bool
	if err := u.QueryRow(ctx, `select exists(select 1 from outbox where tenant_id=$1 and outbox_id=$2 and status='processing' and lease_epoch=$3 and lease_expires_at>clock_timestamp())`, tenant.String(), id.String(), epoch).Scan(&current); err != nil {
		return err
	}
	if !current {
		return ErrStaleVersion
	}
	return nil
}

// RenewArtifactClaim renews only a still-live, exactly owned claim.
func RenewArtifactClaim(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64, ttl time.Duration, now time.Time) error {
	return execVersionedUpdate(ctx, u, `update outbox set lease_expires_at=$5,updated_at=$4 where tenant_id=$1 and outbox_id=$2 and status='processing' and lease_epoch=$3 and lease_expires_at>$4`, tenant.String(), id.String(), epoch, now, now.Add(ttl))
}

// ClaimOutboxRow takes the oldest due, unexhausted, dispatchable row for
// owner. The SELECT skips rows locked by a live worker; the UPDATE moves the
// row to 'processing' from 'pending' or from a lapsed 'processing' lease, so
// a crashed holder's row is reclaimable and a concurrent claim loses exactly
// once. Person-demand rows come first, mirroring Scheduler.Select. It
// reports claimed=false (and no error) when nothing is due.
func ClaimOutboxRow(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, owner string, ttl time.Duration, now time.Time) (OutboxClaim, bool, error) {
	var claim OutboxClaim
	var idText, recordText, recordIDText, eventText, demandText string
	var attemptText, lastErrText *string
	var payload []byte
	err := u.QueryRow(ctx, `
		update outbox set status = 'processing', claimed_at = $3, claimed_by = $4,
			lease_epoch = lease_epoch + 1, lease_expires_at = $5, updated_at = $3
		where (tenant_id, outbox_id) = (
			select tenant_id, outbox_id from outbox
			where tenant_id = $1 and exhausted_at is null and next_attempt_at <= $2
				and `+dispatchableRows+`
				and (status = 'pending'
					or (status = 'processing' and lease_expires_at is not null and lease_expires_at <= $3))
			order by case when demand = 'person' then 0 else 1 end, next_attempt_at
			limit 1 for update skip locked
		) and `+dispatchableRows+` and (status = 'pending'
			or (status = 'processing' and lease_expires_at is not null and lease_expires_at <= $3))
		returning outbox_id::text, attempt_id::text, record_kind, record_id::text, event, payload,
			demand, attempts, next_attempt_at, last_error, created_at, lease_epoch, claimed_by`,
		tenant.String(), now, now, owner, now.Add(ttl),
	).Scan(&idText, &attemptText, &recordText, &recordIDText, &eventText, &payload,
		&demandText, &claim.Row.Attempts, &claim.Row.NextTryAt, &lastErrText,
		&claim.Row.CreatedAt, &claim.Epoch, &claim.Owner)
	if err != nil {
		if isNoRows(err) {
			return OutboxClaim{}, false, nil
		}
		return OutboxClaim{}, false, err
	}
	var parseErr error
	if claim.Row.ID, parseErr = parseID[ids.Any](`outbox.outbox_id`, idText); parseErr != nil {
		return OutboxClaim{}, false, parseErr
	}
	if claim.Row.Attempt, parseErr = parseOptionalID[ids.Attempt](`outbox.attempt_id`, attemptText); parseErr != nil {
		return OutboxClaim{}, false, parseErr
	}
	claim.Row.Record = lifecycle.RecordKind(recordText)
	if claim.Row.RecordID, parseErr = parseID[ids.Any](`outbox.record_id`, recordIDText); parseErr != nil {
		return OutboxClaim{}, false, parseErr
	}
	claim.Row.Event, claim.Row.Payload = eventText, payload
	claim.Row.Demand = contracts.Demand(demandText)
	if lastErrText != nil {
		claim.Row.LastError = *lastErrText
	}
	return claim, true, nil
}

// ClaimOutboxRowByID takes one dispatchable row the caller selected, e.g.
// from OutboxReadyDispatch. The status predicate makes the take atomic: zero
// rows mean another worker took it first (or it stopped being dispatchable),
// reported as ErrStaleVersion so the caller skips it.
func ClaimOutboxRowByID(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, owner string, ttl time.Duration, now time.Time) (OutboxClaim, error) {
	var claim OutboxClaim
	var idText, recordText, recordIDText, eventText, demandText string
	var attemptText, lastErrText *string
	var payload []byte
	err := u.QueryRow(ctx, `
		update outbox set status = 'processing', claimed_at = $3, claimed_by = $4,
			lease_epoch = lease_epoch + 1, lease_expires_at = $5, updated_at = $3
		where tenant_id = $1 and outbox_id = $2 and exhausted_at is null
			and `+dispatchableRows+`
			and (status = 'pending'
				or (status = 'processing' and lease_expires_at is not null and lease_expires_at <= $3))
		returning outbox_id::text, attempt_id::text, record_kind, record_id::text, event, payload,
			demand, attempts, next_attempt_at, last_error, created_at, lease_epoch, claimed_by`,
		tenant.String(), id.String(), now, owner, now.Add(ttl),
	).Scan(&idText, &attemptText, &recordText, &recordIDText, &eventText, &payload,
		&demandText, &claim.Row.Attempts, &claim.Row.NextTryAt, &lastErrText,
		&claim.Row.CreatedAt, &claim.Epoch, &claim.Owner)
	if err != nil {
		if isNoRows(err) {
			return OutboxClaim{}, ErrStaleVersion
		}
		return OutboxClaim{}, err
	}
	var parseErr error
	if claim.Row.ID, parseErr = parseID[ids.Any](`outbox.outbox_id`, idText); parseErr != nil {
		return OutboxClaim{}, parseErr
	}
	if claim.Row.Attempt, parseErr = parseOptionalID[ids.Attempt](`outbox.attempt_id`, attemptText); parseErr != nil {
		return OutboxClaim{}, parseErr
	}
	claim.Row.Record = lifecycle.RecordKind(recordText)
	if claim.Row.RecordID, parseErr = parseID[ids.Any](`outbox.record_id`, recordIDText); parseErr != nil {
		return OutboxClaim{}, parseErr
	}
	claim.Row.Event, claim.Row.Payload = eventText, payload
	claim.Row.Demand = contracts.Demand(demandText)
	if lastErrText != nil {
		claim.Row.LastError = *lastErrText
	}
	return claim, nil
}

// ReleaseOutboxRow yields a held row back to pending without advancing its
// retry facts: no attempts bump, due immediately. The epoch predicate fences
// a holder whose claim moved on. Use it for no-fault yields (lease
// contention, lost races, no registered provider) so contention never burns
// the retry budget.
func ReleaseOutboxRow(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update outbox set status = 'pending', claimed_at = null, claimed_by = null,
			next_attempt_at = $4, updated_at = $4
		where tenant_id = $1 and outbox_id = $2 and status = 'processing' and lease_epoch = $3`,
		tenant.String(), id.String(), epoch, now,
	)
}

// AckOutboxRow marks a claimed row dispatched. The lease-epoch predicate
// fences a stale holder: zero rows mean the claim moved on without us.
func AckOutboxRow(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update outbox set status = 'done', updated_at = $5
		where tenant_id = $1 and outbox_id = $2 and status = 'processing' and lease_epoch = $3 and lease_expires_at > $4`,
		tenant.String(), id.String(), epoch, now, now,
	)
}

// ScheduleOutboxRetry releases a claimed row back to pending with its retry
// facts advanced: one more attempt, a not-before timestamp from the worker's
// backoff, and the last typed failure. The epoch predicate fences stale
// holders the same way AckOutboxRow does.
func ScheduleOutboxRetry(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64, nextTryAt time.Time, lastErr string, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update outbox set status = 'pending', claimed_at = null, claimed_by = null,
			attempts = attempts + 1, next_attempt_at = $4, last_error = $5, updated_at = $6
		where tenant_id = $1 and outbox_id = $2 and status = 'processing' and lease_epoch = $3`,
		tenant.String(), id.String(), epoch, nextTryAt, textOrNil(lastErr), now,
	)
}

// ExhaustOutboxRow stops a row from ever polling again while keeping it
// inspectable: exhausted_at marks the give-up, last_error the reason.
// The epoch predicate fences stale holders.
func ExhaustOutboxRow(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.AnyID, epoch int64, lastErr string, now time.Time) error {
	return execVersionedUpdate(ctx, u, `
		update outbox set exhausted_at = $4, last_error = $5, status = 'pending',
			claimed_at = null, claimed_by = null, updated_at = $4
		where tenant_id = $1 and outbox_id = $2 and status = 'processing' and lease_epoch = $3`,
		tenant.String(), id.String(), epoch, now, textOrNil(lastErr),
	)
}
