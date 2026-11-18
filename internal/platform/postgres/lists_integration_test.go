//go:build integration

package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

func TestProviderNameSurvivesRegisteringAgainWithoutOne(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	at := time.Now().UTC().Truncate(time.Microsecond)
	provider := mustID[ids.Provider](t)
	put := func(name string) {
		t.Helper()
		inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
			if err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{Tenant: f.tenant, Provider: provider, Role: contracts.RoleRuntime, Name: name, UpdatedAt: at}); err != nil {
				t.Fatalf("PutProviderRegistration(%q) error = %v", name, err)
			}
		})
	}
	nameOf := func() string {
		t.Helper()
		listings, err := store.ListProviderListings(ctx, pool, f.tenant, []contracts.Role{contracts.RoleRuntime})
		if err != nil || len(listings) != 1 {
			t.Fatalf("ListProviderListings() = %v, %v", listings, err)
		}
		return listings[0].Name
	}
	put("Codex")
	put("")
	if got := nameOf(); got != "Codex" {
		t.Fatalf("name after registering again without one = %q, want Codex", got)
	}
	put("Codex on Camer")
	if got := nameOf(); got != "Codex on Camer" {
		t.Fatalf("name after renaming = %q", got)
	}
}

// A binary serving schema 19 during a rollout lists providers without names and
// refuses to write one (ADR-0011).
func TestProviderNamesBeforeTheListSchema(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	at := time.Now().UTC().Truncate(time.Microsecond)
	provider := mustID[ids.Provider](t)
	inTx(t, ctx, pool, func(uow *store.UnitOfWork) {
		if err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{Tenant: f.tenant, Provider: provider, Role: contracts.RoleEnvironment, Name: "Box", UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := pool.Exec(ctx, `update goose_db_version set is_applied = false where version_id = 20`); err != nil {
		t.Fatal(err)
	}
	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{Tenant: f.tenant, Provider: mustID[ids.Provider](t), Role: contracts.RoleEnvironment, Name: "Other", UpdatedAt: at})
	if !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Fatalf("a name before the list schema error = %v, want ErrSchemaUnavailable", err)
	}
	if err := store.PutProviderRegistration(ctx, uow, contracts.ProviderRegistration{Tenant: f.tenant, Provider: mustID[ids.Provider](t), Role: contracts.RoleEnvironment, UpdatedAt: at}); err != nil {
		t.Fatalf("an unnamed registration before the list schema error = %v", err)
	}
	listings, err := store.ListProviderListings(ctx, uow, f.tenant, []contracts.Role{contracts.RoleEnvironment})
	if err != nil || len(listings) != 2 {
		t.Fatalf("ListProviderListings() = %v, %v, want both registrations", listings, err)
	}
	for _, l := range listings {
		if l.Name != "" {
			t.Fatalf("listing %+v carries a name before the list schema", l)
		}
	}
}
