// Package identity defines tenant, principal and delegated-authority contracts.
//
// The package is transport-neutral and holds no credential material: no type
// here can carry a provider secret, so no product or audit record built from
// these types can leak one.
package identity

import (
	"errors"
	"fmt"
	"slices"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var (
	// ErrInvalid reports a malformed identity or authority value.
	ErrInvalid = errors.New("invalid identity contract value")
	// ErrCrossTenant reports use of a resource owned by another tenant.
	ErrCrossTenant = errors.New("cross-tenant access")
)

// PrincipalKind distinguishes people from automated callers.
type PrincipalKind string

const (
	PrincipalHuman   PrincipalKind = "human"
	PrincipalService PrincipalKind = "service"
)

// PrincipalKinds lists every valid PrincipalKind.
func PrincipalKinds() []PrincipalKind {
	return []PrincipalKind{PrincipalHuman, PrincipalService}
}

// Principal is an actor within exactly one tenant. A person who works in two
// tenants is two principals. Providers are not principals; they are audiences
// of authority.
type Principal struct {
	Tenant ids.TenantID    `json:"tenant_id"`
	ID     ids.PrincipalID `json:"principal_id"`
	Kind   PrincipalKind   `json:"kind"`
}

// Validate reports whether every field is set and the kind is known.
func (p Principal) Validate() error {
	if p.Tenant.IsZero() || p.ID.IsZero() {
		return fmt.Errorf("%w: principal requires tenant and principal identifiers", ErrInvalid)
	}
	if !slices.Contains(PrincipalKinds(), p.Kind) {
		return fmt.Errorf("%w: unknown principal kind", ErrInvalid)
	}
	return nil
}

// AuthorizationContext is the authenticated actor of one command, independent
// of the transport that authenticated it. Construct it with
// NewAuthorizationContext so it always holds a valid principal.
type AuthorizationContext struct {
	principal Principal
}

// NewAuthorizationContext binds an authenticated principal to a command.
func NewAuthorizationContext(principal Principal) (AuthorizationContext, error) {
	if err := principal.Validate(); err != nil {
		return AuthorizationContext{}, err
	}
	return AuthorizationContext{principal: principal}, nil
}

// Principal returns the authenticated principal.
func (c AuthorizationContext) Principal() Principal {
	return c.principal
}

// Tenant returns the tenant the command acts within.
func (c AuthorizationContext) Tenant() ids.TenantID {
	return c.principal.Tenant
}

// RequireTenant rejects access to a resource owned by another tenant. A zero
// context or owner is rejected rather than treated as a match.
func (c AuthorizationContext) RequireTenant(owner ids.TenantID) error {
	if c.principal.Tenant.IsZero() || owner.IsZero() || c.principal.Tenant != owner {
		return ErrCrossTenant
	}
	return nil
}
