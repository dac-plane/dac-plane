package migrations

import (
	"embed"
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
const CurrentVersion int64 = 7
