// Package provision is the trusted provisioning ADR-0007 and ADR-0008
// define: the only way to create tenants and principals, bind external
// identities to principals, grant or revoke the tenant-reader permission,
// register providers and bind adapter identities to them. The Surface
// API exposes none of it. An operator runs it with database access, which in a
// self-hosted deployment is already the root of trust, so the first binding
// needs no pre-existing administrator.
//
// Every operation is idempotent and records a provisioning change in the same
// transaction as its effect.
package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// ErrConflict reports an operation that would silently change an existing
// record, such as binding an identity already bound to another principal of
// the tenant. Nothing is written.
var ErrConflict = errors.New("provisioning conflicts with an existing record")

// Provisioner performs trusted provisioning against one database.
type Provisioner struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a provisioner. now stamps binding and permission updates.
func New(pool *pgxpool.Pool, now func() time.Time) (*Provisioner, error) {
	if pool == nil || now == nil {
		return nil, errors.New("provision: pool and clock are required")
	}
	return &Provisioner{pool: pool, now: now}, nil
}

// CreateTenant creates tenant with an initial empty authority policy. A zero
// tenant gets a new identifier; repeating the call with the same identifier
// changes nothing.
func (p *Provisioner) CreateTenant(ctx context.Context, tenant ids.TenantID) (ids.TenantID, error) {
	if tenant.IsZero() {
		var err error
		if tenant, err = ids.New[ids.Tenant](); err != nil {
			return ids.TenantID{}, err
		}
	}
	err := p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if err := store.InsertTenant(ctx, u, tenant); err != nil {
			return err
		}
		switch _, err := store.GetPolicy(ctx, u, tenant); {
		case errors.Is(err, store.ErrNotFound):
			if err := store.InsertPolicy(ctx, u, identity.Policy{Tenant: tenant, Generation: 1}); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{Operation: store.ProvisionCreateTenant, Tenant: tenant})
	})
	return tenant, err
}

// CreatePrincipal creates a principal of kind in an existing tenant. A zero
// principal gets a new identifier; repeating the call with the same
// identifier and kind changes nothing, and a different kind is ErrConflict.
func (p *Provisioner) CreatePrincipal(ctx context.Context, tenant ids.TenantID, principal ids.PrincipalID, kind identity.PrincipalKind) (ids.PrincipalID, error) {
	if principal.IsZero() {
		var err error
		if principal, err = ids.New[ids.Principal](); err != nil {
			return ids.PrincipalID{}, err
		}
	}
	record := identity.Principal{Tenant: tenant, ID: principal, Kind: kind}
	if err := record.Validate(); err != nil {
		return ids.PrincipalID{}, err
	}
	err := p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if err := p.requireTenant(ctx, u, tenant); err != nil {
			return err
		}
		if err := store.InsertPrincipal(ctx, u, record); err != nil {
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{Operation: store.ProvisionCreatePrincipal, Tenant: tenant, Principal: principal})
	})
	return principal, err
}

// BindIdentity binds (issuer, subject) to principal in tenant, enabling an
// existing binding to the same principal. Binding an identity already bound
// to another principal of the tenant is ErrConflict; unbind it first.
func (p *Provisioner) BindIdentity(ctx context.Context, tenant ids.TenantID, principal ids.PrincipalID, issuer, subject string) error {
	return p.setBinding(ctx, store.ProvisionBindIdentity, identity.IdentityBinding{
		Tenant: tenant, Principal: principal, Issuer: issuer, Subject: subject, Enabled: true, UpdatedAt: p.now().UTC(),
	})
}

// UnbindIdentity disables the binding of (issuer, subject) in tenant. The
// next authentication of that identity in the tenant is refused. The binding
// row stays, so the history of who was bound remains.
func (p *Provisioner) UnbindIdentity(ctx context.Context, tenant ids.TenantID, issuer, subject string) error {
	if !identity.ValidIssuer(issuer) || !identity.ValidSubject(subject) {
		return fmt.Errorf("%w: issuer or subject is malformed", identity.ErrInvalid)
	}
	return p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		existing, err := store.GetIdentityBinding(ctx, u, tenant, issuer, subject)
		if err != nil {
			return err
		}
		existing.Enabled, existing.UpdatedAt = false, p.now().UTC()
		if err := store.PutIdentityBinding(ctx, u, existing); err != nil {
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{
			Operation: store.ProvisionUnbindIdentity, Tenant: tenant, Principal: existing.Principal, Issuer: issuer, Subject: subject,
		})
	})
}

func (p *Provisioner) setBinding(ctx context.Context, operation store.ProvisioningOperation, binding identity.IdentityBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	return p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if _, err := store.GetPrincipal(ctx, u, binding.Tenant, binding.Principal); err != nil {
			return err
		}
		if err := store.PutIdentityBinding(ctx, u, binding); err != nil {
			if errors.Is(err, store.ErrPayloadMismatch) {
				return ErrConflict
			}
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{
			Operation: operation, Tenant: binding.Tenant, Principal: binding.Principal, Issuer: binding.Issuer, Subject: binding.Subject,
		})
	})
}

// SetTenantReader grants (enabled) or revokes the tenant-reader permission.
func (p *Provisioner) SetTenantReader(ctx context.Context, tenant ids.TenantID, principal ids.PrincipalID, enabled bool) error {
	reader := identity.TenantReader{Tenant: tenant, Principal: principal, Enabled: enabled, UpdatedAt: p.now().UTC()}
	if err := reader.Validate(); err != nil {
		return err
	}
	operation := store.ProvisionRevokeReader
	if enabled {
		operation = store.ProvisionGrantReader
	}
	return p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if _, err := store.GetPrincipal(ctx, u, tenant, principal); err != nil {
			return err
		}
		if err := store.PutTenantReader(ctx, u, reader); err != nil {
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{Operation: operation, Tenant: tenant, Principal: principal})
	})
}

// RegisterProvider registers provider in role in an existing tenant. A zero
// provider gets a new identifier. A provider may be registered in several
// roles; registering it again in the same role changes only its artifact
// store. A runtime's artifact store must already be registered as one.
func (p *Provisioner) RegisterProvider(ctx context.Context, tenant ids.TenantID, provider ids.ProviderID, role contracts.Role, artifactStore *ids.ProviderID) (ids.ProviderID, error) {
	if provider.IsZero() {
		var err error
		if provider, err = ids.New[ids.Provider](); err != nil {
			return ids.ProviderID{}, err
		}
	}
	registration := contracts.ProviderRegistration{Tenant: tenant, Provider: provider, Role: role, ArtifactStore: artifactStore, UpdatedAt: p.now().UTC()}
	if err := registration.Validate(); err != nil {
		return ids.ProviderID{}, err
	}
	err := p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if err := p.requireTenant(ctx, u, tenant); err != nil {
			return err
		}
		if artifactStore != nil {
			if _, err := store.GetProviderRegistration(ctx, u, tenant, *artifactStore, contracts.RoleArtifactStore); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: provider %s is not registered as an artifact store", ErrConflict, artifactStore)
			} else if err != nil {
				return err
			}
		}
		if err := store.PutProviderRegistration(ctx, u, registration); err != nil {
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{Operation: store.ProvisionRegisterProvider, Tenant: tenant, Provider: provider})
	})
	return provider, err
}

// BindAdapter binds the adapter identity (issuer, subject) to the
// registration of provider in role, enabling an existing binding to the same
// registration. An identity already bound to another registration is
// ErrConflict: give each adapter registration its own client.
func (p *Provisioner) BindAdapter(ctx context.Context, tenant ids.TenantID, provider ids.ProviderID, role contracts.Role, issuer, subject string) error {
	binding := contracts.AdapterBinding{Issuer: issuer, Subject: subject, Tenant: tenant, Provider: provider, Role: role, Enabled: true, UpdatedAt: p.now().UTC()}
	if err := binding.Validate(); err != nil {
		return err
	}
	return p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		if _, err := store.GetProviderRegistration(ctx, u, tenant, provider, role); err != nil {
			return err
		}
		if err := store.PutAdapterBinding(ctx, u, binding); err != nil {
			return err
		}
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{
			Operation: store.ProvisionBindAdapter, Tenant: tenant, Provider: provider, Issuer: issuer, Subject: subject,
		})
	})
}

// UnbindAdapter disables the binding of the adapter identity (issuer,
// subject). Its next connection is refused, and its live connections close
// at their next binding check. The row stays as history.
func (p *Provisioner) UnbindAdapter(ctx context.Context, issuer, subject string) (contracts.AdapterBinding, error) {
	if !identity.ValidIssuer(issuer) || !identity.ValidSubject(subject) {
		return contracts.AdapterBinding{}, fmt.Errorf("%w: issuer or subject is malformed", identity.ErrInvalid)
	}
	var binding contracts.AdapterBinding
	err := p.inTransaction(ctx, func(u *store.UnitOfWork) error {
		existing, err := store.GetAdapterBinding(ctx, u, issuer, subject)
		if err != nil {
			return err
		}
		existing.Enabled, existing.UpdatedAt = false, p.now().UTC()
		if err := store.PutAdapterBinding(ctx, u, existing); err != nil {
			return err
		}
		binding = existing
		return store.RecordProvisioningChange(ctx, u, store.ProvisioningChange{
			Operation: store.ProvisionUnbindAdapter, Tenant: existing.Tenant, Provider: existing.Provider, Issuer: issuer, Subject: subject,
		})
	})
	return binding, err
}

func (p *Provisioner) requireTenant(ctx context.Context, u *store.UnitOfWork, tenant ids.TenantID) error {
	if tenant.IsZero() {
		return fmt.Errorf("%w: tenant is required", identity.ErrInvalid)
	}
	_, err := store.GetPolicy(ctx, u, tenant)
	return err
}

func (p *Provisioner) inTransaction(ctx context.Context, work func(*store.UnitOfWork) error) error {
	u, err := store.Begin(ctx, p.pool)
	if err != nil {
		return err
	}
	if err := work(u); err != nil {
		_ = u.Rollback(ctx)
		if errors.Is(err, store.ErrPayloadMismatch) {
			return ErrConflict
		}
		return err
	}
	return u.Commit(ctx)
}
