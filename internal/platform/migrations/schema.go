package migrations

import (
	"errors"
	"fmt"
)

// ErrSchemaOutOfRange reports a database schema this release cannot serve.
var ErrSchemaOutOfRange = errors.New("schema version is outside the range this release serves")

// CheckServable reports whether a database at version may be served.
func CheckServable(version int64) error {
	if version < MinServeVersion || version > CurrentVersion {
		return fmt.Errorf("%w: database is at %d, this release serves %d to %d; run dac-plane migrate", ErrSchemaOutOfRange, version, MinServeVersion, CurrentVersion)
	}
	return nil
}
