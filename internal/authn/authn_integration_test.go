//go:build integration

package authn

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/provision"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

// TestAuthenticateThroughProvisionedBindings runs the whole path: a token
// from a discovered issuer, verified, then resolved through a binding that
// trusted provisioning created in PostgreSQL.
func TestAuthenticateThroughProvisionedBindings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
		t.Fatal(err)
	}
	_ = db.Close()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	provisioner, err := provision.New(pool, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}

	issuer := newTestIssuer(t)
	key := newRSAKey(t)
	issuer.publish("k1", key)
	config := Config{Issuers: []IssuerConfig{{Issuer: issuer.url(), Audience: testAudience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 3600}}}
	logs := &bytes.Buffer{}
	auth, err := New(ctx, config, StoreResolver(pool), slog.New(slog.NewJSONHandler(logs, nil)),
		WithHTTPClient(issuer.Client()), WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}

	tenant, err := provisioner.CreateTenant(ctx, ids.TenantID{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := provisioner.CreateTenant(ctx, ids.TenantID{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := provisioner.CreatePrincipal(ctx, tenant, ids.PrincipalID{}, identity.PrincipalHuman)
	if err != nil {
		t.Fatal(err)
	}
	token := issuer.token(t, tokenSpec{Key: key})
	const subject = "user-248289761001"

	if _, err := auth.Authenticate(ctx, token, tenant); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unbound identity: %v", err)
	}
	if err := provisioner.BindIdentity(ctx, tenant, principal, issuer.url(), subject); err != nil {
		t.Fatal(err)
	}
	actor, err := auth.Authenticate(ctx, token, tenant)
	if err != nil || actor.Principal().ID != principal || actor.Tenant() != tenant {
		t.Fatalf("bound identity = %+v, %v", actor.Principal(), err)
	}
	if _, err := auth.Authenticate(ctx, token, other); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("other tenant: %v", err)
	}
	if err := provisioner.UnbindIdentity(ctx, tenant, issuer.url(), subject); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, token, tenant); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unbound on the next request: %v", err)
	}

	pool.Close()
	if _, err := auth.Authenticate(context.Background(), token, tenant); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("database failure reported as %v, want an operational error", err)
	}
}
