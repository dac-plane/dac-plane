package lifecycle

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// MaxAmount bounds every budget amount so that JSON consumers, which may hold
// numbers as IEEE-754 doubles, read them exactly.
const MaxAmount uint64 = 1<<53 - 1

var unitPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Unit names what a budget counts, for example "usd_micros" or "tokens".
// Amounts are non-negative integers in this unit; there is no conversion.
type Unit string

// Validate reports whether the unit is a short lowercase name.
func (u Unit) Validate() error {
	if !unitPattern.MatchString(string(u)) {
		return fmt.Errorf("%w: malformed budget unit", ErrInvalid)
	}
	return nil
}

// BudgetState is derived from a budget's counts: open while more can be
// reserved, exhausted once reserved and consumed amounts reach the ceiling.
type BudgetState string

const (
	BudgetOpen      BudgetState = "open"
	BudgetExhausted BudgetState = "exhausted"
)

// Budget is the authoritative spending limit of one Task in one unit.
// Reserved is the sum of outstanding holds and Consumed the sum of recorded
// usage. Overshoot is the largest amount by which in-flight or late usage has
// taken Reserved+Consumed past Ceiling; it is recorded, never hidden. Every
// change is a compare-and-set on Version, so competing reservations serialize.
type Budget struct {
	ID        ids.BudgetID `json:"budget_id"`
	Tenant    ids.TenantID `json:"tenant_id"`
	Task      ids.TaskID   `json:"task_id"`
	Unit      Unit         `json:"unit"`
	Ceiling   uint64       `json:"ceiling"`
	Reserved  uint64       `json:"reserved"`
	Consumed  uint64       `json:"consumed"`
	Overshoot uint64       `json:"overshoot"`
	Version   uint64       `json:"version"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// NewBudget returns an empty budget at version 1.
func NewBudget(id ids.BudgetID, tenant ids.TenantID, task ids.TaskID, unit Unit, ceiling uint64, at time.Time) (Budget, error) {
	budget := Budget{ID: id, Tenant: tenant, Task: task, Unit: unit, Ceiling: ceiling, Version: 1, UpdatedAt: at}
	return budget, budget.Validate()
}

// Validate reports whether the counts are within bounds and consistent.
func (b Budget) Validate() error {
	if b.ID.IsZero() || b.Tenant.IsZero() || b.Task.IsZero() || b.Version == 0 || b.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: budget requires identifiers, version and time", ErrInvalid)
	}
	if err := b.Unit.Validate(); err != nil {
		return err
	}
	if b.Ceiling == 0 || b.Ceiling > MaxAmount || b.Reserved > MaxAmount || b.Consumed > MaxAmount || b.Overshoot > MaxAmount {
		return fmt.Errorf("%w: budget amounts must be within bounds and the ceiling positive", ErrInvalid)
	}
	if b.Overshoot < b.excess() {
		return fmt.Errorf("%w: recorded overshoot is below the current excess", ErrInvalid)
	}
	return nil
}

// committed is what the budget has promised or spent. Each term is at most
// MaxAmount, so the sum cannot overflow.
func (b Budget) committed() uint64 {
	return b.Reserved + b.Consumed
}

func (b Budget) excess() uint64 {
	if committed := b.committed(); committed > b.Ceiling {
		return committed - b.Ceiling
	}
	return 0
}

// State reports whether more work may be reserved against the budget.
func (b Budget) State() BudgetState {
	if b.committed() >= b.Ceiling {
		return BudgetExhausted
	}
	return BudgetOpen
}

// ReservationState is whether a reservation still holds budget.
type ReservationState string

const (
	ReservationHeld     ReservationState = "held"
	ReservationReleased ReservationState = "released"
)

// ReservationStates lists every ReservationState.
func ReservationStates() []ReservationState {
	return []ReservationState{ReservationHeld, ReservationReleased}
}

// Reservation holds part of a budget for one Attempt before dispatch.
// Consumed is the cumulative usage reported against it, which may exceed
// Amount when usage runs past the estimate.
type Reservation struct {
	ID        ids.ReservationID `json:"reservation_id"`
	Tenant    ids.TenantID      `json:"tenant_id"`
	Budget    ids.BudgetID      `json:"budget_id"`
	Attempt   ids.AttemptID     `json:"attempt_id"`
	Amount    uint64            `json:"amount"`
	Consumed  uint64            `json:"consumed"`
	State     ReservationState  `json:"state"`
	Version   uint64            `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Validate reports whether the reservation is complete and within bounds.
func (r Reservation) Validate() error {
	if r.ID.IsZero() || r.Tenant.IsZero() || r.Budget.IsZero() || r.Attempt.IsZero() || r.Version == 0 {
		return fmt.Errorf("%w: reservation requires identifiers and a positive version", ErrInvalid)
	}
	if r.Amount == 0 || r.Amount > MaxAmount || r.Consumed > MaxAmount {
		return fmt.Errorf("%w: reservation amounts must be within bounds and the amount positive", ErrInvalid)
	}
	if !slices.Contains(ReservationStates(), r.State) {
		return fmt.Errorf("%w: unknown reservation state", ErrInvalid)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("%w: reservation timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// outstanding is the part of the hold not yet covered by usage.
func (r Reservation) outstanding() uint64 {
	if r.State == ReservationReleased || r.Consumed >= r.Amount {
		return 0
	}
	return r.Amount - r.Consumed
}

func (b Budget) change(event string) Change {
	return Change{
		Tenant: b.Tenant, Record: RecordBudget, RecordID: b.ID.Untyped(),
		Event: event, From: string(b.State()), Version: b.Version,
	}
}

func (r Reservation) change(event string) Change {
	return Change{
		Tenant: r.Tenant, Record: RecordReservation, RecordID: r.ID.Untyped(),
		Event: event, From: string(r.State), Version: r.Version,
	}
}

func (b Budget) bump(change Change, at time.Time) (Budget, Change) {
	b.Version++
	b.UpdatedAt = at
	change.To = string(b.State())
	change.Version = b.Version
	return b, change
}

func (r Reservation) bump(change Change, at time.Time) (Reservation, Change) {
	r.Version++
	r.UpdatedAt = at
	change.To = string(r.State)
	change.Version = r.Version
	return r, change
}

// Reserve holds amount for attempt if the budget is still at expectedVersion
// and the hold keeps Reserved+Consumed within the ceiling. A budget that has
// reached its ceiling accepts no further reservation, which stops dispatch.
func (b Budget) Reserve(id ids.ReservationID, attempt ids.AttemptID, amount, expectedVersion uint64, at time.Time) (Budget, Reservation, Change, error) {
	change := b.change("reserve")
	if id.IsZero() || attempt.IsZero() || amount == 0 || amount > MaxAmount || at.IsZero() {
		return b, Reservation{}, change, fmt.Errorf("%w: reservation requires identifiers, a bounded positive amount and a time", ErrInvalid)
	}
	switch {
	case b.Version != expectedVersion:
		return b, Reservation{}, change, reject(change, RejectVersionConflict)
	case b.committed()+amount > b.Ceiling:
		return b, Reservation{}, change, reject(change, RejectCeilingReached)
	}
	b.Reserved += amount
	b, change = b.bump(change, at)
	reservation := Reservation{
		ID: id, Tenant: b.Tenant, Budget: b.ID, Attempt: attempt, Amount: amount,
		State: ReservationHeld, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	return b, reservation, change, nil
}

// ReportUsage applies a provider's cumulative usage for a reservation. The
// product keeps the largest cumulative value seen, so a replayed or reordered
// report changes nothing and returns no changes. Usage past the held amount,
// or after release, is still recorded; any excess over the ceiling raises the
// recorded overshoot.
func (b Budget) ReportUsage(r Reservation, cumulative, expectedVersion uint64, at time.Time) (Budget, Reservation, []Change, error) {
	change := b.change("consume")
	if cumulative > MaxAmount || at.IsZero() {
		return b, r, nil, fmt.Errorf("%w: usage must be within bounds and timed", ErrInvalid)
	}
	switch {
	case r.Budget != b.ID || r.Tenant != b.Tenant:
		return b, r, nil, reject(change, RejectWrongRecord)
	case b.Version != expectedVersion:
		return b, r, nil, reject(change, RejectVersionConflict)
	case cumulative <= r.Consumed:
		return b, r, nil, nil
	}
	delta := cumulative - r.Consumed
	if b.Consumed+delta > MaxAmount {
		return b, r, nil, fmt.Errorf("%w: consumed usage would exceed the representable bound", ErrInvalid)
	}
	reservationChange := r.change("consume")
	before := r.outstanding()
	r.Consumed = cumulative
	b.Reserved -= before - r.outstanding()
	b.Consumed += delta
	b.Overshoot = max(b.Overshoot, b.excess())
	b, change = b.bump(change, at)
	r, reservationChange = r.bump(reservationChange, at)
	return b, r, []Change{change, reservationChange}, nil
}

// Release returns a reservation's unused hold to the budget, exactly once.
func (b Budget) Release(r Reservation, expectedVersion uint64, at time.Time) (Budget, Reservation, []Change, error) {
	change := b.change("release")
	reservationChange := r.change("release")
	switch {
	case r.Budget != b.ID || r.Tenant != b.Tenant:
		return b, r, nil, reject(change, RejectWrongRecord)
	case b.Version != expectedVersion:
		return b, r, nil, reject(change, RejectVersionConflict)
	case r.State == ReservationReleased:
		return b, r, nil, reject(reservationChange, RejectTerminalState)
	}
	b.Reserved -= r.outstanding()
	r.State = ReservationReleased
	b, change = b.bump(change, at)
	r, reservationChange = r.bump(reservationChange, at)
	return b, r, []Change{change, reservationChange}, nil
}
