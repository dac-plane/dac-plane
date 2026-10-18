//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

func TestPersistenceCommandRecordsReplayAndScope(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	at := time.Now().UTC().Truncate(time.Microsecond)
	record := store.CommandRecord{
		Tenant: f.tenant, Principal: f.principal, Command: "submit_task", Nonce: "n-1",
		RequestDigest: identity.DigestOf([]byte("request")), CreatedAt: at,
	}

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	inserted, err := store.InsertCommandRecord(ctx, uow, record)
	if err != nil || !inserted {
		t.Fatalf("InsertCommandRecord() = %v, %v", inserted, err)
	}
	if err := store.SetCommandResult(ctx, uow, f.tenant, f.principal, "submit_task", "n-1", json.RawMessage(`{"task":"t"}`)); err != nil {
		t.Fatalf("SetCommandResult() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	stored, err := store.GetCommandRecord(ctx, pool, f.tenant, f.principal, "submit_task", "n-1")
	if err != nil {
		t.Fatalf("GetCommandRecord() error = %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(stored.Result, &result); err != nil || result["task"] != "t" || stored.RequestDigest != record.RequestDigest {
		t.Fatalf("stored record = %+v, %v", stored, err)
	}

	// A second insert of the committed key reports a conflict without
	// failing the transaction, so the caller can roll back and replay.
	uow, err = store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = uow.Rollback(ctx) }()
	if inserted, err := store.InsertCommandRecord(ctx, uow, record); err != nil || inserted {
		t.Fatalf("duplicate InsertCommandRecord() = %v, %v; want conflict", inserted, err)
	}
	if err := store.SetCommandResult(ctx, uow, f.tenant, f.principal, "submit_task", "absent", json.RawMessage(`{}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetCommandResult(absent) = %v, want ErrNotFound", err)
	}

	for name, read := range map[string]func() error{
		"other principal": func() error {
			_, err := store.GetCommandRecord(ctx, pool, f.tenant, mustID[ids.Principal](t), "submit_task", "n-1")
			return err
		},
		"other command": func() error {
			_, err := store.GetCommandRecord(ctx, pool, f.tenant, f.principal, "assert_spend", "n-1")
			return err
		},
		"other tenant": func() error {
			_, err := store.GetCommandRecord(ctx, pool, mustID[ids.Tenant](t), f.principal, "submit_task", "n-1")
			return err
		},
	} {
		if err := read(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: GetCommandRecord() = %v, want ErrNotFound", name, err)
		}
	}
}

func TestPersistenceRecordRefusalSurvivesRollback(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	at := time.Now().UTC()

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	task := createTask(t, ctx, uow, f, at)
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// A multi-write command refuses a step and rolls back: the step's own
	// refusal row goes with it.
	uow, err = store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	auditID := newAuditID(t)
	_, _, applyErr := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskAccept, "", store.Transition{
		ExpectedVersion: task.Version, At: at, Actor: f.actor, AuditID: auditID,
	})
	var refusal *lifecycle.TransitionError
	if !errors.As(applyErr, &refusal) {
		t.Fatalf("ApplyTask(accept on draft) = %v, want a refusal", applyErr)
	}
	if err := uow.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	where := "tenant_id = $1 and outcome = 'rejected'"
	if got := countRows(t, ctx, pool, "audit_event", where, f.tenant.String()); got != 0 {
		t.Fatalf("refusal rows after rollback = %d, want 0", got)
	}

	if err := store.RecordRefusal(ctx, pool, refusal.Audit(auditID, f.actor, at)); err != nil {
		t.Fatalf("RecordRefusal() error = %v", err)
	}
	if got := countRows(t, ctx, pool, "audit_event", where, f.tenant.String()); got != 1 {
		t.Fatalf("refusal rows after RecordRefusal = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, "outbox", "tenant_id = $1", f.tenant.String()); got != 0 {
		t.Fatalf("outbox rows after refusal = %d, want 0", got)
	}

	applied := lifecycle.Change{
		Tenant: f.tenant, Record: lifecycle.RecordTask, RecordID: task.ID.Untyped(),
		Event: "submit", From: "draft", To: "queued", Version: 2,
	}.Audit(newAuditID(t), f.actor, at)
	if err := store.RecordRefusal(ctx, pool, applied); err == nil {
		t.Fatal("RecordRefusal(applied change) succeeded")
	}
}

func TestPersistenceGetOutboxRowReportsStatus(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	at := time.Now().UTC()

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	task := createTask(t, ctx, uow, f, at)
	attempt := createAttempt(t, ctx, uow, f, task, 1, at)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, at)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	outbox := &store.OutboxPayload{ID: mustID[ids.Any](t), Attempt: attempt.ID, Body: []byte(`{"dispatch":"test"}`)}
	if _, _, err := store.ApplyOperation(ctx, uow, f.tenant, operation.ID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, store.Transition{
		ExpectedVersion: operation.Version, At: at, Actor: f.actor, AuditID: newAuditID(t), Outbox: outbox,
	}); err != nil {
		t.Fatalf("ApplyOperation() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	row, err := store.GetOutboxRow(ctx, pool, f.tenant, outbox.ID)
	if err != nil || row.Status != "pending" || !row.ExhaustedAt.IsZero() || row.RecordID.String() != operation.ID.String() {
		t.Fatalf("GetOutboxRow() = %+v, %v; want pending", row, err)
	}

	uow, err = store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	claim, err := store.ClaimOutboxRowByID(ctx, uow, f.tenant, outbox.ID, "owner", time.Minute, at)
	if err != nil {
		t.Fatalf("ClaimOutboxRowByID() error = %v", err)
	}
	if err := store.ExhaustOutboxRow(ctx, uow, f.tenant, outbox.ID, claim.Epoch, "harness_not_live", at); err != nil {
		t.Fatalf("ExhaustOutboxRow() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	row, err = store.GetOutboxRow(ctx, pool, f.tenant, outbox.ID)
	if err != nil || row.ExhaustedAt.IsZero() || row.LastError != "harness_not_live" {
		t.Fatalf("GetOutboxRow() after exhaustion = %+v, %v", row, err)
	}
	if _, err := store.GetOutboxRow(ctx, pool, mustID[ids.Tenant](t), outbox.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOutboxRow(other tenant) = %v, want ErrNotFound", err)
	}
}
