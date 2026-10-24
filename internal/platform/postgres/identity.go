package postgres

import (
	"context"
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ProvisioningOperation names one trusted provisioning change.
type ProvisioningOperation string

const (
	ProvisionCreateTenant    ProvisioningOperation = "create_tenant"
	ProvisionCreatePrincipal ProvisioningOperation = "create_principal"
	ProvisionBindIdentity    ProvisioningOperation = "bind_identity"
	ProvisionUnbindIdentity  ProvisioningOperation = "unbind_identity"
	ProvisionGrantReader     ProvisioningOperation = "grant_reader"
	ProvisionRevokeReader    ProvisioningOperation = "revoke_reader"
)

// ProvisioningChange is one append-only record of what trusted provisioning
// changed. Issuer and Subject are set only for identity and adapter binding
// operations; Principal only for principal operations; Provider only for
// provider operations. It never carries credential material.
type ProvisioningChange struct {
	Operation ProvisioningOperation
	Tenant    ids.TenantID
	Principal ids.PrincipalID
	Provider  ids.ProviderID
	Issuer    string
	Subject   string
}

// RecordProvisioningChange appends one provisioning change in the caller's
// transaction, so the change and its record commit together.
func RecordProvisioningChange(ctx context.Context, u *UnitOfWork, change ProvisioningChange) error {
	var principal, provider, issuer, subject *string
	if !change.Principal.IsZero() {
		value := change.Principal.String()
		principal = &value
	}
	if !change.Provider.IsZero() {
		value := change.Provider.String()
		provider = &value
	}
	if change.Issuer != "" {
		issuer = &change.Issuer
	}
	if change.Subject != "" {
		subject = &change.Subject
	}
	_, err := u.Exec(ctx, `
		insert into provisioning_change (operation, tenant_id, principal_id, provider_id, issuer, subject)
		values ($1, $2, $3, $4, $5, $6)`,
		string(change.Operation), change.Tenant.String(), principal, provider, issuer, subject,
	)
	return err
}

// PutIdentityBinding creates a binding or updates the enabled state of an
// existing binding for the same principal. A binding of the same identity in
// the same tenant to a different principal is never moved silently: it is
// ErrPayloadMismatch and nothing is written.
func PutIdentityBinding(ctx context.Context, u *UnitOfWork, binding identity.IdentityBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	var written bool
	err := u.QueryRow(ctx, `
		insert into identity_binding (issuer, subject, tenant_id, principal_id, enabled, updated_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (issuer, subject, tenant_id) do update
		set enabled = excluded.enabled, updated_at = excluded.updated_at
		where identity_binding.principal_id = excluded.principal_id
		returning true`,
		binding.Issuer, binding.Subject, binding.Tenant.String(), binding.Principal.String(), binding.Enabled, binding.UpdatedAt,
	).Scan(&written)
	if isNoRows(err) {
		return ErrPayloadMismatch
	}
	return err
}

// GetIdentityBinding loads one binding, enabled or not.
func GetIdentityBinding(ctx context.Context, q Querier, tenant ids.TenantID, issuer, subject string) (identity.IdentityBinding, error) {
	binding := identity.IdentityBinding{Tenant: tenant, Issuer: issuer, Subject: subject}
	var principalText string
	err := q.QueryRow(ctx, `
		select principal_id::text, enabled, updated_at from identity_binding
		where issuer = $1 and subject = $2 and tenant_id = $3`,
		issuer, subject, tenant.String(),
	).Scan(&principalText, &binding.Enabled, &binding.UpdatedAt)
	if isNoRows(err) {
		return identity.IdentityBinding{}, ErrNotFound
	}
	if err != nil {
		return identity.IdentityBinding{}, err
	}
	if binding.Principal, err = parseID[ids.Principal](`identity_binding.principal_id`, principalText); err != nil {
		return identity.IdentityBinding{}, err
	}
	binding.UpdatedAt = binding.UpdatedAt.UTC()
	return binding, binding.Validate()
}

// ResolveIdentityBinding returns the principal an enabled binding of
// (issuer, subject) names in tenant. An unbound identity, a disabled binding
// and a binding in another tenant are all ErrNotFound, so a caller cannot tell
// them apart.
func ResolveIdentityBinding(ctx context.Context, q Querier, issuer, subject string, tenant ids.TenantID) (identity.Principal, error) {
	var principalText, kind string
	err := q.QueryRow(ctx, `
		select p.principal_id::text, p.kind
		from identity_binding b
		join principal p on p.tenant_id = b.tenant_id and p.principal_id = b.principal_id
		where b.issuer = $1 and b.subject = $2 and b.tenant_id = $3 and b.enabled`,
		issuer, subject, tenant.String(),
	).Scan(&principalText, &kind)
	if isNoRows(err) {
		return identity.Principal{}, ErrNotFound
	}
	if err != nil {
		return identity.Principal{}, err
	}
	id, err := parseID[ids.Principal](`principal.principal_id`, principalText)
	if err != nil {
		return identity.Principal{}, err
	}
	principal := identity.Principal{Tenant: tenant, ID: id, Kind: identity.PrincipalKind(kind)}
	if err := principal.Validate(); err != nil {
		return identity.Principal{}, fmt.Errorf("resolve identity binding: %w", err)
	}
	return principal, nil
}

// PutTenantReader sets a principal's tenant-reader permission. It is trusted
// provisioning, not a public command; updating the same row serializes a
// revocation with the reads that lock it.
func PutTenantReader(ctx context.Context, u *UnitOfWork, reader identity.TenantReader) error {
	if err := reader.Validate(); err != nil {
		return err
	}
	_, err := u.Exec(ctx, `
		insert into tenant_reader (tenant_id, principal_id, enabled, updated_at) values ($1, $2, $3, $4)
		on conflict (tenant_id, principal_id) do update set enabled = excluded.enabled, updated_at = excluded.updated_at`,
		reader.Tenant.String(), reader.Principal.String(), reader.Enabled, reader.UpdatedAt,
	)
	return err
}

// LockTenantReader reports whether principal currently holds the
// tenant-reader permission, holding a share lock on the row for the rest of
// the transaction so a concurrent revocation waits for the read to finish.
func LockTenantReader(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, principal ids.PrincipalID) (bool, error) {
	var enabled bool
	err := u.QueryRow(ctx, `
		select enabled from tenant_reader where tenant_id = $1 and principal_id = $2 for share`,
		tenant.String(), principal.String(),
	).Scan(&enabled)
	if isNoRows(err) {
		return false, nil
	}
	return enabled, err
}
