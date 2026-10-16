package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// SpendState is the state of a two-phase spend authorization.
type SpendState string

const (
	SpendPending  SpendState = "pending"
	SpendAsserted SpendState = "asserted"
	SpendExpired  SpendState = "expired"
)

// SpendStates lists every SpendState.
func SpendStates() []SpendState {
	return []SpendState{SpendPending, SpendAsserted, SpendExpired}
}

// SpendTerms is what a principal is asked to authorize, and what an assertion
// must match exactly.
type SpendTerms struct {
	Tenant    ids.TenantID    `json:"tenant_id"`
	Principal ids.PrincipalID `json:"principal_id"`
	Task      ids.TaskID      `json:"task_id"`
	Attempt   ids.AttemptID   `json:"attempt_id"`
	Operation ids.OperationID `json:"operation_id"`
	Amount    uint64          `json:"amount"`
	Unit      Unit            `json:"unit"`
	Nonce     identity.Digest `json:"nonce"`
}

// Validate reports whether the terms are complete and the amount bounded.
func (s SpendTerms) Validate() error {
	if s.Tenant.IsZero() || s.Principal.IsZero() || s.Task.IsZero() || s.Attempt.IsZero() || s.Operation.IsZero() {
		return fmt.Errorf("%w: spend terms require tenant, principal, task, attempt and operation", ErrInvalid)
	}
	if s.Amount == 0 || s.Amount > MaxAmount {
		return fmt.Errorf("%w: spend amount must be positive and bounded", ErrInvalid)
	}
	if err := s.Unit.Validate(); err != nil {
		return err
	}
	return validDigest(s.Nonce)
}

// SpendChallenge is the first phase of spend authorization: the product asks
// a principal to authorize exactly these terms before ExpiresAt. The nonce is
// the replay identity. Spend authorization never reserves or consumes Budget;
// reservation and ceiling enforcement stay separate and authoritative.
type SpendChallenge struct {
	ID        ids.SpendID `json:"spend_id"`
	Terms     SpendTerms  `json:"terms"`
	State     SpendState  `json:"state"`
	Version   uint64      `json:"version"`
	ExpiresAt time.Time   `json:"expires_at"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// BeginSpend opens a pending challenge at version 1.
func BeginSpend(id ids.SpendID, terms SpendTerms, expiresAt, at time.Time) (SpendChallenge, error) {
	challenge := SpendChallenge{ID: id, Terms: terms, State: SpendPending, Version: 1, ExpiresAt: expiresAt, CreatedAt: at, UpdatedAt: at}
	return challenge, challenge.Validate()
}

// Validate reports whether the challenge is complete.
func (c SpendChallenge) Validate() error {
	if c.ID.IsZero() || c.Version == 0 {
		return fmt.Errorf("%w: spend challenge requires an identifier and a positive version", ErrInvalid)
	}
	if err := c.Terms.Validate(); err != nil {
		return err
	}
	if !slices.Contains(SpendStates(), c.State) {
		return fmt.Errorf("%w: unknown spend state", ErrInvalid)
	}
	if c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.UpdatedAt.Before(c.CreatedAt) {
		return fmt.Errorf("%w: spend challenge timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

func (c SpendChallenge) change(event string) Change {
	return Change{
		Tenant: c.Terms.Tenant, Record: RecordSpend, RecordID: c.ID.Untyped(),
		Event: event, From: string(c.State), Version: c.Version,
	}
}

// SpendAssertion is the second phase: a principal's claim to authorize terms
// for one challenge. Proof is the digest of the credential ceremony's output;
// the ceremony itself is not selected.
type SpendAssertion struct {
	Challenge  ids.SpendID     `json:"spend_id"`
	Terms      SpendTerms      `json:"terms"`
	Proof      identity.Digest `json:"proof"`
	AssertedAt time.Time       `json:"asserted_at"`
}

// SpendReceipt is the durable evidence of an authorized spend. It records
// authorization only: it reserves and consumes nothing.
type SpendReceipt struct {
	Challenge ids.SpendID     `json:"spend_id"`
	Terms     SpendTerms      `json:"terms"`
	Proof     identity.Digest `json:"proof"`
	IssuedAt  time.Time       `json:"issued_at"`
}

// Validate reports whether the receipt is complete.
func (r SpendReceipt) Validate() error {
	if r.Challenge.IsZero() || r.IssuedAt.IsZero() {
		return fmt.Errorf("%w: spend receipt requires a challenge and a time", ErrInvalid)
	}
	if err := r.Terms.Validate(); err != nil {
		return err
	}
	return validDigest(r.Proof)
}

// Assert completes the challenge if the assertion matches it exactly, arrives
// before expiry, and the challenge is still at expectedVersion and pending. A
// second assertion of the same challenge is a replay and is refused.
func (c SpendChallenge) Assert(assertion SpendAssertion, expectedVersion uint64) (SpendChallenge, SpendReceipt, Change, error) {
	change := c.change("assert")
	if assertion.AssertedAt.IsZero() {
		return c, SpendReceipt{}, change, fmt.Errorf("%w: assertion requires a time", ErrInvalid)
	}
	if err := validDigest(assertion.Proof); err != nil {
		return c, SpendReceipt{}, change, err
	}
	switch {
	case c.Version != expectedVersion:
		return c, SpendReceipt{}, change, reject(change, RejectVersionConflict)
	case c.State != SpendPending:
		return c, SpendReceipt{}, change, reject(change, RejectTerminalState)
	case assertion.Challenge != c.ID || assertion.Terms != c.Terms:
		return c, SpendReceipt{}, change, reject(change, RejectAssertionMismatch)
	case !assertion.AssertedAt.Before(c.ExpiresAt):
		return c, SpendReceipt{}, change, reject(change, RejectRequestExpired)
	}
	c.State = SpendAsserted
	c.Version++
	c.UpdatedAt = assertion.AssertedAt
	change.To, change.Version = string(c.State), c.Version
	return c, SpendReceipt{Challenge: c.ID, Terms: c.Terms, Proof: assertion.Proof, IssuedAt: assertion.AssertedAt}, change, nil
}

// Expire closes a pending challenge that was not asserted in time.
func (c SpendChallenge) Expire(expectedVersion uint64, at time.Time) (SpendChallenge, Change, error) {
	change := c.change("expire")
	switch {
	case c.Version != expectedVersion:
		return c, change, reject(change, RejectVersionConflict)
	case c.State != SpendPending:
		return c, change, reject(change, RejectTerminalState)
	case at.Before(c.ExpiresAt):
		return c, change, reject(change, RejectInvalidTransition)
	}
	c.State = SpendExpired
	c.Version++
	c.UpdatedAt = at
	change.To, change.Version = string(c.State), c.Version
	return c, change, nil
}
