// Package migrations configures Goose for PostgreSQL migrations.
package migrations

import (
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// NewProvider creates a PostgreSQL Goose provider backed by source.
// Migration files are intentionally introduced by the schema issue, not here.
func NewProvider(db *sql.DB, source fs.FS) (*goose.Provider, error) {
	provider, err := goose.NewProvider(goose.DialectPostgres, db, source)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL migration provider: %w", err)
	}
	return provider, nil
}

// NewLockedProvider is NewProvider holding a PostgreSQL session advisory
// lock for every migration run, so a second concurrent dac-plane migrate
// waits for the first instead of racing it (ADR-0009).
func NewLockedProvider(db *sql.DB, source fs.FS) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, source, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL migration provider: %w", err)
	}
	return provider, nil
}
