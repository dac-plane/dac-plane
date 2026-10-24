package contracts

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ProviderRegistration records that a provider of one role exists in a
// tenant. ArtifactStore names the artifact store a runtime exports its
// results to; only a runtime has one, and it is optional. A registration
// grants nothing by itself: an adapter speaks for it only through an enabled
// AdapterBinding, and each operation still needs its own authority.
type ProviderRegistration struct {
	Tenant        ids.TenantID    `json:"tenant_id"`
	Provider      ids.ProviderID  `json:"provider_id"`
	Role          Role            `json:"role"`
	ArtifactStore *ids.ProviderID `json:"artifact_store_id,omitempty"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Validate reports whether the registration is complete and only a runtime
// names an artifact store other than itself.
func (r ProviderRegistration) Validate() error {
	if r.Tenant.IsZero() || r.Provider.IsZero() || r.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: provider registration requires tenant, provider and update time", ErrInvalid)
	}
	if !slices.Contains(Roles(), r.Role) {
		return fmt.Errorf("%w: unknown provider role", ErrInvalid)
	}
	if r.ArtifactStore != nil && (r.Role != RoleRuntime || r.ArtifactStore.IsZero() || *r.ArtifactStore == r.Provider) {
		return fmt.Errorf("%w: only a runtime names an artifact store, and not itself", ErrInvalid)
	}
	return nil
}

// AdapterBinding maps an adapter's external identity, the pair (issuer,
// subject) of its client-credentials token, to the one provider it speaks for.
// It is the only source of the Peer a connection carries: nothing an adapter
// writes in a frame changes it. An identity binds to at most one provider,
// and a provider may have several bound identities. A disabled binding
// authenticates nobody, and its live connections close.
type AdapterBinding struct {
	Issuer    string         `json:"issuer"`
	Subject   string         `json:"subject"`
	Tenant    ids.TenantID   `json:"tenant_id"`
	Provider  ids.ProviderID `json:"provider_id"`
	Enabled   bool           `json:"enabled"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Validate reports whether every field is set and well formed.
func (b AdapterBinding) Validate() error {
	if b.Tenant.IsZero() || b.Provider.IsZero() || b.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: adapter binding requires tenant, provider and update time", ErrInvalid)
	}
	if !identity.ValidIssuer(b.Issuer) || !identity.ValidSubject(b.Subject) {
		return fmt.Errorf("%w: adapter binding requires a well-formed issuer and subject", ErrInvalid)
	}
	return nil
}

// Peer returns the provider identity the binding establishes.
func (b AdapterBinding) Peer() Peer {
	return Peer{Tenant: b.Tenant, Provider: b.Provider}
}
