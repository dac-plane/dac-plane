//go:build !operationstest

package migrations

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Source holds released migrations at its root, which is how Goose discovers
// versions. Released migrations are never edited; later changes add versions.
var Source = mustSub(embedded, "migrations")

// CurrentVersion is the latest released migration version.
const CurrentVersion int64 = 15

// MinServeVersion is the oldest schema this release serves (ADR-0011).
const MinServeVersion int64 = 13

func mustSub(source fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(source, dir)
	if err != nil {
		panic("migrations: embedded migration directory is missing: " + err.Error())
	}
	return sub
}
