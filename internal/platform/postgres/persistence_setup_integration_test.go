//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

// setupTestDB starts one hermetic PostgreSQL container, migrates it and
// opens a pool. Every test gets a fresh database; tenants isolate flows
// inside it.
func setupTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	return ctx, openPool(t, ctx, databaseURL)
}
