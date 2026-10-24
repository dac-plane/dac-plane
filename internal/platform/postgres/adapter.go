package postgres

import (
	"context"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const (
	ProvisionRegisterProvider ProvisioningOperation = "register_provider"
	ProvisionBindAdapter      ProvisioningOperation = "bind_adapter"
	ProvisionUnbindAdapter    ProvisioningOperation = "unbind_adapter"
)

// PutProviderRegistration creates a registration or changes the artifact
// store of an existing one. A provider's role never changes: registering it
// again with another role is ErrPayloadMismatch and nothing is written.
func PutProviderRegistration(ctx context.Context, u *UnitOfWork, registration contracts.ProviderRegistration) error {
	if err := registration.Validate(); err != nil {
		return err
	}
	var store *string
	if registration.ArtifactStore != nil {
		value := registration.ArtifactStore.String()
		store = &value
	}
	var written bool
	err := u.QueryRow(ctx, `
		insert into provider_registration (tenant_id, provider_id, role, artifact_store_id, updated_at)
		values ($1, $2, $3, $4, $5)
		on conflict (tenant_id, provider_id) do update
		set artifact_store_id = excluded.artifact_store_id, updated_at = excluded.updated_at
		where provider_registration.role = excluded.role
		returning true`,
		registration.Tenant.String(), registration.Provider.String(), string(registration.Role), store, registration.UpdatedAt,
	).Scan(&written)
	if isNoRows(err) {
		return ErrPayloadMismatch
	}
	return err
}

// GetProviderRegistration loads one registration.
func GetProviderRegistration(ctx context.Context, q Querier, tenant ids.TenantID, provider ids.ProviderID) (contracts.ProviderRegistration, error) {
	return scanRegistration(q.QueryRow(ctx, `
		select tenant_id::text, provider_id::text, role, artifact_store_id::text, updated_at
		from provider_registration where tenant_id = $1 and provider_id = $2`,
		tenant.String(), provider.String(),
	))
}

// PutAdapterBinding creates a binding or changes the enabled state of an
// existing binding to the same provider. An identity bound to one provider is
// never moved to another: that is ErrPayloadMismatch and nothing is written.
func PutAdapterBinding(ctx context.Context, u *UnitOfWork, binding contracts.AdapterBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	var written bool
	err := u.QueryRow(ctx, `
		insert into adapter_binding (issuer, subject, tenant_id, provider_id, enabled, updated_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (issuer, subject) do update
		set enabled = excluded.enabled, updated_at = excluded.updated_at
		where adapter_binding.tenant_id = excluded.tenant_id and adapter_binding.provider_id = excluded.provider_id
		returning true`,
		binding.Issuer, binding.Subject, binding.Tenant.String(), binding.Provider.String(), binding.Enabled, binding.UpdatedAt,
	).Scan(&written)
	if isNoRows(err) {
		return ErrPayloadMismatch
	}
	return err
}

// GetAdapterBinding loads one binding, enabled or not.
func GetAdapterBinding(ctx context.Context, q Querier, issuer, subject string) (contracts.AdapterBinding, error) {
	binding := contracts.AdapterBinding{Issuer: issuer, Subject: subject}
	var tenantText, providerText string
	err := q.QueryRow(ctx, `
		select tenant_id::text, provider_id::text, enabled, updated_at from adapter_binding
		where issuer = $1 and subject = $2`,
		issuer, subject,
	).Scan(&tenantText, &providerText, &binding.Enabled, &binding.UpdatedAt)
	if isNoRows(err) {
		return contracts.AdapterBinding{}, ErrNotFound
	}
	if err != nil {
		return contracts.AdapterBinding{}, err
	}
	if binding.Tenant, err = parseID[ids.Tenant](`adapter_binding.tenant_id`, tenantText); err != nil {
		return contracts.AdapterBinding{}, err
	}
	if binding.Provider, err = parseID[ids.Provider](`adapter_binding.provider_id`, providerText); err != nil {
		return contracts.AdapterBinding{}, err
	}
	binding.UpdatedAt = binding.UpdatedAt.UTC()
	return binding, binding.Validate()
}

// ResolveAdapterBinding returns the registration an enabled binding of
// (issuer, subject) speaks for. An unbound identity and a disabled binding are
// both ErrNotFound, so a caller cannot tell them apart.
func ResolveAdapterBinding(ctx context.Context, q Querier, issuer, subject string) (contracts.ProviderRegistration, error) {
	return scanRegistration(q.QueryRow(ctx, `
		select r.tenant_id::text, r.provider_id::text, r.role, r.artifact_store_id::text, r.updated_at
		from adapter_binding b
		join provider_registration r on r.tenant_id = b.tenant_id and r.provider_id = b.provider_id
		where b.issuer = $1 and b.subject = $2 and b.enabled`,
		issuer, subject,
	))
}

func scanRegistration(row interface{ Scan(...any) error }) (contracts.ProviderRegistration, error) {
	var registration contracts.ProviderRegistration
	var tenantText, providerText, role string
	var storeText *string
	err := row.Scan(&tenantText, &providerText, &role, &storeText, &registration.UpdatedAt)
	if isNoRows(err) {
		return contracts.ProviderRegistration{}, ErrNotFound
	}
	if err != nil {
		return contracts.ProviderRegistration{}, err
	}
	if registration.Tenant, err = parseID[ids.Tenant](`provider_registration.tenant_id`, tenantText); err != nil {
		return contracts.ProviderRegistration{}, err
	}
	if registration.Provider, err = parseID[ids.Provider](`provider_registration.provider_id`, providerText); err != nil {
		return contracts.ProviderRegistration{}, err
	}
	if storeText != nil {
		store, err := parseID[ids.Provider](`provider_registration.artifact_store_id`, *storeText)
		if err != nil {
			return contracts.ProviderRegistration{}, err
		}
		registration.ArtifactStore = &store
	}
	registration.Role, registration.UpdatedAt = contracts.Role(role), registration.UpdatedAt.UTC()
	return registration, registration.Validate()
}
