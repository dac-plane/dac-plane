//go:build integration

package postgres_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func connect(t *testing.T, ctx context.Context, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pgx.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestStartIsolatesConcurrentFixtures(t *testing.T) {
	// Parallel subtests run after this function returns, so a deferred cancel
	// would end their context before they start.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	var written sync.WaitGroup
	written.Add(2)
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wrote := sync.OnceFunc(written.Done)
			defer wrote()
			conn := connect(t, ctx, testpostgres.Start(t))
			if _, err := conn.Exec(ctx, `create table marker (owner text not null)`); err != nil {
				t.Fatalf("create table: %v", err)
			}
			if _, err := conn.Exec(ctx, `insert into marker (owner) values ($1)`, name); err != nil {
				t.Fatalf("insert: %v", err)
			}
			// Both fixtures have written before either reads.
			wrote()
			written.Wait()
			rows, err := conn.Query(ctx, `select owner from marker`)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if len(owners) != 1 || owners[0] != name {
				t.Fatalf("marker owners = %v, want only %q", owners, name)
			}
		})
	}
}

func TestStartReusesServerAndDropsDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var first *url.URL
	t.Run("fixture", func(t *testing.T) {
		var err error
		if first, err = url.Parse(testpostgres.Start(t)); err != nil {
			t.Fatal(err)
		}
	})
	if first == nil {
		t.FailNow()
	}
	second, err := url.Parse(testpostgres.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	if first.Host != second.Host {
		t.Fatalf("second fixture host = %s, want the shared server %s", second.Host, first.Host)
	}
	if first.Path == second.Path {
		t.Fatalf("fixtures share database %s", first.Path)
	}
	var count int
	conn := connect(t, ctx, second.String())
	if err := conn.QueryRow(ctx, `select count(*) from pg_database where datname = $1`, strings.TrimPrefix(first.Path, "/")).Scan(&count); err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	if count != 0 {
		t.Fatalf("database %s survived its fixture's cleanup", first.Path)
	}
}
