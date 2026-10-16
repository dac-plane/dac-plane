// Package postgres persists lifecycle mutations with transactional
// consistency on top of the #19 schema.
//
// Domain logic stays in the pure internal/lifecycle, internal/identity and
// internal/contracts packages: the store loads rows, runs the in-memory
// transition, and commits the returned record together with its audit event,
// durable event and outbox entry. It never reimplements a transition rule.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrStaleVersion reports an expected-version predicate that updated zero
	// rows: another writer moved the record first.
	ErrStaleVersion = errors.New("stale expected version")
	// ErrNotFound reports a missing record. Cross-tenant access returns the
	// same error so tenants are indistinguishable.
	ErrNotFound = errors.New("record not found")
	// ErrPayloadMismatch reports an idempotency-key reuse with a different
	// payload. Nothing is written.
	ErrPayloadMismatch = errors.New("idempotency key reused with different payload")
)

// Querier is the read/write surface both *pgxpool.Pool and *UnitOfWork
// provide, so repositories work inside and outside transactions.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork is one database transaction. Repositories take it so state,
// audit, durable event and outbox rows commit or roll back together.
type UnitOfWork struct {
	tx pgx.Tx
}

// Begin starts a unit of work on pool.
func Begin(ctx context.Context, pool *pgxpool.Pool) (*UnitOfWork, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &UnitOfWork{tx: tx}, nil
}

// Commit commits the transaction.
func (u *UnitOfWork) Commit(ctx context.Context) error {
	return u.tx.Commit(ctx)
}

// Rollback aborts the transaction. It is safe to call after Commit.
func (u *UnitOfWork) Rollback(ctx context.Context) error {
	return u.tx.Rollback(ctx)
}

// Exec runs a statement in the transaction.
func (u *UnitOfWork) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return u.tx.Exec(ctx, sql, args...)
}

// Query runs a query in the transaction.
func (u *UnitOfWork) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return u.tx.Query(ctx, sql, args...)
}

// QueryRow runs a single-row query in the transaction.
func (u *UnitOfWork) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return u.tx.QueryRow(ctx, sql, args...)
}

// isUniqueViolation reports a PostgreSQL unique violation, optionally narrowed
// to one constraint or index name.
func isUniqueViolation(err error, names ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	if len(names) == 0 {
		return true
	}
	for _, name := range names {
		if pgErr.ConstraintName == name {
			return true
		}
	}
	return false
}
