// Package postgres owns PostgreSQL connection setup for the control plane.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInvalidConfiguration reports an unusable connection configuration
	// without including a potentially credential-bearing connection string.
	ErrInvalidConfiguration = errors.New("invalid PostgreSQL configuration")
	// ErrConnection reports that PostgreSQL was not reachable without exposing
	// connection details.
	ErrConnection = errors.New("connect to PostgreSQL")
)

// Open creates and verifies a PostgreSQL connection pool.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, ErrConnection
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, ErrConnection
	}

	return pool, nil
}

// SchemaVersion returns the latest applied migration version, or 0 for a
// database dac-plane migrate never ran on. It only reads, so a serving role
// without DDL privileges can check its schema range (ADR-0009).
func SchemaVersion(ctx context.Context, q Querier) (int64, error) {
	var exists bool
	if err := q.QueryRow(ctx, `select to_regclass('goose_db_version') is not null`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version int64
	err := q.QueryRow(ctx, `select coalesce(max(version_id), 0) from goose_db_version where is_applied`).Scan(&version)
	return version, err
}
