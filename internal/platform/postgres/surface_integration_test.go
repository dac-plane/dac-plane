//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// insertEvent writes one durable event outside any task in its own
// transaction, holding the transaction open for hold before committing.
func insertEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant ids.TenantID, hold time.Duration) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	eventID, recordID := newV7(t), newV7(t)
	if _, err := tx.Exec(ctx, `
		insert into durable_event (tenant_id, event_id, record_kind, record_id, event, from_state, to_state, version, occurred_at)
		values ($1, $2, 'harness_qualification', $3, 'promote', 'off', 'shadow', 1, now())`,
		tenant.String(), eventID, recordID); err != nil {
		t.Error(err)
		return
	}
	time.Sleep(hold)
	if err := tx.Commit(ctx); err != nil {
		t.Error(err)
	}
}

// TestStreamEventsArriveInCommitOrderWithoutSkips races many writers whose
// transactions are held open for different times, so they insert in one
// order and commit in another, while a reader follows the stream by cursor.
// The reader must see every event exactly once, in increasing position.
func TestStreamEventsArriveInCommitOrderWithoutSkips(t *testing.T) {
	ctx, pool := setupTestDB(t)
	tenant := seedTenant(t, ctx, pool).tenant
	const writers = 40

	var wait sync.WaitGroup
	for i := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			insertEvent(t, ctx, pool, tenant, time.Duration((writers-i)%7)*15*time.Millisecond)
		}()
	}
	done := make(chan struct{})
	go func() { wait.Wait(); close(done) }()

	seen := map[int64]bool{}
	var cursor int64
	read := func() {
		events, err := store.ListStreamEvents(ctx, pool, tenant, ids.TaskID{}, ids.PrincipalID{}, cursor, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Position <= cursor || seen[event.Position] {
				t.Fatalf("position %d after cursor %d (seen before: %v)", event.Position, cursor, seen[event.Position])
			}
			seen[event.Position] = true
			cursor = event.Position
		}
	}
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		case <-time.After(2 * time.Millisecond):
		}
		read()
	}
	read()

	var total int
	if err := pool.QueryRow(ctx, `select count(*) from event_position where tenant_id = $1`, tenant.String()).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != writers || len(seen) != writers {
		t.Fatalf("reader saw %d of %d committed events (%d positioned)", len(seen), writers, total)
	}
	head, err := store.StreamHead(ctx, pool, tenant)
	if err != nil || head != cursor {
		t.Fatalf("head = %d, %v; reader ended at %d", head, err, cursor)
	}
}

// TestRolledBackEventGetsNoPosition shows a transaction that rolls back
// leaves nothing in the stream.
func TestRolledBackEventGetsNoPosition(t *testing.T) {
	ctx, pool := setupTestDB(t)
	tenant := seedTenant(t, ctx, pool).tenant
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		insert into durable_event (tenant_id, event_id, record_kind, record_id, event, from_state, version, occurred_at)
		values ($1, $2, 'harness_qualification', $3, 'promote', 'off', 1, now())`, tenant.String(), newV7(t), newV7(t)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if head, err := store.StreamHead(ctx, pool, tenant); err != nil || head != 0 {
		t.Fatalf("head after rollback = %d, %v", head, err)
	}
	if _, err := pool.Exec(ctx, `update event_position set task_id = null`); err != nil {
		t.Fatalf("update on empty table should not fail: %v", err)
	}
}

func newV7(t *testing.T) string {
	t.Helper()
	id, err := ids.New[ids.Any]()
	if err != nil {
		t.Error(err)
	}
	return id.String()
}
