// Package postgres Budget repositories persist budgets, reservations and spend challenges.
// Reserve, consume and release each commit the budget row, the reservation
// row and every returned change's history atomically; spend assertion commits
// the challenge, its receipt and history atomically.
package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// GetBudget loads a budget by tenant and id.
func GetBudget(ctx context.Context, q Querier, tenant ids.TenantID, id ids.BudgetID) (lifecycle.Budget, error) {
	var budget lifecycle.Budget
	var unit, idText, tenantText, taskText string
	err := q.QueryRow(ctx, `
		select budget_id::text, tenant_id::text, task_id::text, unit, ceiling, reserved, consumed, overshoot, version, updated_at
		from budget where tenant_id = $1 and budget_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &unit, &budget.Ceiling, &budget.Reserved,
		&budget.Consumed, &budget.Overshoot, &budget.Version, &budget.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Budget{}, ErrNotFound
		}
		return lifecycle.Budget{}, err
	}
	var parseErr error
	if budget.ID, parseErr = parseID[ids.Budget](`budget.budget_id`, idText); parseErr != nil {
		return lifecycle.Budget{}, parseErr
	}
	if budget.Tenant, parseErr = parseID[ids.Tenant](`budget.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Budget{}, parseErr
	}
	if budget.Task, parseErr = parseID[ids.Task](`budget.task_id`, taskText); parseErr != nil {
		return lifecycle.Budget{}, parseErr
	}
	budget.Unit = lifecycle.Unit(unit)
	if err := budget.Validate(); err != nil {
		return lifecycle.Budget{}, err
	}
	return budget, nil
}

// InsertBudget persists a new budget with replay comparison on conflict.
func InsertBudget(ctx context.Context, u *UnitOfWork, budget lifecycle.Budget) (lifecycle.Budget, bool, error) {
	if err := budget.Validate(); err != nil {
		return lifecycle.Budget{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into budget (tenant_id, budget_id, task_id, unit, ceiling, reserved, consumed, overshoot, version, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		budget.Tenant.String(), budget.ID.String(), budget.Task.String(), string(budget.Unit),
		int64(budget.Ceiling), int64(budget.Reserved), int64(budget.Consumed),
		int64(budget.Overshoot), int64(budget.Version), budget.UpdatedAt,
	)
	if err != nil {
		return lifecycle.Budget{}, false, err
	}
	if inserted {
		return budget, false, nil
	}
	stored, loadErr := GetBudget(ctx, u, budget.Tenant, budget.ID)
	if isNotFound(loadErr) {
		return lifecycle.Budget{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.Budget{}, false, loadErr
	}
	if !budgetsEqual(stored, budget) {
		return lifecycle.Budget{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// budgetsEqual compares the domain payload of two budgets. UpdatedAt is a
// server stamp, not command payload: a replayed insert carries a fresh clock
// but the same counts, ceiling and version.
func budgetsEqual(a, b lifecycle.Budget) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Task == b.Task && a.Unit == b.Unit &&
		a.Ceiling == b.Ceiling && a.Reserved == b.Reserved && a.Consumed == b.Consumed &&
		a.Overshoot == b.Overshoot && a.Version == b.Version
}

// updateBudgetRow writes a budget with a version predicate.
func updateBudgetRow(ctx context.Context, u *UnitOfWork, budget lifecycle.Budget, expectedVersion uint64) error {
	return execVersionedUpdate(ctx, u, `
		update budget set reserved = $3, consumed = $4, overshoot = $5, version = $6, updated_at = $7
		where tenant_id = $1 and budget_id = $2 and version = $8`,
		budget.Tenant.String(), budget.ID.String(),
		int64(budget.Reserved), int64(budget.Consumed), int64(budget.Overshoot),
		int64(budget.Version), budget.UpdatedAt, int64(expectedVersion),
	)
}

// updateReservationRow writes a reservation with a version predicate.
func updateReservationRow(ctx context.Context, u *UnitOfWork, reservation lifecycle.Reservation, expectedVersion uint64) error {
	return execVersionedUpdate(ctx, u, `
		update reservation set consumed = $3, state = $4, version = $5, updated_at = $6
		where tenant_id = $1 and reservation_id = $2 and version = $7`,
		reservation.Tenant.String(), reservation.ID.String(),
		int64(reservation.Consumed), string(reservation.State),
		int64(reservation.Version), reservation.UpdatedAt, int64(expectedVersion),
	)
}

// secondAuditID mints the audit identity for a second change in one unit of
// work. Audit ids only need uniqueness, never stability.
func secondAuditID() (ids.AuditID, error) {
	return ids.New[ids.Audit]()
}

// ReservationRequest holds an amount for an attempt: the budget update, the
// reservation insert and history commit together.
type ReservationRequest struct {
	Tenant      ids.TenantID
	Budget      ids.BudgetID
	Reservation ids.ReservationID
	Attempt     ids.AttemptID
	Amount      uint64
}

// ReserveBudget holds an amount for an attempt: budget update, reservation
// insert and history commit together. Rejections persist refusal evidence.
func ReserveBudget(ctx context.Context, u *UnitOfWork, req ReservationRequest, tr Transition) (lifecycle.Budget, lifecycle.Reservation, bool, error) {
	budget, err := GetBudgetForUpdate(ctx, u, req.Tenant, req.Budget)
	if err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	next, reservation, change, reserveErr := budget.Reserve(req.Reservation, req.Attempt, req.Amount, tr.ExpectedVersion, tr.At)
	if reserveErr != nil {
		if terr, ok := reserveErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Budget{}, lifecycle.Reservation{}, false, histErr
			}
		}
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, reserveErr
	}
	if err := updateBudgetRow(ctx, u, next, tr.ExpectedVersion); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	if _, _, err := InsertReservation(ctx, u, reservation); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, tr.At)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	return next, reservation, false, nil
}

// GetBudgetForUpdate locks a budget row for the caller's read-modify-write.
func GetBudgetForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.BudgetID) (lifecycle.Budget, error) {
	if err := lockRow(ctx, u, "budget", "budget_id", tenant.String(), id.String()); err != nil {
		return lifecycle.Budget{}, err
	}
	return GetBudget(ctx, u, tenant, id)
}

// ReportUsage applies cumulative provider usage to a reservation: budget and
// reservation updates plus both changes' history commit together.
func ReportUsage(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, budgetID ids.BudgetID, reservationID ids.ReservationID, cumulative uint64, tr Transition) (lifecycle.Budget, lifecycle.Reservation, bool, error) {
	budget, err := GetBudgetForUpdate(ctx, u, tenant, budgetID)
	if err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	reservation, err := GetReservationForUpdate(ctx, u, tenant, reservationID)
	if err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	nextBudget, nextReservation, changes, usageErr := budget.ReportUsage(reservation, cumulative, tr.ExpectedVersion, tr.At)
	if usageErr != nil {
		if terr, ok := usageErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Budget{}, lifecycle.Reservation{}, false, histErr
			}
		}
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, usageErr
	}
	if len(changes) == 0 {
		return nextBudget, nextReservation, true, nil
	}
	if err := updateBudgetRow(ctx, u, nextBudget, tr.ExpectedVersion); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	if err := updateReservationRow(ctx, u, nextReservation, reservation.Version); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
	}
	for i, change := range changes {
		auditID := tr.AuditID
		if i > 0 {
			var idErr error
			if auditID, idErr = secondAuditID(); idErr != nil {
				return lifecycle.Budget{}, lifecycle.Reservation{}, false, idErr
			}
		}
		audit := change.Audit(auditID, tr.Actor, tr.At)
		var payload *OutboxPayload
		if i == 0 {
			payload = tr.Outbox
		}
		if err := appendHistory(ctx, u, change, audit, payload); err != nil {
			return lifecycle.Budget{}, lifecycle.Reservation{}, false, err
		}
	}
	return nextBudget, nextReservation, false, nil
}

// ReleaseBudget returns a reservation's unused hold exactly once.
func ReleaseBudget(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, budgetID ids.BudgetID, reservationID ids.ReservationID, tr Transition) (lifecycle.Budget, lifecycle.Reservation, error) {
	budget, err := GetBudgetForUpdate(ctx, u, tenant, budgetID)
	if err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, err
	}
	reservation, err := GetReservationForUpdate(ctx, u, tenant, reservationID)
	if err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, err
	}
	nextBudget, nextReservation, changes, releaseErr := budget.Release(reservation, tr.ExpectedVersion, tr.At)
	if releaseErr != nil {
		if terr, ok := releaseErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, tr.At)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Budget{}, lifecycle.Reservation{}, histErr
			}
		}
		return lifecycle.Budget{}, lifecycle.Reservation{}, releaseErr
	}
	if err := updateBudgetRow(ctx, u, nextBudget, tr.ExpectedVersion); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, err
	}
	if err := updateReservationRow(ctx, u, nextReservation, reservation.Version); err != nil {
		return lifecycle.Budget{}, lifecycle.Reservation{}, err
	}
	for i, change := range changes {
		auditID := tr.AuditID
		if i > 0 {
			var idErr error
			if auditID, idErr = secondAuditID(); idErr != nil {
				return lifecycle.Budget{}, lifecycle.Reservation{}, idErr
			}
		}
		audit := change.Audit(auditID, tr.Actor, tr.At)
		var payload *OutboxPayload
		if i == 0 {
			payload = tr.Outbox
		}
		if err := appendHistory(ctx, u, change, audit, payload); err != nil {
			return lifecycle.Budget{}, lifecycle.Reservation{}, err
		}
	}
	return nextBudget, nextReservation, nil
}

// GetReservation loads a reservation by tenant and id.
func GetReservation(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ReservationID) (lifecycle.Reservation, error) {
	var reservation lifecycle.Reservation
	var state, idText, tenantText, budgetText, attemptText string
	err := q.QueryRow(ctx, `
		select reservation_id::text, tenant_id::text, budget_id::text, attempt_id::text, amount, consumed, state, version, created_at, updated_at
		from reservation where tenant_id = $1 and reservation_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &budgetText, &attemptText, &reservation.Amount, &reservation.Consumed,
		&state, &reservation.Version, &reservation.CreatedAt, &reservation.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Reservation{}, ErrNotFound
		}
		return lifecycle.Reservation{}, err
	}
	var parseErr error
	if reservation.ID, parseErr = parseID[ids.Reservation](`reservation.reservation_id`, idText); parseErr != nil {
		return lifecycle.Reservation{}, parseErr
	}
	if reservation.Tenant, parseErr = parseID[ids.Tenant](`reservation.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Reservation{}, parseErr
	}
	if reservation.Budget, parseErr = parseID[ids.Budget](`reservation.budget_id`, budgetText); parseErr != nil {
		return lifecycle.Reservation{}, parseErr
	}
	if reservation.Attempt, parseErr = parseID[ids.Attempt](`reservation.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.Reservation{}, parseErr
	}
	reservation.State = lifecycle.ReservationState(state)
	if err := reservation.Validate(); err != nil {
		return lifecycle.Reservation{}, err
	}
	return reservation, nil
}

// GetReservationForUpdate locks a reservation row for read-modify-write.
func GetReservationForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.ReservationID) (lifecycle.Reservation, error) {
	if err := lockRow(ctx, u, "reservation", "reservation_id", tenant.String(), id.String()); err != nil {
		return lifecycle.Reservation{}, err
	}
	return GetReservation(ctx, u, tenant, id)
}

// InsertReservation persists a reservation created by ReserveBudget with
// replay comparison on conflict.
func InsertReservation(ctx context.Context, u *UnitOfWork, reservation lifecycle.Reservation) (lifecycle.Reservation, bool, error) {
	if err := reservation.Validate(); err != nil {
		return lifecycle.Reservation{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into reservation (tenant_id, reservation_id, budget_id, attempt_id, amount, consumed, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		reservation.Tenant.String(), reservation.ID.String(), reservation.Budget.String(), reservation.Attempt.String(),
		int64(reservation.Amount), int64(reservation.Consumed), string(reservation.State),
		int64(reservation.Version), reservation.CreatedAt, reservation.UpdatedAt,
	)
	if err != nil {
		return lifecycle.Reservation{}, false, err
	}
	if inserted {
		return reservation, false, nil
	}
	stored, loadErr := GetReservation(ctx, u, reservation.Tenant, reservation.ID)
	if isNotFound(loadErr) {
		return lifecycle.Reservation{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.Reservation{}, false, loadErr
	}
	if !reservationsEqual(stored, reservation) {
		return lifecycle.Reservation{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// reservationsEqual compares the domain payload of two reservations,
// excluding server stamps for the same reason as budgetsEqual.
func reservationsEqual(a, b lifecycle.Reservation) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Budget == b.Budget && a.Attempt == b.Attempt &&
		a.Amount == b.Amount && a.Consumed == b.Consumed && a.State == b.State && a.Version == b.Version
}

// InsertSpendChallenge persists a new spend challenge with replay comparison.
func InsertSpendChallenge(ctx context.Context, u *UnitOfWork, challenge lifecycle.SpendChallenge) (lifecycle.SpendChallenge, bool, error) {
	if err := challenge.Validate(); err != nil {
		return lifecycle.SpendChallenge{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into spend_challenge (tenant_id, spend_id, principal_id, task_id, attempt_id, operation_id, amount, unit, nonce, state, version, expires_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		challenge.Terms.Tenant.String(), challenge.ID.String(), challenge.Terms.Principal.String(),
		challenge.Terms.Task.String(), challenge.Terms.Attempt.String(), challenge.Terms.Operation.String(),
		int64(challenge.Terms.Amount), string(challenge.Terms.Unit), string(challenge.Terms.Nonce),
		string(challenge.State), int64(challenge.Version), challenge.ExpiresAt, challenge.CreatedAt, challenge.UpdatedAt,
	)
	if err != nil {
		return lifecycle.SpendChallenge{}, false, err
	}
	if inserted {
		return challenge, false, nil
	}
	stored, loadErr := GetSpendChallenge(ctx, u, challenge.Terms.Tenant, challenge.ID)
	if isNotFound(loadErr) {
		return lifecycle.SpendChallenge{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.SpendChallenge{}, false, loadErr
	}
	if !spendChallengesEqual(stored, challenge) {
		return lifecycle.SpendChallenge{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetSpendChallenge loads a spend challenge by tenant and id.
func GetSpendChallenge(ctx context.Context, q Querier, tenant ids.TenantID, id ids.SpendID) (lifecycle.SpendChallenge, error) {
	var challenge lifecycle.SpendChallenge
	var state, idText, principalText, taskText, attemptText, operationText, unitText, nonceText string
	var amount, version int64
	err := q.QueryRow(ctx, `
		select spend_id::text, principal_id::text, task_id::text, attempt_id::text, operation_id::text,
			amount, unit::text, nonce, state, version, expires_at, created_at, updated_at
		from spend_challenge where tenant_id = $1 and spend_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &principalText, &taskText, &attemptText, &operationText,
		&amount, &unitText, &nonceText, &state, &version, &challenge.ExpiresAt, &challenge.CreatedAt, &challenge.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.SpendChallenge{}, ErrNotFound
		}
		return lifecycle.SpendChallenge{}, err
	}
	var parseErr error
	if challenge.ID, parseErr = parseID[ids.Spend](`spend_challenge.spend_id`, idText); parseErr != nil {
		return lifecycle.SpendChallenge{}, parseErr
	}
	terms := &challenge.Terms
	terms.Tenant = tenant
	if terms.Principal, parseErr = parseID[ids.Principal](`spend_challenge.principal_id`, principalText); parseErr != nil {
		return lifecycle.SpendChallenge{}, parseErr
	}
	if terms.Task, parseErr = parseID[ids.Task](`spend_challenge.task_id`, taskText); parseErr != nil {
		return lifecycle.SpendChallenge{}, parseErr
	}
	if terms.Attempt, parseErr = parseID[ids.Attempt](`spend_challenge.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.SpendChallenge{}, parseErr
	}
	if terms.Operation, parseErr = parseID[ids.Operation](`spend_challenge.operation_id`, operationText); parseErr != nil {
		return lifecycle.SpendChallenge{}, parseErr
	}
	terms.Amount, terms.Unit = uint64(amount), lifecycle.Unit(unitText)
	terms.Nonce = identityDigest(nonceText)
	challenge.State = lifecycle.SpendState(state)
	challenge.Version = uint64(version)
	if err := challenge.Validate(); err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	return challenge, nil
}

func spendChallengesEqual(a, b lifecycle.SpendChallenge) bool {
	return a.ID == b.ID && a.Terms == b.Terms && a.State == b.State && a.Version == b.Version &&
		sameInstant(a.ExpiresAt, b.ExpiresAt) && sameInstant(a.CreatedAt, b.CreatedAt) && sameInstant(a.UpdatedAt, b.UpdatedAt)
}

// AssertSpendChallenge completes a challenge with a matching assertion: the
// challenge update, the receipt insert and history commit together. An
// already-asserted challenge replays identical assertions and mismatches
// anything else.
func AssertSpendChallenge(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.SpendID, assertion lifecycle.SpendAssertion, expectedVersion uint64, tr Transition) (lifecycle.SpendChallenge, lifecycle.SpendReceipt, bool, error) {
	challenge, err := GetSpendChallengeForUpdate(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, err
	}
	if challenge.State == lifecycle.SpendAsserted {
		receipt, loadErr := GetSpendReceipt(ctx, u, tenant, id)
		if loadErr != nil {
			return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, loadErr
		}
		if receipt.Terms != assertion.Terms || receipt.Proof != assertion.Proof {
			return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, ErrPayloadMismatch
		}
		return challenge, receipt, true, nil
	}
	next, receipt, change, assertErr := challenge.Assert(assertion, expectedVersion)
	if assertErr != nil {
		if terr, ok := assertErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, assertion.AssertedAt)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, histErr
			}
		}
		return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, assertErr
	}
	if err := execVersionedUpdate(ctx, u, `
		update spend_challenge set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and spend_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version),
		next.UpdatedAt, int64(expectedVersion),
	); err != nil {
		return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, err
	}
	if _, _, err := InsertSpendReceipt(ctx, u, receipt); err != nil {
		return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, assertion.AssertedAt)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.SpendChallenge{}, lifecycle.SpendReceipt{}, false, err
	}
	return next, receipt, false, nil
}

// GetSpendChallengeForUpdate locks a challenge row for read-modify-write.
func GetSpendChallengeForUpdate(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.SpendID) (lifecycle.SpendChallenge, error) {
	if err := lockRow(ctx, u, "spend_challenge", "spend_id", tenant.String(), id.String()); err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	return GetSpendChallenge(ctx, u, tenant, id)
}

// ExpireSpendChallenge closes an unasserted challenge after expiry.
func ExpireSpendChallenge(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, id ids.SpendID, expectedVersion uint64, at time.Time, tr Transition) (lifecycle.SpendChallenge, error) {
	challenge, err := GetSpendChallengeForUpdate(ctx, u, tenant, id)
	if err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	next, change, expireErr := challenge.Expire(expectedVersion, at)
	if expireErr != nil {
		if terr, ok := expireErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, at)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.SpendChallenge{}, histErr
			}
		}
		return lifecycle.SpendChallenge{}, expireErr
	}
	if err := execVersionedUpdate(ctx, u, `
		update spend_challenge set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and spend_id = $2 and version = $6`,
		tenant.String(), id.String(), string(next.State), int64(next.Version), next.UpdatedAt, int64(expectedVersion),
	); err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, at)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	return next, nil
}

// InsertSpendReceipt persists a spend receipt. Receipts are immutable: a
// primary-key conflict compares for replay.
func InsertSpendReceipt(ctx context.Context, u *UnitOfWork, receipt lifecycle.SpendReceipt) (lifecycle.SpendReceipt, bool, error) {
	if err := receipt.Validate(); err != nil {
		return lifecycle.SpendReceipt{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into spend_receipt (tenant_id, spend_id, principal_id, task_id, attempt_id, operation_id, amount, unit, nonce, proof, issued_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		receipt.Terms.Tenant.String(), receipt.Challenge.String(), receipt.Terms.Principal.String(),
		receipt.Terms.Task.String(), receipt.Terms.Attempt.String(), receipt.Terms.Operation.String(),
		int64(receipt.Terms.Amount), string(receipt.Terms.Unit), string(receipt.Terms.Nonce),
		string(receipt.Proof), receipt.IssuedAt,
	)
	if err != nil {
		return lifecycle.SpendReceipt{}, false, err
	}
	if inserted {
		return receipt, false, nil
	}
	stored, loadErr := GetSpendReceipt(ctx, u, receipt.Terms.Tenant, receipt.Challenge)
	if isNotFound(loadErr) {
		return lifecycle.SpendReceipt{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.SpendReceipt{}, false, loadErr
	}
	if stored.Terms != receipt.Terms || stored.Proof != receipt.Proof || !sameInstant(stored.IssuedAt, receipt.IssuedAt) {
		return lifecycle.SpendReceipt{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetSpendReceipt loads a spend receipt by tenant and challenge.
func GetSpendReceipt(ctx context.Context, q Querier, tenant ids.TenantID, id ids.SpendID) (lifecycle.SpendReceipt, error) {
	var receipt lifecycle.SpendReceipt
	var principalText, taskText, attemptText, operationText, unitText, nonceText, proofText string
	var amount int64
	err := q.QueryRow(ctx, `
		select principal_id::text, task_id::text, attempt_id::text, operation_id::text, amount, unit::text, nonce, proof, issued_at
		from spend_receipt where tenant_id = $1 and spend_id = $2`,
		tenant.String(), id.String(),
	).Scan(&principalText, &taskText, &attemptText, &operationText, &amount, &unitText, &nonceText, &proofText, &receipt.IssuedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.SpendReceipt{}, ErrNotFound
		}
		return lifecycle.SpendReceipt{}, err
	}
	var parseErr error
	receipt.Challenge = id
	terms := &receipt.Terms
	terms.Tenant = tenant
	if terms.Principal, parseErr = parseID[ids.Principal](`spend_receipt.principal_id`, principalText); parseErr != nil {
		return lifecycle.SpendReceipt{}, parseErr
	}
	if terms.Task, parseErr = parseID[ids.Task](`spend_receipt.task_id`, taskText); parseErr != nil {
		return lifecycle.SpendReceipt{}, parseErr
	}
	if terms.Attempt, parseErr = parseID[ids.Attempt](`spend_receipt.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.SpendReceipt{}, parseErr
	}
	if terms.Operation, parseErr = parseID[ids.Operation](`spend_receipt.operation_id`, operationText); parseErr != nil {
		return lifecycle.SpendReceipt{}, parseErr
	}
	terms.Amount, terms.Unit = uint64(amount), lifecycle.Unit(unitText)
	terms.Nonce = identityDigest(nonceText)
	receipt.Proof = identityDigest(proofText)
	if err := receipt.Validate(); err != nil {
		return lifecycle.SpendReceipt{}, err
	}
	return receipt, nil
}
