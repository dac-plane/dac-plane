//go:build integration

package provision_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/provision"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

const (
	issuer  = "https://issuer.example/realms/a"
	subject = "248289761001"
)

var provisionedAt = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (context.Context, *pgxpool.Pool, *provision.Provisioner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = db.Close()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	provisioner, err := provision.New(pool, func() time.Time { return provisionedAt })
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, provisioner
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func changes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `select operation from provisioning_change where tenant_id = $1 order by change_id`, tenant.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var operations []string
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
	}
	return operations
}

func TestProvisionIdentityLifecycle(t *testing.T) {
	ctx, pool, p := setup(t)
	requested := mustID[ids.Tenant](t)
	tenant, err := p.CreateTenant(ctx, requested)
	if err != nil || tenant != requested {
		t.Fatalf("CreateTenant() = %v, %v", tenant, err)
	}
	if again, err := p.CreateTenant(ctx, tenant); err != nil || again != tenant {
		t.Fatalf("repeated CreateTenant() = %v, %v", again, err)
	}
	principal, err := p.CreatePrincipal(ctx, tenant, ids.PrincipalID{}, identity.PrincipalHuman)
	if err != nil {
		t.Fatalf("CreatePrincipal() error = %v", err)
	}
	if _, err := p.CreatePrincipal(ctx, tenant, principal, identity.PrincipalService); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("CreatePrincipal(other kind) error = %v, want ErrConflict", err)
	}
	if err := p.BindIdentity(ctx, tenant, principal, issuer, subject); err != nil {
		t.Fatalf("BindIdentity() error = %v", err)
	}
	if err := p.BindIdentity(ctx, tenant, principal, issuer, subject); err != nil {
		t.Fatalf("repeated BindIdentity() error = %v", err)
	}
	resolved, err := store.ResolveIdentityBinding(ctx, pool, issuer, subject, tenant)
	if err != nil || resolved.ID != principal || resolved.Kind != identity.PrincipalHuman {
		t.Fatalf("ResolveIdentityBinding() = %+v, %v", resolved, err)
	}
	binding, err := store.GetIdentityBinding(ctx, pool, tenant, issuer, subject)
	if err != nil || !binding.Enabled || !binding.UpdatedAt.Equal(provisionedAt) {
		t.Fatalf("GetIdentityBinding() = %+v, %v", binding, err)
	}

	other, err := p.CreatePrincipal(ctx, tenant, ids.PrincipalID{}, identity.PrincipalHuman)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BindIdentity(ctx, tenant, other, issuer, subject); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("rebinding to another principal error = %v, want ErrConflict", err)
	}

	if err := p.UnbindIdentity(ctx, tenant, issuer, subject); err != nil {
		t.Fatalf("UnbindIdentity() error = %v", err)
	}
	if _, err := store.ResolveIdentityBinding(ctx, pool, issuer, subject, tenant); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve after unbind error = %v, want ErrNotFound", err)
	}
	if err := p.UnbindIdentity(ctx, tenant, issuer, "never-bound"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unbind unknown identity error = %v, want ErrNotFound", err)
	}
	if err := p.BindIdentity(ctx, tenant, principal, issuer, subject); err != nil {
		t.Fatalf("re-enable binding error = %v", err)
	}

	want := []string{"create_tenant", "create_tenant", "create_principal", "bind_identity", "bind_identity", "create_principal", "unbind_identity", "bind_identity"}
	if got := changes(t, ctx, pool, tenant); !slices.Equal(got, want) {
		t.Fatalf("provisioning changes = %v, want %v", got, want)
	}
}

func TestProvisionBindingIsScopedToItsTenant(t *testing.T) {
	ctx, pool, p := setup(t)
	first, _ := p.CreateTenant(ctx, ids.TenantID{})
	second, _ := p.CreateTenant(ctx, ids.TenantID{})
	firstPrincipal, err := p.CreatePrincipal(ctx, first, ids.PrincipalID{}, identity.PrincipalHuman)
	if err != nil {
		t.Fatal(err)
	}
	secondPrincipal, err := p.CreatePrincipal(ctx, second, ids.PrincipalID{}, identity.PrincipalHuman)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BindIdentity(ctx, first, firstPrincipal, issuer, subject); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveIdentityBinding(ctx, pool, issuer, subject, second); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding leaked into another tenant: %v", err)
	}
	if err := p.BindIdentity(ctx, second, firstPrincipal, issuer, subject); err == nil {
		t.Fatal("bound a principal of another tenant")
	}
	if err := p.BindIdentity(ctx, second, secondPrincipal, issuer, subject); err != nil {
		t.Fatal(err)
	}
	for tenant, want := range map[ids.TenantID]ids.PrincipalID{first: firstPrincipal, second: secondPrincipal} {
		if resolved, err := store.ResolveIdentityBinding(ctx, pool, issuer, subject, tenant); err != nil || resolved.ID != want {
			t.Fatalf("tenant %s resolved %+v, %v; want %s", tenant, resolved, err, want)
		}
	}
	if _, err := store.ResolveIdentityBinding(ctx, pool, "https://other.example", subject, first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("subject matched under another issuer: %v", err)
	}
}

func TestProvisionTenantReaderGrantAndRevoke(t *testing.T) {
	ctx, pool, p := setup(t)
	tenant, _ := p.CreateTenant(ctx, ids.TenantID{})
	principal, err := p.CreatePrincipal(ctx, tenant, ids.PrincipalID{}, identity.PrincipalService)
	if err != nil {
		t.Fatal(err)
	}
	reader := func() bool {
		u, err := store.Begin(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = u.Rollback(ctx) }()
		enabled, err := store.LockTenantReader(ctx, u, tenant, principal)
		if err != nil {
			t.Fatal(err)
		}
		return enabled
	}
	if reader() {
		t.Fatal("reader before grant")
	}
	if err := p.SetTenantReader(ctx, tenant, principal, true); err != nil || !reader() {
		t.Fatalf("grant: %v, reader %v", err, reader())
	}
	if err := p.SetTenantReader(ctx, tenant, principal, false); err != nil || reader() {
		t.Fatalf("revoke: %v, reader %v", err, reader())
	}
	if err := p.SetTenantReader(ctx, tenant, mustID[ids.Principal](t), true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant to unknown principal error = %v", err)
	}
}

func TestProvisioningChangesAreImmutable(t *testing.T) {
	ctx, pool, p := setup(t)
	tenant, _ := p.CreateTenant(ctx, ids.TenantID{})
	if _, err := pool.Exec(ctx, `update provisioning_change set operation = 'grant_reader' where tenant_id = $1`, tenant.String()); err == nil {
		t.Fatal("provisioning change updated")
	}
	if _, err := pool.Exec(ctx, `delete from provisioning_change where tenant_id = $1`, tenant.String()); err == nil {
		t.Fatal("provisioning change deleted")
	}
}

func TestProvisionCommandWritesJSON(t *testing.T) {
	ctx, pool, p := setup(t)
	open := func(context.Context) (*provision.Provisioner, func(), error) { return p, func() {}, nil }
	run := func(args ...string) map[string]string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := provision.Run(ctx, args, &stdout, &stderr, open); code != provision.ExitOK {
			t.Fatalf("Run(%v) = %d: %s", args, code, stderr.String())
		}
		var result map[string]string
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("output %q: %v", stdout.String(), err)
		}
		return result
	}
	tenant := run("tenant")["tenant_id"]
	principal := run("principal", "-tenant", tenant, "-kind", "service")["principal_id"]
	run("bind", "-tenant", tenant, "-principal", principal, "-issuer", issuer, "-subject", "ci-runner")
	run("grant-reader", "-tenant", tenant, "-principal", principal)
	tenantID, _ := ids.Parse[ids.Tenant](tenant)
	if resolved, err := store.ResolveIdentityBinding(ctx, pool, issuer, "ci-runner", tenantID); err != nil || resolved.ID.String() != principal || resolved.Kind != identity.PrincipalService {
		t.Fatalf("resolved %+v, %v", resolved, err)
	}

	var stdout, stderr bytes.Buffer
	code := provision.Run(ctx, []string{"bind", "-tenant", tenant, "-principal", principal, "-issuer", "http://plain.example", "-subject", "x"}, &stdout, &stderr, open)
	if code != provision.ExitError || stdout.Len() != 0 {
		t.Fatalf("plaintext issuer: code %d stdout %q", code, stdout.String())
	}
}
