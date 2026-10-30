//go:build operationstest

package migrations

import (
	"embed"
	"io/fs"
	"testing/fstest"
)

// The operations image models the next release without putting its probe in
// the production migration source. It is built only by the #74 test recipe.
//
//go:embed migrations/*.sql operationstest/*.sql
var operationsEmbedded embed.FS

// Source contains released migrations plus the isolated schema-16 probe.
var Source = operationsSource()

// CurrentVersion is the next-release fixture's migration version.
const CurrentVersion int64 = 16

// MinServeVersion keeps the next image compatible with the current schema
// while every binary is rolled before migration (ADR-0011).
const MinServeVersion int64 = 13

func operationsSource() fs.FS {
	out := fstest.MapFS{}
	for _, dir := range []string{"migrations", "operationstest"} {
		entries, err := fs.ReadDir(operationsEmbedded, dir)
		if err != nil {
			panic("migrations: embedded operations directory is missing: " + err.Error())
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := fs.ReadFile(operationsEmbedded, dir+"/"+entry.Name())
			if err != nil {
				panic("migrations: cannot read embedded operation migration: " + err.Error())
			}
			out[entry.Name()] = &fstest.MapFile{Data: data, Mode: 0444}
		}
	}
	return out
}
