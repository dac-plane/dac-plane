//go:build integration

package application_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

func TestSecurityMigrationRevocationSerializesWithCommand(t *testing.T) {
	f, cmd := migrationFixture(t)
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(f.ctx) }()
	if err := store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := application.CreateMigration(f.ctx, f.pool, cmd); done <- err }()
	waitCtx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	for {
		var blocked bool
		if err := f.pool.QueryRow(waitCtx, `select exists(select 1 from pg_stat_activity where wait_event_type='Lock' and query like '%from harness_administrator%for update%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-waitCtx.Done():
			t.Fatal("command did not reach permission lock")
		}
	}
	if err := u.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("revoked waiting command=%v", err)
	}
	if f.count(t, "migration_pass", "true") != 0 || f.count(t, "command_record", "true") != 0 {
		t.Fatal("revoked command wrote partial administration")
	}
}

func TestSecurityMigrationCrossTenantReadAndHoldPreventAcquisition(t *testing.T) {
	f, cmd := migrationFixture(t)
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "preview")
	v = migrate(t, f, v, lifecycle.MigrationHold, "hold")
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "held-reaper")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	claim, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim)
	if err != nil || claim.State != lifecycle.ClaimScheduled || claim.Epoch != 0 {
		t.Fatalf("held pass acquired=%+v %v", claim, err)
	}
	tenant, principal := mustID[ids.Tenant](t), mustID[ids.Principal](t)
	p := identity.Principal{Tenant: tenant, ID: principal, Kind: identity.PrincipalHuman}
	actor, err := identity.NewAuthorizationContext(p)
	if err != nil {
		t.Fatal(err)
	}
	f.seed(t, func(u *store.UnitOfWork) error {
		if err := store.InsertTenant(f.ctx, u, tenant); err != nil {
			return err
		}
		if err := store.InsertPrincipal(f.ctx, u, p); err != nil {
			return err
		}
		return store.SetHarnessAdministrator(f.ctx, u, tenant, principal, true)
	})
	if _, err := application.GetMigration(f.ctx, f.pool, actor, v.Pass.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant admin read=%v", err)
	}
	forged := cmd
	forged.Actor = actor
	if _, err := application.CreateMigration(f.ctx, f.pool, forged); !errors.Is(err, application.ErrNoHarness) {
		t.Fatalf("cross-tenant qualification reuse=%v", err)
	}
}

func migrationFixture(t *testing.T) (*fixture, application.CreateMigrationPass) {
	t.Helper()
	f := newFixture(t, lifecycle.HarnessLive)
	target := mustID[ids.Provider](t)
	f.seed(t, func(u *store.UnitOfWork) error {
		if err := store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, true); err != nil {
			return err
		}
		_, err := store.UpsertHarness(f.ctx, u, lifecycle.HarnessQualification{Tenant: f.tenant, Provider: target, Mode: lifecycle.HarnessLive, Version: 1, UpdatedAt: f.at})
		return err
	})
	return f, application.CreateMigrationPass{Command: application.Command{Actor: f.actor, Nonce: "migration-create"}, From: f.provider, To: target}
}

func migrate(t *testing.T, f *fixture, v application.MigrationView, event lifecycle.MigrationEvent, nonce string) application.MigrationView {
	t.Helper()
	next, err := application.AdministerMigration(f.ctx, f.pool, application.MigrationAdministration{Command: application.Command{Actor: f.actor, Nonce: nonce, ExpectedVersion: v.Pass.Version}, Migration: v.Pass.ID, Event: event})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestSecurityMigrationPermissionReplayAndPreviewHold(t *testing.T) {
	f, cmd := migrationFixture(t)
	f.seed(t, func(u *store.UnitOfWork) error {
		return store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, false)
	})
	if _, err := application.CreateMigration(f.ctx, f.pool, cmd); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("non-admin creation=%v", err)
	}
	if f.count(t, "migration_pass", "true") != 0 || f.count(t, "maintenance_claim", "true") != 0 {
		t.Fatal("denied administration wrote work")
	}
	f.seed(t, func(u *store.UnitOfWork) error {
		return store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, true)
	})
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	again, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil || again.Pass.ID != v.Pass.ID {
		t.Fatalf("create replay=%+v %v", again, err)
	}
	changed := cmd
	changed.To = mustID[ids.Provider](t)
	if _, err := application.CreateMigration(f.ctx, f.pool, changed); !errors.Is(err, application.ErrPayloadMismatch) {
		t.Fatalf("changed nonce=%v", err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "preview")
	if v.Preview.At.IsZero() || v.Preview.FromVersion != 1 || v.Preview.ToVersion != 1 || f.count(t, "outbox", "true") != 0 {
		t.Fatal("preview missing snapshot or enqueued provider work")
	}
	v = migrate(t, f, v, lifecycle.MigrationHold, "hold")
	if !v.Preview.At.IsZero() {
		t.Fatal("hold retained a usable preview")
	}
	if _, err := application.AdministerMigration(f.ctx, f.pool, application.MigrationAdministration{Command: application.Command{Actor: f.actor, Nonce: "held-execute", ExpectedVersion: v.Pass.Version}, Migration: v.Pass.ID, Event: lifecycle.MigrationExecute}); err == nil {
		t.Fatal("held migration executed")
	}
	v = migrate(t, f, v, lifecycle.MigrationRelease, "release")
	if v.Pass.State != lifecycle.MigrationPlanned || !v.Preview.At.IsZero() {
		t.Fatal("release reused preview")
	}
	f.seed(t, func(u *store.UnitOfWork) error {
		return store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, false)
	})
	if _, err := application.CreateMigration(f.ctx, f.pool, cmd); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("revoked replay=%v", err)
	}
	if _, err := application.GetMigration(f.ctx, f.pool, f.actor, v.Pass.ID); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("revoked read=%v", err)
	}
	f.seed(t, func(u *store.UnitOfWork) error {
		return store.SetHarnessAdministrator(f.ctx, u, f.tenant, f.principal, true)
	})
	v = migrate(t, f, v, lifecycle.MigrationCancel, "cancel")
	cmd.Nonce = "next-pass"
	next, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil || next.Claim != v.Claim {
		t.Fatalf("reusable slot=%+v %v", next, err)
	}
	if f.count(t, "maintenance_claim", "true") != 1 {
		t.Fatal("next pass allocated competing claim")
	}
	if _, err := application.AdministerMigration(f.ctx, f.pool, application.MigrationAdministration{Command: application.Command{Actor: f.actor, Nonce: "fake-complete", ExpectedVersion: next.Pass.Version}, Migration: next.Pass.ID, Event: lifecycle.MigrationComplete}); !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("fabricated outcome=%v", err)
	}
}

func TestSecurityMigrationStalePreviewAndClaimTakeover(t *testing.T) {
	f, cmd := migrationFixture(t)
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "preview")
	if _, err := f.pool.Exec(f.ctx, `update migration_administration set preview_at=clock_timestamp()-interval '6 minutes' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	execute := application.MigrationAdministration{Command: application.Command{Actor: f.actor, Nonce: "execute", ExpectedVersion: v.Pass.Version}, Migration: v.Pass.ID, Event: lifecycle.MigrationExecute}
	if _, err := application.AdministerMigration(f.ctx, f.pool, execute); err == nil {
		t.Fatal("expired preview executed")
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "fresh-preview")
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpsertHarness(f.ctx, u, lifecycle.HarnessQualification{Tenant: f.tenant, Provider: cmd.To, Mode: lifecycle.HarnessLive, Version: 2, UpdatedAt: time.Now()})
		return err
	})
	execute.ExpectedVersion = v.Pass.Version
	if _, err := application.AdministerMigration(f.ctx, f.pool, execute); err == nil {
		t.Fatal("changed qualification reused old preview")
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "current-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "execute-current")
	before, err := application.GetMigration(f.ctx, f.pool, f.actor, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	newWorker := func(owner string) *worker.Worker {
		t.Helper()
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), owner)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if err := newWorker("old-holder").ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	old, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim)
	if err != nil || old.State != lifecycle.ClaimClaimed || old.Claimant != "old-holder" {
		t.Fatalf("initial scheduled acquisition=%+v %v", old, err)
	}
	f.reopen(t)
	if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if err := newWorker("new-holder").ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim)
	if err != nil || current.Epoch <= old.Epoch || current.Claimant != "new-holder" {
		t.Fatalf("takeover=%+v %v", current, err)
	}
	for _, action := range []string{"renew", "complete", "release"} {
		u, err := store.Begin(f.ctx, f.pool)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.HoldClaim(f.ctx, u, f.tenant, v.Claim, func(c lifecycle.MaintenanceClaim) (lifecycle.MaintenanceClaim, lifecycle.Change, error) {
			switch action {
			case "renew":
				return c.Renew(old.Claimant, old.Epoch, time.Minute, time.Now())
			case "complete":
				return c.Complete(old.Claimant, old.Epoch, time.Now())
			default:
				return c.Release(old.Claimant, old.Epoch, time.Now())
			}
		}, store.Transition{Actor: worker.SystemActor(), At: time.Now(), AuditID: mustID[ids.Audit](t)})
		_ = u.Rollback(f.ctx)
		requireRefusal(t, err, lifecycle.RejectStaleClaim)
	}
	after, err := application.GetMigration(f.ctx, f.pool, f.actor, v.Pass.ID)
	if err != nil || after.Pass != before.Pass || after.Preview != before.Preview {
		t.Fatalf("takeover changed administration=%+v %v", after, err)
	}
	if f.count(t, "outbox", "true") != 0 || f.count(t, "binding", "true") != 0 {
		t.Fatal("claim maintenance fabricated migration effects")
	}
	// A subsequently demoted target cannot acquire or renew administrative
	// authority, even though the pass itself remains explicitly unresolved.
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpsertHarness(f.ctx, u, lifecycle.HarnessQualification{Tenant: f.tenant, Provider: cmd.To, Mode: lifecycle.HarnessShadow, Version: 3, UpdatedAt: time.Now()})
		return err
	})
	if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := newWorker("demoted-target").ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	if after, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim); err != nil || after != beforeClaim {
		t.Fatalf("shadow target acquired claim=%+v %v", after, err)
	}
}

func TestSecurityMigrationDowngradePreservesAdministration(t *testing.T) {
	f, cmd := migrationFixture(t)
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", f.pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.ApplyVersion(f.ctx, 7, false); err == nil || !strings.Contains(err.Error(), "version 7 migration administration") {
		t.Fatalf("destructive downgrade=%v", err)
	}
	if _, err = application.GetMigration(f.ctx, f.pool, f.actor, v.Pass.ID); err != nil {
		t.Fatal("failed downgrade lost administration", err)
	}
	legacy, err := lifecycle.NewMigrationPass(mustID[ids.Migration](t), f.tenant, cmd.From, cmd.To, f.at)
	if err != nil {
		t.Fatal(err)
	}
	f.seed(t, func(u *store.UnitOfWork) error { _, _, err := store.InsertMigrationPass(f.ctx, u, legacy); return err })
	old, err := application.GetMigration(f.ctx, f.pool, f.actor, legacy.ID)
	if err != nil || old.Pass.ID != legacy.ID || !old.Preview.At.IsZero() || !old.Claim.IsZero() {
		t.Fatalf("legacy read fabricated administration=%+v %v", old, err)
	}
}
