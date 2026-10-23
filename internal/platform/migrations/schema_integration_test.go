//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func openMigrated(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testpostgres.Start(t))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close() error = %v", err)
		}
	})
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	// No provider.Close: it would close db, which the caller still needs.
	// t.Cleanup above owns the connection lifetime.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("GetDBVersion() error = %v", err)
	}
	if version != migrations.CurrentVersion {
		t.Fatalf("GetDBVersion() = %d, want %d", version, migrations.CurrentVersion)
	}
	return db
}

func TestMigrationFreshAppliesAndReapplyIsNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", testpostgres.Start(t))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	// No provider.Close here: it would close db before the assertions below.
	first, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if len(first) == 0 {
		t.Fatal("Up() applied no migrations on a fresh database")
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("GetDBVersion() error = %v", err)
	}
	if version != migrations.CurrentVersion {
		t.Fatalf("GetDBVersion() = %d, want %d", version, migrations.CurrentVersion)
	}

	second, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("second Up() error = %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second Up() applied %d migrations, want a no-op", len(second))
	}

	var outboxIndex string
	if err := db.QueryRowContext(ctx, `
		select indexname from pg_indexes where schemaname = 'public' and indexname = 'outbox_pending'
	`).Scan(&outboxIndex); err != nil {
		t.Fatalf("outbox_pending index is not observable: %v", err)
	}
}

func TestMigrationDownThenUpRestoresSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	for v := migrations.CurrentVersion; v >= 1; v-- {
		if _, err := provider.ApplyVersion(ctx, v, false); err != nil {
			t.Fatalf("ApplyVersion(%d, down) error = %v", v, err)
		}
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("GetDBVersion() error = %v", err)
	}
	if version != 0 {
		t.Fatalf("GetDBVersion() after down = %d, want 0", version)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `
		select count(*) from pg_catalog.pg_tables
		where schemaname = 'public' and tablename in ('task', 'outbox', 'audit_event')
	`).Scan(&remaining); err != nil {
		t.Fatalf("count product tables after down: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("product tables after down = %d, want 0", remaining)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up() after down error = %v", err)
	}
	version, err = provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("GetDBVersion() error = %v", err)
	}
	if version != migrations.CurrentVersion {
		t.Fatalf("GetDBVersion() after re-up = %d, want %d", version, migrations.CurrentVersion)
	}
}

func TestMigrationRefusesToDropHookStageEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	if _, err := db.ExecContext(ctx, `insert into hook_stage_evidence (tenant_id,operation_id,kind,input_digest,output_digest,duration_ns,response_digest,usage) values ($1,$2,'post_action',$3,$4,1,$5,'[]'::jsonb)`, f.tenant, f.operation, digest(), digest(), digest()); err != nil {
		t.Fatalf("insert hook stage evidence: %v", err)
	}
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.ApplyVersion(ctx, 9, false); err == nil || !strings.Contains(err.Error(), "cannot downgrade while enriched hook evidence exists") {
		t.Fatalf("ApplyVersion(9, down) error = %v, want data-preserving refusal", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil || version != migrations.CurrentVersion {
		t.Fatalf("version after refused downgrade = %d, %v", version, err)
	}
}

func TestMigrationRefusesToDropActorIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	if _, err := db.ExecContext(ctx, `insert into identity_binding (issuer, subject, tenant_id, principal_id, enabled, updated_at) values ('https://issuer.example', 'user-1', $1, $2, true, now())`, f.tenant, f.principal); err != nil {
		t.Fatalf("insert identity binding: %v", err)
	}
	for _, bad := range []struct{ issuer, subject string }{{"http://issuer.example", "user-1"}, {"https://issuer.example/?q", "user-1"}, {"https://issuer.example", "user 1"}} {
		if _, err := db.ExecContext(ctx, `insert into identity_binding (issuer, subject, tenant_id, principal_id, enabled, updated_at) values ($1, $2, $3, $4, true, now())`, bad.issuer, bad.subject, f.tenant, f.principal); err == nil {
			t.Fatalf("schema accepted binding %q %q", bad.issuer, bad.subject)
		}
	}
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.ApplyVersion(ctx, 10, false); err == nil || !strings.Contains(err.Error(), "cannot downgrade while version 10 actor identity exists") {
		t.Fatalf("ApplyVersion(10, down) error = %v, want identity-preserving refusal", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil || version != migrations.CurrentVersion {
		t.Fatalf("version after refused downgrade = %d, %v", version, err)
	}
}

type fixture struct {
	tenant    string
	principal string
	task      string
	attempt   string
	operation string
	provider  string
}

func mustID[K any](t *testing.T) string {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id.String()
}

func seedCore(t *testing.T, ctx context.Context, db *sql.DB) fixture {
	t.Helper()
	f := fixture{
		tenant:    mustID[ids.Tenant](t),
		principal: mustID[ids.Principal](t),
		task:      mustID[ids.Task](t),
		attempt:   mustID[ids.Attempt](t),
		operation: mustID[ids.Operation](t),
		provider:  mustID[ids.Provider](t),
	}
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`insert into tenant (id) values ($1)`, f.tenant)
	exec(`insert into principal (tenant_id, principal_id, kind) values ($1, $2, 'human')`, f.tenant, f.principal)
	exec(`insert into policy (tenant_id, generation) values ($1, 1)`, f.tenant)
	exec(`insert into task (tenant_id, task_id, owner_id, state, version, created_at, updated_at)
		values ($1, $2, $3, 'queued', 1, $4, $4)`, f.tenant, f.task, f.principal, now)
	exec(`insert into attempt (tenant_id, attempt_id, task_id, number, state, version, created_at, updated_at)
		values ($1, $2, $3, 1, 'executing', 1, $4, $4)`, f.tenant, f.attempt, f.task, now)
	exec(`insert into provider_operation (tenant_id, operation_id, attempt_id, provider_id, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, 'dispatched', 1, $5, $5)`, f.tenant, f.operation, f.attempt, f.provider, now)
	return f
}

func digest() string {
	return string(identity.DigestOf([]byte("fixture-bytes")))
}

func TestMigrationPreventsRetiredOneTimeAuthorityReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	now := time.Now().UTC()
	grant := mustID[ids.Grant](t)
	if _, err := db.ExecContext(ctx, `
		insert into authority_grant (tenant_id, grant_id, attempt_id, provider_id, action, mode, operation_id, input_digest, granted_by, policy_generation, issued_at, expires_at, retired_at)
		values ($1, $2, $3, $4, 'fs.write', 'allow_once', $5, $6, $7, 1, $8, $9, $9)`,
		f.tenant, grant, f.attempt, f.provider, f.operation, digest(), f.principal, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("insert retired grant: %v", err)
	}
	use := func(decidedAt time.Time) error {
		_, err := db.ExecContext(ctx, `
			insert into authority_use (tenant_id, operation_id, attempt_id, provider_id, action, input_digest, grant_id, source, granted_by, outcome, policy_generation, decided_at)
			values ($1, $2, $3, $4, 'fs.write', $5, $6, 'one_time_approval', $7, 'allowed', 1, $8)`,
			f.tenant, f.operation, f.attempt, f.provider, digest(), grant, f.principal, decidedAt)
		return err
	}
	if err := use(now); err != nil {
		t.Fatalf("first allowed use: %v", err)
	}
	if err := use(now.Add(time.Second)); err == nil {
		t.Fatal("second allowed use of a retired one-time grant succeeded")
	} else if !strings.Contains(err.Error(), "authority_use_one_time_single_allow") {
		t.Fatalf("second allowed use error = %v, want the one-time uniqueness violation", err)
	}
}

func TestMigrationPreventsStaleGrantGenerations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `update policy set generation = 2 where tenant_id = $1`, f.tenant); err != nil {
		t.Fatalf("advance policy generation: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		insert into authority_grant (tenant_id, grant_id, attempt_id, provider_id, action, mode, operation_id, input_digest, granted_by, policy_generation, issued_at, expires_at)
		values ($1, $2, $3, $4, 'fs.write', 'allow_once', $5, $6, $7, 1, $8, $9)`,
		f.tenant, mustID[ids.Grant](t), f.attempt, f.provider, f.operation, digest(), f.principal, now, now.Add(time.Hour))
	if err == nil {
		t.Fatal("grant at a stale policy generation succeeded")
	} else if !strings.Contains(err.Error(), "stale grant generation") {
		t.Fatalf("stale grant error = %v, want the generation trigger refusal", err)
	}
}

func TestMigrationScopesCursorsAndBindingsToTenantAndAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	otherTenant := mustID[ids.Tenant](t)
	if _, err := db.ExecContext(ctx, `insert into tenant (id) values ($1)`, otherTenant); err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		insert into stream_cursor (tenant_id, attempt_id, operation_id, stream, generation, after_updated_sequence)
		values ($1, $2, $3, 'transcript', 1, 0)`, otherTenant, f.attempt, f.operation); err == nil {
		t.Fatal("cursor bound to another tenant's attempt succeeded")
	}
	if _, err := db.ExecContext(ctx, `
		insert into stream_cursor (tenant_id, attempt_id, operation_id, stream, generation, after_updated_sequence)
		values ($1, $2, $3, 'transcript', 1, 0)`, f.tenant, f.attempt, f.operation); err != nil {
		t.Fatalf("tenant-scoped cursor insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		insert into provider_operation (tenant_id, operation_id, attempt_id, provider_id, state, version, created_at, updated_at)
		values ($1, $2, $3, $4, 'dispatched', 1, now(), now())`,
		f.tenant, f.operation, f.attempt, f.provider); err == nil {
		t.Fatal("duplicate operation identity succeeded")
	}
}

func TestMigrationEnforcesExpiryAndFences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `
		insert into spend_challenge (tenant_id, spend_id, principal_id, task_id, attempt_id, operation_id, amount, unit, nonce, state, version, expires_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, 10, 'tokens', $7, 'pending', 1, $8, $8, $8)`,
		f.tenant, mustID[ids.Spend](t), f.principal, f.task, f.attempt, f.operation, digest(), now); err == nil {
		t.Fatal("spend challenge with no expiry window succeeded")
	}
	claim := mustID[ids.Claim](t)
	if _, err := db.ExecContext(ctx, `
		insert into maintenance_claim (tenant_id, claim_id, provider_id, resource_ref, state, version, updated_at)
		values ($1, $2, $3, 'worker-a', 'scheduled', 1, now())`,
		f.tenant, claim, f.provider); err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		insert into maintenance_claim (tenant_id, claim_id, provider_id, resource_ref, state, version, updated_at)
		values ($1, $2, $3, 'worker-a', 'scheduled', 1, now())`,
		f.tenant, mustID[ids.Claim](t), f.provider); err == nil {
		t.Fatal("second claim row for the same resource succeeded")
	}
	audit := mustID[ids.Audit](t)
	if _, err := db.ExecContext(ctx, `
		insert into audit_event (tenant_id, audit_id, record_kind, record_id, actor_kind, principal_id, event, from_state, to_state, version, outcome, occurred_at)
		values ($1, $2, 'task', $3, 'principal', $4, 'dispatch', 'queued', 'running', 2, 'applied', now())`,
		f.tenant, audit, f.task, f.principal); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	if _, err := db.ExecContext(ctx, `update audit_event set to_state = 'failed' where tenant_id = $1 and audit_id = $2`, f.tenant, audit); err == nil {
		t.Fatal("audit event mutation succeeded")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("audit mutation error = %v, want the immutability refusal", err)
	}
}

func TestMigrationDispatchColumnsAndShadowTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	now := time.Now().UTC()
	outbox := mustID[ids.Any](t)
	if _, err := db.ExecContext(ctx, `
		insert into outbox (tenant_id, outbox_id, attempt_id, record_kind, record_id, event, payload, demand)
		values ($1, $2, $3, 'attempt', $4, 'dispatch', '{}', 'person')`,
		f.tenant, outbox, f.attempt, f.attempt); err != nil {
		t.Fatalf("insert person-demand outbox row: %v", err)
	}
	var attempts int
	var demand string
	var lastErr *string
	var exhaustedAt *time.Time
	if err := db.QueryRowContext(ctx, `
		select attempts, demand, last_error, exhausted_at from outbox where tenant_id = $1 and outbox_id = $2`,
		f.tenant, outbox).Scan(&attempts, &demand, &lastErr, &exhaustedAt); err != nil {
		t.Fatalf("read dispatch defaults: %v", err)
	}
	if attempts != 0 || demand != "person" || lastErr != nil || exhaustedAt != nil {
		t.Fatalf("dispatch defaults = (%d, %q, %v, %v), want (0, person, nil, nil)", attempts, demand, lastErr, exhaustedAt)
	}
	if _, err := db.ExecContext(ctx, `update outbox set demand = 'bulk' where tenant_id = $1 and outbox_id = $2`,
		f.tenant, outbox); err == nil {
		t.Fatal("outbox row with unknown demand succeeded")
	}
	var readyIndex string
	if err := db.QueryRowContext(ctx, `
		select indexname from pg_indexes where schemaname = 'public' and indexname = 'outbox_ready'
	`).Scan(&readyIndex); err != nil {
		t.Fatalf("outbox_ready index is not observable: %v", err)
	}
	shadow := mustID[ids.Any](t)
	if _, err := db.ExecContext(ctx, `
		insert into shadow_result (tenant_id, shadow_id, attempt_id, captured_at, summary, comparison_summary, evidence_digest)
		values ($1, $2, $3, $4, 'shadow ran', 'matches live', $5)`,
		f.tenant, shadow, f.attempt, now, digest()); err != nil {
		t.Fatalf("insert shadow result: %v", err)
	}
	if _, err := db.ExecContext(ctx, `update shadow_result set summary = 'edited' where tenant_id = $1 and shadow_id = $2`,
		f.tenant, shadow); err == nil {
		t.Fatal("shadow result mutation succeeded")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("shadow mutation error = %v, want the immutability refusal", err)
	}
}

func TestMigrationCommandRecordScopesNoncesAndChecksShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openMigrated(t, ctx)
	f := seedCore(t, ctx, db)
	other := mustID[ids.Principal](t)
	if _, err := db.ExecContext(ctx, `insert into principal (tenant_id, principal_id, kind) values ($1, $2, 'service')`, f.tenant, other); err != nil {
		t.Fatalf("seed second principal: %v", err)
	}
	now := time.Now().UTC()
	insert := func(principal, command, nonce, digest, result string) error {
		_, err := db.ExecContext(ctx, `
			insert into command_record (tenant_id, principal_id, command, nonce, request_digest, result, created_at)
			values ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
			f.tenant, principal, command, nonce, digest, result, now)
		return err
	}
	if err := insert(f.principal, "submit_task", "n-1", digest(), `{}`); err != nil {
		t.Fatalf("insert command record: %v", err)
	}
	if err := insert(f.principal, "submit_task", "n-1", digest(), `{}`); err == nil {
		t.Fatal("duplicate (tenant, principal, command, nonce) succeeded")
	}
	// The same nonce is independent per principal and per command kind.
	if err := insert(other, "submit_task", "n-1", digest(), `{}`); err != nil {
		t.Fatalf("same nonce for another principal: %v", err)
	}
	if err := insert(f.principal, "assert_spend", "n-1", digest(), `{}`); err != nil {
		t.Fatalf("same nonce for another command: %v", err)
	}
	for name, args := range map[string][5]string{
		"empty nonce":       {f.principal, "submit_task", "", digest(), `{}`},
		"over-long nonce":   {f.principal, "submit_task", strings.Repeat("n", 129), digest(), `{}`},
		"empty command":     {f.principal, "", "n-2", digest(), `{}`},
		"malformed digest":  {f.principal, "submit_task", "n-3", "md5:abc", `{}`},
		"non-object result": {f.principal, "submit_task", "n-4", digest(), `[]`},
		"unknown principal": {mustID[ids.Principal](t), "submit_task", "n-5", digest(), `{}`},
	} {
		if err := insert(args[0], args[1], args[2], args[3], args[4]); err == nil {
			t.Errorf("%s: insert succeeded", name)
		}
	}
}
