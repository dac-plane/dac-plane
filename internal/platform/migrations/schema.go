package migrations

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Source holds the migration files at its root, which is how the goose
// provider discovers versions. Released migrations are never edited; later
// changes add new versions.
var Source = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic("migrations: embedded migration directory is missing: " + err.Error())
	}
	return sub
}

// CurrentVersion is the latest migration version. Fresh databases migrate to
// exactly this version; the integration suite asserts it.
const CurrentVersion int64 = 13

// MinServeVersion is the oldest schema this release serves (ADR-0009). With
// CurrentVersion it bounds the range a serving process accepts at startup.
// Migrations within a release line are additive, so a release raises this
// only when it contracts a shape no release it rolls out beside still needs;
// a destructive change is split across two releases to keep that true.
const MinServeVersion int64 = 12

// ErrSchemaOutOfRange reports a database schema this release cannot serve.
var ErrSchemaOutOfRange = errors.New("schema version is outside the range this release serves")

// CheckServable reports whether a database at version may be served.
func CheckServable(version int64) error {
	if version < MinServeVersion || version > CurrentVersion {
		return fmt.Errorf("%w: database is at %d, this release serves %d to %d; run dac-plane migrate", ErrSchemaOutOfRange, version, MinServeVersion, CurrentVersion)
	}
	return nil
}
