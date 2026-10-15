// Package migrations configures Goose for PostgreSQL migrations.
package migrations

import (
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
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
