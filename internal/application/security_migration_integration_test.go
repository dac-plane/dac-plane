//go:build integration

package application_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

type reportUsageAfterMigrationProvision struct {
	contracts.Port
	report func()
	done   bool
}

type reportUsageAfterMigrationQuiesce struct {
	contracts.Port
	report func()
	done   bool
}

func (p *reportUsageAfterMigrationQuiesce) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if err == nil && !p.done && request.Method == contracts.MethodInterrupt {
		p.done = true
		p.report()
	}
	return reply, err
}

func (p *reportUsageAfterMigrationProvision) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if err == nil && !p.done && request.Method == contracts.MethodProvision {
		p.done = true
		p.report()
	}
	return reply, err
}

func TestSecurityMigrationRevocationSerializesWithCommand(t *testing.T) {
	t.Parallel()
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
		if err := f.pool.QueryRow(waitCtx, `select exists(select 1 from pg_stat_activity where datname=current_database() and wait_event_type='Lock' and query like '%from harness_administrator%for update%')`).Scan(&blocked); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f, cmd := migrationFixture(t)
	s := f.submit(t, f.submitCommand("migration-takeover-source"))
	runtime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{s.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, runtime)
	// Provisioning already bound the source environment resource; only the
	// environment provider may observe it, so the runtime reports its session.
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "takeover-session"}),
	)
	if got := f.ingest(t, f.event(runtime, s, 1, frame)); got.Applied != 1 {
		t.Fatalf("takeover source bindings=%+v", got)
	}
	outboxBefore, bindingsBefore := f.count(t, "outbox", "true"), f.count(t, "binding", "true")
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
	if f.count(t, "outbox", "true") != outboxBefore || f.count(t, "binding", "true") != bindingsBefore {
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

func TestMigrationExecutionSnapshotsTenantSourceAttempts(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	s := f.submit(t, f.submitCommand("migration-source"))
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "migration-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "migration-execute")
	lateBeforeWorker := f.submit(t, f.submitCommand("migration-after-execute-before-worker"))
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "migration-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SourceAttempt != s.Attempt.ID || items[0].SourceAttempt == lateBeforeWorker.Attempt.ID || items[0].State != lifecycle.MigrationItemUnresolved || items[0].Reason != "source_attempt_not_executing" || items[0].TargetAttempt != (ids.AttemptID{}) || pass.State != lifecycle.MigrationFailed {
		t.Fatalf("migration snapshot = %+v, pass=%s; want ineligible source durably unresolved and pass failed", items, pass.State)
	}
	claim, err := store.GetClaim(f.ctx, f.pool, f.tenant, v.Claim)
	if err != nil || claim.State != lifecycle.ClaimClaimed || claim.Claimant != "migration-snapshot" || items[0].ClaimEpoch != claim.Epoch {
		t.Fatalf("claim/fence = %+v / %+v / %v", claim, items, err)
	}
	late := f.submit(t, f.submitCommand("migration-after-snapshot"))
	if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if err := w.ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	items, err = store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil || len(items) != 1 || items[0].SourceAttempt != s.Attempt.ID || items[0].SourceAttempt == late.Attempt.ID {
		t.Fatalf("frozen migration snapshot enrolled later attempt: %+v, late=%s, err=%v", items, late.Attempt.ID, err)
	}
}

func TestMigrationRefusesLegacyTaskBeforeSourceQuiesce(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	submission := f.submit(t, f.submitCommand("migration-legacy-brief"))
	sourceRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations:        map[ids.OperationID]simulator.RuntimeOp{submission.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}},
		MigrationReceipts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, sourceRuntime)
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "legacy-source-session"}),
	)
	if got := f.ingest(t, f.event(sourceRuntime, submission, 1, frame)); got.Applied != 1 {
		t.Fatalf("legacy source bindings=%+v", got)
	}
	if _, err := f.pool.Exec(f.ctx, `update task set objective=null, acceptance_criteria=null where tenant_id=$1 and task_id=$2`, f.tenant.String(), submission.Task.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `update command_record set result=result-'objective'-'acceptance_criteria' where tenant_id=$1 and command='submit_task' and result->>'task'=$2`, f.tenant.String(), submission.Task.ID.String()); err != nil {
		t.Fatal(err)
	}

	view, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	view = migrate(t, f, view, lifecycle.MigrationPreview, "migration-legacy-preview")
	view = migrate(t, f, view, lifecycle.MigrationExecute, "migration-legacy-execute")
	targetRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.RuntimeScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	targetEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "migration-legacy-brief",
		wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: sourceRuntime, cmd.To: targetRuntime},
			wiretest.Environments(map[ids.ProviderID]contracts.Port{f.environmentProvider: sourceEnvironment, cmd.To: targetEnvironment})))
	if err != nil {
		t.Fatal(err)
	}
	for cycle := range 2 {
		if cycle > 0 {
			if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.ReapClaims(f.ctx); err != nil {
			t.Fatal(err)
		}
	}

	items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || len(items) != 1 || items[0].State != lifecycle.MigrationItemUnresolved || items[0].Reason != "legacy_task_missing_execution_brief" || !items[0].QuiesceOperation.IsZero() || !items[0].TargetAttempt.IsZero() {
		t.Fatalf("legacy migration items=%+v err=%v; want pre-quiesce refusal", items, err)
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || pass.State != lifecycle.MigrationFailed {
		t.Fatalf("legacy migration pass=%s err=%v; want failed", pass.State, err)
	}
	source, err := store.GetAttempt(f.ctx, f.pool, f.tenant, submission.Attempt.ID)
	if err != nil || source.State != lifecycle.AttemptExecuting {
		t.Fatalf("legacy source attempt=%s err=%v; want executing and never quiesced", source.State, err)
	}
	for _, sent := range sourceRuntime.Ledger().Sent() {
		if sent.Method == contracts.MethodInterrupt {
			t.Fatalf("legacy source was quiesced: %+v", sent)
		}
	}
}

func TestMigrationFailsCleanlyWhenReservationExhaustsDuringQuiesce(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	submission := f.submit(t, f.submitCommand("migration-exhausted-reservation"))
	sourceRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{submission.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}, MigrationReceipts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, sourceRuntime)
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "exhausted-source-session"}),
	)
	if got := f.ingest(t, f.event(sourceRuntime, submission, 1, frame)); got.Applied != 1 {
		t.Fatalf("source bindings=%+v", got)
	}
	view, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	view = migrate(t, f, view, lifecycle.MigrationPreview, "migration-exhausted-preview")
	view = migrate(t, f, view, lifecycle.MigrationExecute, "migration-exhausted-execute")
	targetRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.RuntimeScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	targetEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	quiescePort := &reportUsageAfterMigrationQuiesce{Port: sourceRuntime, report: func() {
		f.seed(t, func(u *store.UnitOfWork) error {
			budget, err := store.GetBudgetForUpdate(f.ctx, u, f.tenant, submission.Budget.ID)
			if err != nil {
				return err
			}
			reservation, err := store.GetReservationForUpdate(f.ctx, u, f.tenant, submission.Reservation.ID)
			if err != nil {
				return err
			}
			outboxID, err := ids.New[ids.Any]()
			if err != nil {
				return err
			}
			_, _, _, err = store.ReportUsage(f.ctx, u, f.tenant, budget.ID, reservation.ID, reservation.Amount, store.Transition{
				ExpectedVersion: budget.Version, At: f.at.Add(2 * time.Minute), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal},
				AuditID: mustID[ids.Audit](t), Outbox: &store.OutboxPayload{ID: outboxID, Body: []byte(`{"usage":"migration-test"}`)},
			})
			return err
		})
	}}
	for cycle := range 6 {
		if cycle > 0 {
			if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
		}
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), fmt.Sprintf("migration-exhausted-%d", cycle),
			wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: quiescePort, cmd.To: targetRuntime},
				wiretest.Environments(map[ids.ProviderID]contracts.Port{f.environmentProvider: sourceEnvironment, cmd.To: targetEnvironment})))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.ReapClaims(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || len(items) != 1 || items[0].State != lifecycle.MigrationItemFailed {
		t.Fatalf("migration items=%+v err=%v; want terminal failed item after a quiesce-time budget exhaustion", items, err)
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || pass.State != lifecycle.MigrationFailed {
		t.Fatalf("migration pass=%s err=%v; want failed", pass.State, err)
	}
	sourceAttempt, err := store.GetAttempt(f.ctx, f.pool, f.tenant, submission.Attempt.ID)
	if err != nil || sourceAttempt.State != lifecycle.AttemptFailed {
		t.Fatalf("source attempt=%s err=%v; want failed rather than stranded migrating", sourceAttempt.State, err)
	}
	latest, err := store.GetAttemptByTask(f.ctx, f.pool, f.tenant, submission.Task.ID)
	if err != nil || latest.ID != sourceAttempt.ID {
		t.Fatalf("latest attempt=%s err=%v; want source %s and no orphan target", latest.ID, err, sourceAttempt.ID)
	}
	stops := 0
	for _, request := range sourceEnvironment.Ledger().Sent() {
		if request.Method == contracts.MethodStop {
			stops++
		}
	}
	if stops != 1 || sourceEnvironment.Ledger().Effects(items[0].CleanupOperation) != 1 {
		t.Fatalf("quiesced source cleanup requests=%d effects=%d; want one confirmed stop", stops, sourceEnvironment.Ledger().Effects(items[0].CleanupOperation))
	}
}

func TestMigrationRechecksBudgetBeforeStartingTarget(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	submit := f.submitCommand("migration-budget-recheck")
	submit.Ceiling = 150
	submission := f.submit(t, submit)
	sourceRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{submission.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}, MigrationReceipts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, sourceRuntime)
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "budget-source-session"}),
	)
	if got := f.ingest(t, f.event(sourceRuntime, submission, 1, frame)); got.Applied != 1 {
		t.Fatalf("source bindings=%+v", got)
	}
	view, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	view = migrate(t, f, view, lifecycle.MigrationPreview, "migration-budget-preview")
	view = migrate(t, f, view, lifecycle.MigrationExecute, "migration-budget-execute")
	targetRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.RuntimeScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	targetEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	provision := &reportUsageAfterMigrationProvision{Port: targetEnvironment, report: func() {
		f.seed(t, func(u *store.UnitOfWork) error {
			budget, err := store.GetBudgetForUpdate(f.ctx, u, f.tenant, submission.Budget.ID)
			if err != nil {
				return err
			}
			reservation, err := store.GetReservationForUpdate(f.ctx, u, f.tenant, submission.Reservation.ID)
			if err != nil {
				return err
			}
			outboxID, err := ids.New[ids.Any]()
			if err != nil {
				return err
			}
			_, _, _, err = store.ReportUsage(f.ctx, u, f.tenant, budget.ID, reservation.ID, reservation.Amount, store.Transition{
				ExpectedVersion: budget.Version, At: f.at.Add(2 * time.Minute), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: f.principal},
				AuditID: mustID[ids.Audit](t), Outbox: &store.OutboxPayload{ID: outboxID, Body: []byte(`{"usage":"migration-late-source"}`)},
			})
			return err
		})
	}}
	var workerLog bytes.Buffer
	droppingTargetStop := &dropFirstMigrationStopReply{Port: provision}
	droppingSourceStop := &dropFirstMigrationStopReply{Port: sourceEnvironment}
	checkedLostCleanupReply := false
	checkedLostSourceCleanupReply := false
	for pass := range 10 {
		if pass > 0 {
			if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
		}
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(&workerLog, nil)), nil, worker.DefaultConfig(), fmt.Sprintf("migration-budget-recheck-%d", pass),
			wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: sourceRuntime, cmd.To: targetRuntime},
				wiretest.Environments(map[ids.ProviderID]contracts.Port{f.environmentProvider: droppingSourceStop, cmd.To: droppingTargetStop})))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.ReapClaims(f.ctx); err != nil {
			t.Fatal(err)
		}
		if droppingTargetStop.dropped && !checkedLostCleanupReply {
			checkedLostCleanupReply = true
			items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, view.Pass.ID)
			if err != nil || len(items) != 1 {
				t.Fatalf("items after lost target cleanup reply=%+v err=%v", items, err)
			}
			pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, view.Pass.ID)
			if err != nil || pass.State != lifecycle.MigrationExecuting {
				t.Fatalf("pass after unconfirmed target cleanup=%s err=%v; slot must remain owned", pass.State, err)
			}
			slotUOW, err := store.Begin(f.ctx, f.pool)
			if err != nil {
				t.Fatal(err)
			}
			slot, err := store.GetMigrationSlot(f.ctx, slotUOW, f.tenant)
			_ = slotUOW.Rollback(f.ctx)
			if err != nil || slot.Active != view.Pass.ID {
				t.Fatalf("slot after unconfirmed target cleanup=%+v err=%v; want active pass", slot, err)
			}
			targetCleanup, err := store.GetOperation(f.ctx, f.pool, f.tenant, items[0].TargetCleanupOperation)
			if err != nil || targetCleanup.State != lifecycle.OperationUncertain {
				t.Fatalf("target cleanup operation=%s err=%v; want uncertain", targetCleanup.State, err)
			}
			if sourceEnvironment.Ledger().Effects(items[0].CleanupOperation) != 0 || targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation) != 1 {
				t.Fatalf("cleanup effects before exact replay source=%d target=%d", sourceEnvironment.Ledger().Effects(items[0].CleanupOperation), targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation))
			}
		}
		if droppingSourceStop.dropped && !checkedLostSourceCleanupReply {
			checkedLostSourceCleanupReply = true
			items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, view.Pass.ID)
			if err != nil || len(items) != 1 {
				t.Fatalf("items after lost source cleanup reply=%+v err=%v", items, err)
			}
			pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, view.Pass.ID)
			if err != nil || pass.State != lifecycle.MigrationExecuting {
				t.Fatalf("pass after unconfirmed source cleanup=%s err=%v; slot must remain owned", pass.State, err)
			}
			sourceCleanup, err := store.GetOperation(f.ctx, f.pool, f.tenant, items[0].CleanupOperation)
			if err != nil || sourceCleanup.State != lifecycle.OperationUncertain {
				t.Fatalf("source cleanup operation=%s err=%v; want uncertain", sourceCleanup.State, err)
			}
			if sourceEnvironment.Ledger().Effects(items[0].CleanupOperation) != 1 || targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation) != 1 {
				t.Fatalf("cleanup effects before exact source replay source=%d target=%d", sourceEnvironment.Ledger().Effects(items[0].CleanupOperation), targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation))
			}
		}
	}
	items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || len(items) != 1 || items[0].State != lifecycle.MigrationItemFailed {
		budget, budgetErr := store.GetBudget(f.ctx, f.pool, f.tenant, submission.Budget.ID)
		t.Fatalf("migration items=%+v err=%v budget=%+v budgetErr=%v target sends=%+v worker=%s; want failed migration after late usage", items, err, budget, budgetErr, targetRuntime.Ledger().Sent(), workerLog.String())
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, view.Pass.ID)
	if err != nil || pass.State != lifecycle.MigrationFailed {
		t.Fatalf("migration pass=%s err=%v; want failed", pass.State, err)
	}
	for _, request := range targetRuntime.Ledger().Sent() {
		if request.Method == contracts.MethodStartTurn {
			t.Fatal("target runtime started after its budget coverage was lost")
		}
	}
	if sourceEnvironment.Ledger().Effects(items[0].CleanupOperation) != 1 || targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation) != 1 {
		t.Fatalf("budget-abort cleanup effects source=%d target=%d; want one confirmed stop each", sourceEnvironment.Ledger().Effects(items[0].CleanupOperation), targetEnvironment.Ledger().Effects(items[0].TargetCleanupOperation))
	}
	if targetEnvironment.Ledger().Deliveries(items[0].TargetCleanupOperation) != 2 {
		t.Fatalf("target cleanup deliveries=%d; want lost-reply replay under stable operation", targetEnvironment.Ledger().Deliveries(items[0].TargetCleanupOperation))
	}
	if sourceEnvironment.Ledger().Deliveries(items[0].CleanupOperation) != 2 {
		t.Fatalf("source cleanup deliveries=%d; want lost-reply replay under stable operation", sourceEnvironment.Ledger().Deliveries(items[0].CleanupOperation))
	}
	reservationID, err := ids.Parse[ids.Reservation](items[0].TargetAttempt.String())
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.GetReservation(f.ctx, f.pool, f.tenant, reservationID)
	if err != nil || reservation.State != lifecycle.ReservationReleased {
		t.Fatalf("failed target reservation=%+v err=%v; want released after cleanup", reservation, err)
	}
}

func TestEmptyMigrationCompletesAndReleasesTenantSlot(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "empty-migration-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "empty-migration-execute")
	w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "empty-migration")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil || pass.State != lifecycle.MigrationCompleted {
		t.Fatalf("empty pass=%+v err=%v", pass, err)
	}
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := store.GetMigrationSlot(f.ctx, u, f.tenant)
	_ = u.Rollback(f.ctx)
	if err != nil || !slot.Active.IsZero() {
		t.Fatalf("empty pass retained slot=%+v err=%v", slot, err)
	}
}

func TestExecutingMigrationCannotBeCancelledWithoutCleanupEvidence(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "migration-cancel-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "migration-cancel-execute")
	_, err = application.AdministerMigration(f.ctx, f.pool, application.MigrationAdministration{Command: application.Command{Actor: f.actor, Nonce: "migration-cancel-executing", ExpectedVersion: v.Pass.Version}, Migration: v.Pass.ID, Event: lifecycle.MigrationCancel})
	if err == nil || !strings.Contains(err.Error(), "cannot be cancelled") {
		t.Fatalf("cancel executing pass=%v; want explicit safe refusal", err)
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil || pass.State != lifecycle.MigrationExecuting {
		t.Fatalf("pass after refused cancellation=%+v %v", pass, err)
	}
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := store.GetMigrationSlot(f.ctx, u, f.tenant)
	_ = u.Rollback(f.ctx)
	if err != nil || slot.Active != v.Pass.ID {
		t.Fatalf("migration slot after refused cancellation=%+v %v", slot, err)
	}
}

func TestMigrationExecutesQuiescePrepareCutoverAndCleanup(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	s := f.submit(t, f.submitCommand("migration-e2e-source"))
	sourceRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{s.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}, MigrationReceipts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	droppingRuntime := &dropFirstMigrationReply{Port: sourceRuntime}
	f.tick(t, sourceRuntime)
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "source-session"}),
	)
	if got := f.ingest(t, f.event(sourceRuntime, s, 1, frame)); got.Applied != 1 {
		t.Fatalf("source binding ingestion=%+v", got)
	}
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "migration-e2e-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "migration-e2e-execute")
	targetRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.RuntimeScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	targetEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	corruptingEnvironment := &corruptFirstMigrationReceipt{Port: targetEnvironment}
	var workerLog bytes.Buffer
	for passIndex := range 6 {
		if passIndex > 0 {
			if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
		}
		restartedPool, err := store.Open(f.ctx, f.pool.Config().ConnConfig.ConnString())
		if err != nil {
			t.Fatal(err)
		}
		w, err := worker.New(restartedPool, slog.New(slog.NewTextHandler(&workerLog, nil)), nil, worker.DefaultConfig(), "migration-e2e",
			wiretest.Ports(t, restartedPool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: droppingRuntime, cmd.To: targetRuntime},
				wiretest.Environments(map[ids.ProviderID]contracts.Port{f.environmentProvider: sourceEnvironment, cmd.To: corruptingEnvironment})))
		if err != nil {
			restartedPool.Close()
			t.Fatal(err)
		}
		corruptedBefore := corruptingEnvironment.corrupted
		reapErr := w.ReapClaims(f.ctx)
		restartedPool.Close()
		if !corruptedBefore && corruptingEnvironment.corrupted {
			items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
			if err != nil || len(items) != 1 {
				t.Fatalf("migration item after malformed target receipt=%+v err=%v", items, err)
			}
			op, err := store.GetOperation(f.ctx, f.pool, f.tenant, items[0].EnvironmentOperation)
			if err != nil || op.State != lifecycle.OperationUncertain {
				t.Fatalf("malformed receipt operation=%s err=%v; want uncertain", op.State, err)
			}
			bindings, err := store.ListAttemptBindings(f.ctx, f.pool, f.tenant, items[0].TargetAttempt)
			if err != nil || len(bindings) != 0 {
				t.Fatalf("malformed receipt admitted target bindings=%+v err=%v", bindings, err)
			}
		} else if reapErr != nil {
			t.Fatal(reapErr)
		}
		pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
		if err != nil {
			t.Fatal(err)
		}
		if pass.State == lifecycle.MigrationExecuting {
			_, cancelErr := application.AdministerMigration(f.ctx, f.pool, application.MigrationAdministration{
				Command:   application.Command{Actor: f.actor, Nonce: fmt.Sprintf("cancel-stage-%d", passIndex), ExpectedVersion: pass.Version},
				Migration: pass.ID, Event: lifecycle.MigrationCancel,
			})
			if cancelErr == nil || !strings.Contains(cancelErr.Error(), "cannot be cancelled") {
				t.Fatalf("cancel during migration stage %d = %v; want safe refusal", passIndex, cancelErr)
			}
		}
	}
	pass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pass.State != lifecycle.MigrationCompleted || len(items) != 1 || items[0].State != lifecycle.MigrationItemCompleted {
		t.Fatalf("migration=%s items=%+v; want completed pass and item; worker log: %s; source sent=%+v effects=%+v", pass.State, items, workerLog.String(), sourceRuntime.Ledger().Sent(), sourceRuntime.Ledger().Applied())
	}
	if items[0].QuiesceEpoch != 1 || items[0].EnvironmentEpoch != 3 || items[0].RestartEpoch != 5 || items[0].CleanupEpoch != 6 || items[0].ClaimEpoch != 6 {
		t.Fatalf("migration stage fences=%d/%d/%d/%d current=%d; want each original send epoch retained across takeover", items[0].QuiesceEpoch, items[0].EnvironmentEpoch, items[0].RestartEpoch, items[0].CleanupEpoch, items[0].ClaimEpoch)
	}
	source, err := store.GetAttempt(f.ctx, f.pool, f.tenant, s.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.GetAttempt(f.ctx, f.pool, f.tenant, items[0].TargetAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if source.State != lifecycle.AttemptMigrated || target.State != lifecycle.AttemptExecuting {
		t.Fatalf("attempt states source/target=%s/%s", source.State, target.State)
	}
	targetBindings, err := store.ListAttemptBindings(f.ctx, f.pool, f.tenant, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var hasRuntime, hasEnvironment bool
	for _, binding := range targetBindings {
		hasRuntime = hasRuntime || binding.Kind == lifecycle.ResourceRuntimeSession
		hasEnvironment = hasEnvironment || binding.Kind == lifecycle.ResourceEnvironmentResource
	}
	if !hasRuntime || !hasEnvironment {
		t.Fatalf("target bindings=%+v; want runtime session and environment resource", targetBindings)
	}
	if sourceRuntime.Ledger().Effects(items[0].QuiesceOperation) != 1 || targetEnvironment.Ledger().Effects(items[0].EnvironmentOperation) != 1 || targetRuntime.Ledger().Effects(items[0].RestartOperation) != 1 || sourceEnvironment.Ledger().Effects(items[0].CleanupOperation) != 1 {
		t.Fatalf("migration effects source-quiesce=%d target-env=%d target-runtime=%d source-cleanup=%d", sourceRuntime.Ledger().Effects(items[0].QuiesceOperation), targetEnvironment.Ledger().Effects(items[0].EnvironmentOperation), targetRuntime.Ledger().Effects(items[0].RestartOperation), sourceEnvironment.Ledger().Effects(items[0].CleanupOperation))
	}
	if sourceRuntime.Ledger().Deliveries(items[0].QuiesceOperation) != 2 {
		t.Fatalf("quiesce deliveries=%d; want lost-reply replay under stable operation identity", sourceRuntime.Ledger().Deliveries(items[0].QuiesceOperation))
	}
	runtimeOperation, err := store.GetOperationByAttempt(f.ctx, f.pool, f.tenant, target.ID)
	if err != nil || runtimeOperation.ID != items[0].RestartOperation {
		t.Fatalf("target runtime operation=%s err=%v; want migration restart operation %s", runtimeOperation.ID, err, items[0].RestartOperation)
	}
	var restart contracts.RuntimeStart
	for _, request := range targetRuntime.Ledger().Sent() {
		if request.Operation == items[0].RestartOperation {
			if err := json.Unmarshal(request.Arguments, &restart); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	wantBrief, present := s.Task.Brief()
	if !present || restart.Recovery == nil || restart.Recovery.Stage != contracts.MigrationTargetStarted || restart.Brief.Objective != wantBrief.Objective || !slices.Equal(restart.Brief.AcceptanceCriteria, wantBrief.AcceptanceCriteria) {
		t.Fatalf("migration runtime start=%+v, want original brief plus target recovery", restart)
	}
	repeat := cmd
	repeat.Command.Nonce = "migration-repeat-source-pass"
	repeatView, err := application.CreateMigration(f.ctx, f.pool, repeat)
	if err != nil {
		t.Fatal(err)
	}
	repeatView = migrate(t, f, repeatView, lifecycle.MigrationPreview, "migration-repeat-source-preview")
	repeatView = migrate(t, f, repeatView, lifecycle.MigrationExecute, "migration-repeat-source-execute")
	if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	workerInstance, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "migration-repeat-source")
	if err != nil {
		t.Fatal(err)
	}
	if err := workerInstance.ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	repeatPass, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, repeatView.Pass.ID)
	repeatItems, listErr := store.ListMigrationItems(f.ctx, f.pool, f.tenant, repeatView.Pass.ID)
	if err != nil || listErr != nil || repeatPass.State != lifecycle.MigrationCompleted {
		t.Fatalf("repeat source pass=%s items=%+v err=%v listErr=%v; want empty pass completed without enrolling migrated source", repeatPass.State, repeatItems, err, listErr)
	}
}

func TestMigrationStaleWorkerCannotSettleAfterTakeover(t *testing.T) {
	t.Parallel()
	f, cmd := migrationFixture(t)
	s := f.submit(t, f.submitCommand("migration-stale-worker-source"))
	runtime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: f.provider}, simulator.RuntimeScenario{
		Operations: map[ids.OperationID]simulator.RuntimeOp{s.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}, MigrationReceipts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, runtime)
	frame := simulator.RowsFrame(false,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "stale-worker-session"}),
	)
	if got := f.ingest(t, f.event(runtime, s, 1, frame)); got.Applied != 1 {
		t.Fatalf("source bindings=%+v", got)
	}
	v, err := application.CreateMigration(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	v = migrate(t, f, v, lifecycle.MigrationPreview, "migration-stale-preview")
	v = migrate(t, f, v, lifecycle.MigrationExecute, "migration-stale-execute")
	targetRuntime, err := simulator.NewRuntime(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.RuntimeScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: f.environmentProvider}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	targetEnvironment, err := simulator.NewEnvironment(simulator.Config{Tenant: f.tenant, Provider: cmd.To}, simulator.EnvironmentScenario{MigrationReceipts: true})
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockFirstMigrationReply{Port: runtime, started: make(chan struct{}), release: make(chan struct{})}
	newWorker := func(owner string, source contracts.Port) *worker.Worker {
		t.Helper()
		w, err := worker.New(f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), owner,
			wiretest.Ports(t, f.pool, f.tenant, map[ids.ProviderID]contracts.Port{f.provider: source, cmd.To: targetRuntime},
				wiretest.Environments(map[ids.ProviderID]contracts.Port{f.environmentProvider: sourceEnvironment, cmd.To: targetEnvironment})))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- newWorker("migration-old", blocked).ReapClaims(f.ctx) }()
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old worker did not reach the post-send boundary")
	}
	if _, err := f.pool.Exec(f.ctx, `update maintenance_claim set expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1`, f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if err := newWorker("migration-new", runtime).ReapClaims(f.ctx); err != nil {
		t.Fatal(err)
	}
	passBefore, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	itemsBefore, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil || len(itemsBefore) != 1 || itemsBefore[0].State != lifecycle.MigrationItemStarting {
		t.Fatalf("winner state=%+v err=%v", itemsBefore, err)
	}
	countBefore, auditBefore := f.count(t, "attempt", "true"), f.count(t, "audit_event", "true")
	close(blocked.release)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	passAfter, err := store.GetMigrationPass(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	itemsAfter, err := store.ListMigrationItems(f.ctx, f.pool, f.tenant, v.Pass.ID)
	if err != nil {
		t.Fatal(err)
	}
	if passAfter != passBefore || len(itemsAfter) != 1 || !reflect.DeepEqual(itemsAfter[0], itemsBefore[0]) || f.count(t, "attempt", "true") != countBefore || f.count(t, "audit_event", "true") != auditBefore {
		t.Fatalf("stale worker changed durable winner: pass %s->%s items %+v->%+v attempts=%d->%d audit=%d->%d", passBefore.State, passAfter.State, itemsBefore, itemsAfter, countBefore, f.count(t, "attempt", "true"), auditBefore, f.count(t, "audit_event", "true"))
	}
}

type blockFirstMigrationReply struct {
	contracts.Port
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockFirstMigrationReply) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if request.Method == contracts.MethodInterrupt && err == nil {
		blocked := false
		p.once.Do(func() { blocked = true; close(p.started) })
		if blocked {
			select {
			case <-p.release:
			case <-ctx.Done():
				return contracts.Reply{}, ctx.Err()
			}
		}
	}
	return reply, err
}

type dropFirstMigrationReply struct {
	contracts.Port
	once sync.Once
}

type dropFirstMigrationStopReply struct {
	contracts.Port
	once    sync.Once
	dropped bool
}

func (p *dropFirstMigrationStopReply) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if request.Method == contracts.MethodStop && err == nil {
		var dropped bool
		p.once.Do(func() { dropped = true })
		if dropped {
			p.dropped = true
			return contracts.Reply{}, errors.New("simulated lost cleanup reply")
		}
	}
	return reply, err
}

type corruptFirstMigrationReceipt struct {
	contracts.Port
	once      sync.Once
	corrupted bool
}

func (p *corruptFirstMigrationReceipt) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if err != nil {
		return reply, err
	}
	corrupt := false
	p.once.Do(func() { corrupt = true })
	if !corrupt {
		return reply, nil
	}
	p.corrupted = true
	reply.Result = json.RawMessage(`{}`)
	return reply, nil
}

func (p *dropFirstMigrationReply) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := p.Port.Send(ctx, request)
	if request.Method == contracts.MethodInterrupt && err == nil {
		var dropped bool
		p.once.Do(func() { dropped = true })
		if dropped {
			return contracts.Reply{}, errors.New("simulated lost migration reply")
		}
	}
	return reply, err
}

func TestSecurityMigrationDowngradePreservesAdministration(t *testing.T) {
	t.Parallel()
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
