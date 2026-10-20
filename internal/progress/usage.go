package progress

import (
	"context"
	"errors"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Usage accounts for a validated runtime entry inside the caller's scope and
// cursor transaction. Late totals remain chargeable after an Attempt ends.
// A refusal is safe to acknowledge, but the caller must record its evidence.
func (s Step) Usage(ctx context.Context, attemptID ids.AttemptID, entry contracts.Entry) (bool, *lifecycle.TransitionError, error) {
	attempt, err := store.GetAttempt(ctx, s.UOW, s.Tenant, attemptID)
	if err != nil {
		return false, nil, err
	}
	refuse := func(code lifecycle.RejectionCode) (bool, *lifecycle.TransitionError, error) {
		return false, &lifecycle.TransitionError{Code: code, Change: lifecycle.Change{
			Tenant: s.Tenant, Record: lifecycle.RecordAttempt, RecordID: attempt.ID.Untyped(),
			Event: entry.Kind, From: string(attempt.State), Version: attempt.Version,
		}}, nil
	}
	payload, err := contracts.DecodeEntry[contracts.UsageReported](entry)
	if err != nil {
		return refuse(lifecycle.RejectInvalidTransition)
	}
	// Match the accounting lock order: budget before reservation.
	budget, err := store.GetBudgetForUpdateByTaskUnit(ctx, s.UOW, s.Tenant, attempt.Task, payload.Unit)
	if errors.Is(err, store.ErrNotFound) {
		return refuse(lifecycle.RejectWrongRecord)
	}
	if err != nil {
		return false, nil, err
	}
	reservationID, err := ids.Parse[ids.Reservation](attempt.ID.String())
	if err != nil {
		return false, nil, err
	}
	reservation, err := store.GetReservationForUpdate(ctx, s.UOW, s.Tenant, reservationID)
	if errors.Is(err, store.ErrNotFound) {
		return refuse(lifecycle.RejectWrongRecord)
	}
	if err != nil {
		return false, nil, err
	}
	if reservation.Attempt != attempt.ID || reservation.Budget != budget.ID {
		return refuse(lifecycle.RejectWrongRecord)
	}
	if payload.Cumulative <= reservation.Consumed {
		return false, nil, nil
	}
	if _, _, _, err := budget.ReportUsage(reservation, payload.Cumulative, budget.Version, s.At); err != nil {
		return refuse(lifecycle.RejectInvalidTransition)
	}
	tr, err := s.transition(budget.Version)
	if err != nil {
		return false, nil, err
	}
	_, _, _, err = store.ReportUsage(ctx, s.UOW, s.Tenant, budget.ID, reservation.ID, payload.Cumulative, tr)
	return err == nil, nil, err
}
