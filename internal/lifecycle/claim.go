package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ClaimState is the state of a maintenance claim.
type ClaimState string

const (
	ClaimScheduled ClaimState = "scheduled"
	ClaimClaimed   ClaimState = "claimed"
	ClaimCompleted ClaimState = "completed"
)

// ClaimStates lists every ClaimState.
func ClaimStates() []ClaimState {
	return []ClaimState{ClaimScheduled, ClaimClaimed, ClaimCompleted}
}

// MaintenanceClaim schedules maintenance on one bound resource and lets one
// worker at a time perform it. A claim expires, and every claim or takeover
// advances Epoch, so a worker that lost its claim cannot renew, complete or
// release it: its epoch is stale.
type MaintenanceClaim struct {
	ID          ids.ClaimID    `json:"claim_id"`
	Tenant      ids.TenantID   `json:"tenant_id"`
	Provider    ids.ProviderID `json:"provider_id"`
	ResourceRef string         `json:"resource_ref"`
	State       ClaimState     `json:"state"`
	Claimant    string         `json:"claimant,omitzero"`
	Epoch       uint64         `json:"epoch"`
	ExpiresAt   time.Time      `json:"expires_at,omitzero"`
	Version     uint64         `json:"version"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// NewMaintenanceClaim returns a scheduled, unclaimed claim at epoch 0.
func NewMaintenanceClaim(id ids.ClaimID, tenant ids.TenantID, provider ids.ProviderID, resourceRef string, at time.Time) (MaintenanceClaim, error) {
	claim := MaintenanceClaim{ID: id, Tenant: tenant, Provider: provider, ResourceRef: resourceRef, State: ClaimScheduled, Version: 1, UpdatedAt: at}
	return claim, claim.Validate()
}

// Validate reports whether the record is complete and a claimant and expiry
// are present exactly while claimed.
func (c MaintenanceClaim) Validate() error {
	if c.ID.IsZero() || c.Tenant.IsZero() || c.Provider.IsZero() || c.Version == 0 || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: maintenance claim requires identifiers, version and time", ErrInvalid)
	}
	if !validNativeRef(c.ResourceRef) {
		return fmt.Errorf("%w: maintenance claim requires a bounded resource reference", ErrInvalid)
	}
	if !slices.Contains(ClaimStates(), c.State) {
		return fmt.Errorf("%w: unknown claim state", ErrInvalid)
	}
	claimed := c.State == ClaimClaimed
	if claimed != (c.Claimant != "") || claimed != !c.ExpiresAt.IsZero() || len(c.Claimant) > maxNativeRefLength {
		return fmt.Errorf("%w: a claimant and expiry exist exactly while claimed", ErrInvalid)
	}
	if claimed && c.Epoch == 0 {
		return fmt.Errorf("%w: a held claim has a positive epoch", ErrInvalid)
	}
	return nil
}

func (c MaintenanceClaim) change(event string) Change {
	return Change{
		Tenant: c.Tenant, Record: RecordClaim, RecordID: c.ID.Untyped(),
		Event: event, From: string(c.State), Version: c.Version,
	}
}

// Claim gives the claim to claimant until now+ttl. A scheduled claim, or a
// claimed one whose holder's time has run out, can be taken; taking it
// advances the epoch, which fences the previous holder.
func (c MaintenanceClaim) Claim(claimant string, ttl time.Duration, now time.Time) (MaintenanceClaim, Change, error) {
	change := c.change("claim")
	if !validNativeRef(claimant) || ttl <= 0 || now.IsZero() {
		return c, change, fmt.Errorf("%w: claim requires a claimant, a positive lease and a time", ErrInvalid)
	}
	switch {
	case c.State == ClaimCompleted:
		return c, change, reject(change, RejectTerminalState)
	case c.State == ClaimClaimed && now.Before(c.ExpiresAt):
		return c, change, reject(change, RejectClaimHeld)
	}
	c.State, c.Claimant, c.ExpiresAt = ClaimClaimed, claimant, now.Add(ttl)
	return c.advance(change, true, now)
}

// Renew extends the holder's claim to now+ttl.
func (c MaintenanceClaim) Renew(claimant string, epoch uint64, ttl time.Duration, now time.Time) (MaintenanceClaim, Change, error) {
	change := c.change("renew")
	if ttl <= 0 {
		return c, change, fmt.Errorf("%w: renew requires a positive lease", ErrInvalid)
	}
	if err := c.requireHolder(change, claimant, epoch, now); err != nil {
		return c, change, err
	}
	c.ExpiresAt = now.Add(ttl)
	return c.advance(change, false, now)
}

// Complete finishes the maintenance as the current holder.
func (c MaintenanceClaim) Complete(claimant string, epoch uint64, now time.Time) (MaintenanceClaim, Change, error) {
	change := c.change("complete")
	if err := c.requireHolder(change, claimant, epoch, now); err != nil {
		return c, change, err
	}
	c.State, c.Claimant, c.ExpiresAt = ClaimCompleted, "", time.Time{}
	return c.advance(change, false, now)
}

// Release gives the claim up unfinished, returning it to scheduled.
func (c MaintenanceClaim) Release(claimant string, epoch uint64, now time.Time) (MaintenanceClaim, Change, error) {
	change := c.change("release")
	if err := c.requireHolder(change, claimant, epoch, now); err != nil {
		return c, change, err
	}
	c.State, c.Claimant, c.ExpiresAt = ClaimScheduled, "", time.Time{}
	return c.advance(change, false, now)
}

// requireHolder fences every holder operation: the claim must be held, by
// this claimant, at this epoch, and not yet expired.
func (c MaintenanceClaim) requireHolder(change Change, claimant string, epoch uint64, now time.Time) error {
	if c.State != ClaimClaimed {
		return reject(change, RejectInvalidTransition)
	}
	if claimant != c.Claimant || epoch != c.Epoch || !now.Before(c.ExpiresAt) {
		return reject(change, RejectStaleClaim)
	}
	return nil
}

func (c MaintenanceClaim) advance(change Change, newEpoch bool, now time.Time) (MaintenanceClaim, Change, error) {
	if newEpoch {
		c.Epoch++
	}
	c.Version++
	c.UpdatedAt = now
	change.To = string(c.State)
	change.Version = c.Version
	return c, change, nil
}
