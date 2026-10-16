// Package postgres Value helpers translate between Go contract values and row columns.
// Zero identifiers and empty optionals become NULL; stored rows are validated
// after mapping so corruption surfaces as an error, never a silent record.
package postgres

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// uuidOrNil maps a zero identifier to NULL.
func uuidOrNil[K any](id ids.ID[K]) any {
	if id.IsZero() {
		return nil
	}
	return id.String()
}

// textOrNil maps an empty optional to NULL.
func textOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timeOrNil maps a zero time to NULL.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// corruptRow reports stored data that no longer validates.
func corruptRow(column string, err error) error {
	return fmt.Errorf("corrupt %s: %w", column, err)
}

// parseID parses a uuid text column back into a typed identifier.
func parseID[K any](column string, value string) (ids.ID[K], error) {
	id, err := ids.Parse[K](value)
	if err != nil {
		return ids.ID[K]{}, fmt.Errorf("parse %s: %w", column, err)
	}
	return id, nil
}

// parseOptionalID parses a nullable uuid text column.
func parseOptionalID[K any](column string, value *string) (ids.ID[K], error) {
	if value == nil || *value == "" {
		return ids.ID[K]{}, nil
	}
	return parseID[K](column, *value)
}

// parseUUIDArray parses a uuid[] column selected as text ({a,b}).
func parseUUIDArray[K any](column string, value string) ([]ids.ID[K], error) {
	trimmed := strings.Trim(value, "{}")
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]ids.ID[K], 0, len(parts))
	for _, part := range parts {
		id, err := parseID[K](column, part)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// isNoRows reports pgx.ErrNoRows without importing it at call sites.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
