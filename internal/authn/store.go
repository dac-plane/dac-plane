package authn

import (
	"context"
	"errors"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// StoreResolver resolves bindings from PostgreSQL on every call, so disabling
// a binding takes effect on the next request.
func StoreResolver(q store.Querier) Resolver {
	return func(ctx context.Context, issuer, subject string, tenant ids.TenantID) (identity.Principal, bool, error) {
		principal, err := store.ResolveIdentityBinding(ctx, q, issuer, subject, tenant)
		if errors.Is(err, store.ErrNotFound) {
			return identity.Principal{}, false, nil
		}
		if err != nil {
			return identity.Principal{}, false, err
		}
		return principal, true, nil
	}
}
