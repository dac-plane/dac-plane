//go:build integration

package postgres_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func TestOpenConnectsToEmptyPostgreSQL16(t *testing.T) {
	databaseURL := testpostgres.Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer pool.Close()

	var versionText string
	if err := pool.QueryRow(ctx, "show server_version_num").Scan(&versionText); err != nil {
		t.Fatalf("query PostgreSQL version: %v", err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil {
		t.Fatalf("parse PostgreSQL version %q: %v", versionText, err)
	}
	if version < 160000 || version >= 170000 {
		t.Fatalf("PostgreSQL version = %d, want major version 16", version)
	}

	var publicTableCount int
	if err := pool.QueryRow(ctx, `
		select count(*)
		from pg_catalog.pg_tables
		where schemaname = 'public'
	`).Scan(&publicTableCount); err != nil {
		t.Fatalf("count public tables: %v", err)
	}
	if publicTableCount != 0 {
		t.Fatalf("public table count = %d, want 0", publicTableCount)
	}
}
