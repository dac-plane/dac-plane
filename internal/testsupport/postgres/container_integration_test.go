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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owners := []string{"first", "second"}
	conns := make([]*pgx.Conn, len(owners))
	for i := range owners {
		conns[i] = connect(t, ctx, testpostgres.Start(t))
	}
	// Both fixtures write at the same time, in goroutines rather than parallel
	// subtests, so the test cannot deadlock when -parallel is 1.
	errs := make([]error, len(owners))
	var writers sync.WaitGroup
	for i, owner := range owners {
		writers.Go(func() {
			if _, errs[i] = conns[i].Exec(ctx, `create table marker (owner text not null)`); errs[i] == nil {
				_, errs[i] = conns[i].Exec(ctx, `insert into marker (owner) values ($1)`, owner)
			}
		})
	}
	writers.Wait()
	for i, owner := range owners {
		if errs[i] != nil {
			t.Fatalf("%s fixture write: %v", owner, errs[i])
		}
		rows, err := conns[i].Query(ctx, `select owner from marker`)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(got) != 1 || got[0] != owner {
			t.Fatalf("%s fixture marker owners = %v, want only %q", owner, got, owner)
		}
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
