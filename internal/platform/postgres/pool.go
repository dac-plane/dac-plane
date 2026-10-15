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
