//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// isStaleVersionForTest reports store.ErrStaleVersion without pulling the
// fencing tests into the store package internals.
func isStaleVersionForTest(err error) bool {
	return errors.Is(err, store.ErrStaleVersion)
}

// enqueueTaskSubmitWithDemand moves a fresh task draft -> queued with an
// outbox payload of the given demand and commits. It returns the outbox
// identity.
func enqueueTaskSubmitWithDemand(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f tenantFixture, at time.Time, demand contracts.Demand) ids.AnyID {
	t.Helper()
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, at)
	outboxID, err := ids.New[ids.Any]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	tr := store.Transition{
		ExpectedVersion: 1, At: at, Actor: f.actor, AuditID: newAuditID(t),
		Outbox: &store.OutboxPayload{ID: outboxID, Demand: demand, Body: []byte(`{"dispatch":"test"}`)},
	}
	if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskSubmit, lifecycle.TaskQueued, tr); err != nil {
		t.Fatalf("ApplyTask() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	return outboxID
}

func TestDispatchOutboxClaimLifecycle(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	outboxID := enqueueTaskSubmitWithDemand(t, ctx, pool, f, now, contracts.DemandPerson)
	pollAt := now.Add(time.Minute)

	ready, err := store.OutboxReadyEntries(ctx, pool, f.tenant, pollAt, 10)
	if err != nil {
		t.Fatalf("OutboxReadyEntries() error = %v", err)
	}
	if len(ready) != 1 || ready[0].ID != outboxID {
		t.Fatalf("OutboxReadyEntries() = %v, want the enqueued row", ready)
	}
	if ready[0].Demand != contracts.DemandPerson || ready[0].Attempts != 0 {
		t.Fatalf("OutboxReadyEntries() row = demand %q attempts %d, want person 0",
			ready[0].Demand, ready[0].Attempts)
	}

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	claim, claimed, err := store.ClaimOutboxRow(ctx, uow, f.tenant, "worker-a", time.Minute, pollAt)
	if err != nil || !claimed {
		t.Fatalf("ClaimOutboxRow() = %v, %v, %v", claim, claimed, err)
	}
	if claim.Row.ID != outboxID || claim.Epoch != 1 || claim.Owner != "worker-a" {
		t.Fatalf("ClaimOutboxRow() = %+v, want the row at epoch 1 owned by worker-a", claim)
	}
	// A rival worker sees nothing to claim while the row is processing.
	rival, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = rival.Rollback(ctx) }()
	_, rivalClaimed, err := store.ClaimOutboxRow(ctx, rival, f.tenant, "worker-b", time.Minute, pollAt)
	if err != nil {
		t.Fatalf("rival ClaimOutboxRow() error = %v", err)
	}
	if rivalClaimed {
		t.Fatal("rival ClaimOutboxRow() claimed a processing row")
	}
	// A stale epoch cannot acknowledge the dispatch.
	if err := store.AckOutboxRow(ctx, uow, f.tenant, outboxID, claim.Epoch+1, pollAt); err == nil {
		t.Fatal("AckOutboxRow() with a stale epoch succeeded")
	} else if !isStaleVersionForTest(err) {
		t.Fatalf("AckOutboxRow() stale error = %v, want ErrStaleVersion", err)
	}
	if err := store.AckOutboxRow(ctx, uow, f.tenant, outboxID, claim.Epoch, pollAt); err != nil {
		t.Fatalf("AckOutboxRow() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	ready, err = store.OutboxReadyEntries(ctx, pool, f.tenant, now, 10)
	if err != nil {
		t.Fatalf("OutboxReadyEntries() error = %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("OutboxReadyEntries() after ack = %d rows, want 0", len(ready))
	}
}

func TestDispatchOutboxRetryAndExhaust(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	outboxID := enqueueTaskSubmitWithDemand(t, ctx, pool, f, now, contracts.DemandPerson)

	claimOne := func(owner string, at time.Time) (store.OutboxClaim, *store.UnitOfWork, func()) {
		t.Helper()
		uow, err := store.Begin(ctx, pool)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		claim, claimed, err := store.ClaimOutboxRow(ctx, uow, f.tenant, owner, time.Minute, at)
		if err != nil || !claimed {
			_ = uow.Rollback(ctx)
			t.Fatalf("ClaimOutboxRow() = %v, %v, %v", claim, claimed, err)
		}
		return claim, uow, func() { _ = uow.Rollback(ctx) }
	}

	// Retry with a future not-before time: the row leaves the ready set.
	claim, uow, done := claimOne("worker-a", now.Add(time.Minute))
	if err := store.ScheduleOutboxRetry(ctx, uow, f.tenant, outboxID, claim.Epoch, now.Add(time.Hour), "provider_failed", now); err != nil {
		done()
		t.Fatalf("ScheduleOutboxRetry() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		done()
		t.Fatalf("Commit() error = %v", err)
	}
	done()
	ready, err := store.OutboxReadyEntries(ctx, pool, f.tenant, now, 10)
	if err != nil {
		t.Fatalf("OutboxReadyEntries() error = %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("OutboxReadyEntries() before not-before = %d rows, want 0", len(ready))
	}
	ready, err = store.OutboxReadyEntries(ctx, pool, f.tenant, now.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("OutboxReadyEntries() error = %v", err)
	}
	if len(ready) != 1 || ready[0].Attempts != 1 || ready[0].LastError != "provider_failed" {
		t.Fatalf("OutboxReadyEntries() after not-before = %+v, want attempts 1 with the failure", ready)
	}

	// A stale holder cannot reschedule or exhaust after losing the claim.
	claim, uow, done = claimOne("worker-a", now.Add(2*time.Hour))
	if err := store.ScheduleOutboxRetry(ctx, uow, f.tenant, outboxID, claim.Epoch+1, now.Add(3*time.Hour), "busy", now); err == nil {
		done()
		t.Fatal("ScheduleOutboxRetry() with a stale epoch succeeded")
	} else if !isStaleVersionForTest(err) {
		done()
		t.Fatalf("ScheduleOutboxRetry() stale error = %v, want ErrStaleVersion", err)
	}
	if err := store.ExhaustOutboxRow(ctx, uow, f.tenant, outboxID, claim.Epoch, "provider_failed", now); err != nil {
		done()
		t.Fatalf("ExhaustOutboxRow() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		done()
		t.Fatalf("Commit() error = %v", err)
	}
	done()
	ready, err = store.OutboxReadyEntries(ctx, pool, f.tenant, now.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatalf("OutboxReadyEntries() error = %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("OutboxReadyEntries() after exhaust = %d rows, want 0", len(ready))
	}
}

func TestDispatchAttemptLeaseFencing(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true

	withTx := func(fn func(uow *store.UnitOfWork) error) error {
		t.Helper()
		uow, err := store.Begin(ctx, pool)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		defer func() { _ = uow.Rollback(ctx) }()
		if err := fn(uow); err != nil {
			return err
		}
		return uow.Commit(ctx)
	}

	var first store.AttemptLease
	if err := withTx(func(uow *store.UnitOfWork) error {
		var err error
		first, err = store.ClaimAttemptLease(ctx, uow, f.tenant, attempt.ID, "worker-a", time.Minute, now)
		return err
	}); err != nil {
		t.Fatalf("ClaimAttemptLease() error = %v", err)
	}
	if first.Epoch != 1 || first.Owner != "worker-a" {
		t.Fatalf("ClaimAttemptLease() = %+v, want epoch 1 owned by worker-a", first)
	}
	// A rival cannot take a live lease; the owner can renew it.
	if err := withTx(func(uow *store.UnitOfWork) error {
		_, err := store.ClaimAttemptLease(ctx, uow, f.tenant, attempt.ID, "worker-b", time.Minute, now.Add(time.Second))
		return err
	}); err == nil {
		t.Fatal("rival ClaimAttemptLease() on a live lease succeeded")
	} else if !isStaleVersionForTest(err) {
		t.Fatalf("rival ClaimAttemptLease() error = %v, want ErrStaleVersion", err)
	}
	if err := withTx(func(uow *store.UnitOfWork) error {
		return store.RenewAttemptLease(ctx, uow, f.tenant, attempt.ID, first, time.Minute, now.Add(time.Second))
	}); err != nil {
		t.Fatalf("RenewAttemptLease() error = %v", err)
	}
	// After expiry, takeover bumps the epoch and fences the old holder.
	var second store.AttemptLease
	if err := withTx(func(uow *store.UnitOfWork) error {
		var err error
		second, err = store.ClaimAttemptLease(ctx, uow, f.tenant, attempt.ID, "worker-b", time.Minute, now.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("takeover ClaimAttemptLease() error = %v", err)
	}
	if second.Epoch != first.Epoch+1 {
		t.Fatalf("takeover epoch = %d, want %d", second.Epoch, first.Epoch+1)
	}
	if err := withTx(func(uow *store.UnitOfWork) error {
		return store.RenewAttemptLease(ctx, uow, f.tenant, attempt.ID, first, time.Minute, now.Add(3*time.Minute))
	}); err == nil {
		t.Fatal("RenewAttemptLease() with a fenced epoch succeeded")
	} else if !isStaleVersionForTest(err) {
		t.Fatalf("RenewAttemptLease() stale error = %v, want ErrStaleVersion", err)
	}
}

func TestDispatchShadowResultAppendOnly(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	shadowID, err := ids.New[ids.Any]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	result := store.ShadowResult{
		ID: shadowID, Tenant: f.tenant, Attempt: attempt.ID, CapturedAt: now,
		Summary: "shadow ran", Comparison: "matches live",
		Digest: identity.DigestOf([]byte("shadow-bytes")),
	}
	stored, replayed, err := store.RecordShadowResult(ctx, uow, result)
	if err != nil || replayed || stored.ID != shadowID {
		t.Fatalf("RecordShadowResult() = %v, %v, %v", stored, replayed, err)
	}
	stored, replayed, err = store.RecordShadowResult(ctx, uow, result)
	if err != nil || !replayed {
		t.Fatalf("RecordShadowResult() replay = %v, %v, %v", stored, replayed, err)
	}
	altered := result
	altered.Comparison = "differs"
	if _, _, err := store.RecordShadowResult(ctx, uow, altered); err == nil {
		t.Fatal("RecordShadowResult() with a different payload succeeded")
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed = true
	listed, err := store.ListShadowResults(ctx, pool, f.tenant, attempt.ID)
	if err != nil {
		t.Fatalf("ListShadowResults() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != shadowID {
		t.Fatalf("ListShadowResults() = %v, want the one recorded row", listed)
	}
}

func TestDispatchOutboxReclaimLapsedLease(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	outboxID := enqueueTaskSubmitWithDemand(t, ctx, pool, f, now, contracts.DemandBackground)
	pollAt := now.Add(time.Minute)

	claimTx := func(owner string, at time.Time) (store.OutboxClaim, *store.UnitOfWork, func()) {
		t.Helper()
		uow, err := store.Begin(ctx, pool)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		claim, claimed, err := store.ClaimOutboxRow(ctx, uow, f.tenant, owner, time.Minute, at)
		if err != nil || !claimed {
			_ = uow.Rollback(ctx)
			t.Fatalf("ClaimOutboxRow(%s) = %v, %v, %v", owner, claim, claimed, err)
		}
		return claim, uow, func() { _ = uow.Rollback(ctx) }
	}

	// worker-a claims, then dies without acking: the lease lapses.
	first, uow, done := claimTx("worker-a", pollAt)
	if err := uow.Commit(ctx); err != nil {
		done()
		t.Fatalf("Commit() error = %v", err)
	}
	done()

	// worker-b reclaims the lapsed processing row at a bumped epoch.
	second, uow, done := claimTx("worker-b", pollAt.Add(2*time.Minute))
	if second.Row.ID != outboxID || second.Epoch != first.Epoch+1 || second.Owner != "worker-b" {
		done()
		t.Fatalf("reclaim = %+v, want the row at epoch %d owned by worker-b", second, first.Epoch+1)
	}
	// The crashed holder's ack is fenced even though the row is processing.
	if err := store.AckOutboxRow(ctx, uow, f.tenant, outboxID, first.Epoch, pollAt.Add(2*time.Minute)); err == nil {
		done()
		t.Fatal("AckOutboxRow() with a reclaimed epoch succeeded")
	} else if !isStaleVersionForTest(err) {
		done()
		t.Fatalf("AckOutboxRow() stale error = %v, want ErrStaleVersion", err)
	}
	if err := store.AckOutboxRow(ctx, uow, f.tenant, outboxID, second.Epoch, pollAt.Add(2*time.Minute)); err != nil {
		done()
		t.Fatalf("AckOutboxRow() error = %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		done()
		t.Fatalf("Commit() error = %v", err)
	}
	done()
}

func TestDispatchOutboxClaimPersonFirst(t *testing.T) {
	ctx, pool := setupTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := seedTenant(t, ctx, pool)
	// Enqueue background first so insertion order cannot explain the result.
	backgroundID := enqueueTaskSubmitWithDemand(t, ctx, pool, f, now, contracts.DemandBackground)
	personID := enqueueTaskSubmitWithDemand(t, ctx, pool, f, now, contracts.DemandPerson)
	pollAt := now.Add(time.Minute)

	uow, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = uow.Rollback(ctx) }()
	first, claimed, err := store.ClaimOutboxRow(ctx, uow, f.tenant, "worker-a", time.Minute, pollAt)
	if err != nil || !claimed {
		t.Fatalf("ClaimOutboxRow() = %v, %v, %v", first, claimed, err)
	}
	if first.Row.ID != personID {
		t.Fatalf("ClaimOutboxRow() took %v, want the person-demand row %v (background was %v)",
			first.Row.ID, personID, backgroundID)
	}
}
