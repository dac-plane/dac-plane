// Package postgres Statement helpers factor the three shapes every repository repeats:
// taking a row lock for read-modify-write, running a version-predicated
// update, and resolving an insert conflict into replay or mismatch.
package postgres

import (
	"context"
)

// lockRow takes the row lock for a read-modify-write cycle. Table and column
// come from fixed call sites, never from external input.
func lockRow(ctx context.Context, u *UnitOfWork, table, idColumn, tenant, id string) error {
	var locked bool
	err := u.QueryRow(ctx, `select true from `+table+` where tenant_id = $1 and `+idColumn+` = $2 for update`,
		tenant, id).Scan(&locked)
	if err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// execVersionedUpdate runs a version-predicated UPDATE: zero affected rows
// mean a concurrent writer moved the record first.
func execVersionedUpdate(ctx context.Context, u *UnitOfWork, query string, args ...any) error {
	res, err := u.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrStaleVersion
	}
	return nil
}

// replayResult resolves an insert conflict: a missing row under the natural
// key means something else holds the conflicting key, a load failure
// propagates, and otherwise equality decides replay versus mismatch.
func replayResult[T any](zero T, stored T, loadErr error, equal bool) (T, bool, error) {
	if loadErr != nil {
		if isNotFound(loadErr) {
			return zero, false, ErrPayloadMismatch
		}
		return zero, false, loadErr
	}
	if !equal {
		return zero, false, ErrPayloadMismatch
	}
	return stored, true, nil
}
