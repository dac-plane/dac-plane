//go:build integration

// Package postgres provides hermetic PostgreSQL integration-test support.
//
// A test process starts at most one pinned PostgreSQL container, on the first
// Start, and every fixture receives its own freshly created database on it.
// A package that calls Start runs its tests through Main, which removes the
// container when the tests finish, pass or fail; if the process dies first,
// the Testcontainers reaper removes it.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const image = "postgres:16.15-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"

var (
	// running is set by Main; Start refuses to run without it, so a package
	// cannot start a container that nothing will remove.
	running atomic.Bool

	// container is written once by the server start and read by Main after
	// every test has finished.
	container *postgrescontainer.PostgresContainer

	// server starts the shared container once and returns the URL of its
	// maintenance database. A failed start is remembered, so every later
	// fixture fails with the same error instead of retrying.
	server = sync.OnceValues(startServer)
)

// Main runs the package's tests and then removes the shared container, if one
// was started. Call it from TestMain in every package that uses Start:
//
//	func TestMain(m *testing.M) { os.Exit(testpostgres.Main(m)) }
func Main(m *testing.M) int {
	running.Store(true)
	code := m.Run()
	if container == nil {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := testcontainers.TerminateContainer(container, testcontainers.StopContext(ctx)); err != nil {
		fmt.Fprintf(os.Stderr, "terminate PostgreSQL integration container: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Start creates an empty PostgreSQL 16 database for the test on the shared
// container and returns its connection string. The database is dropped by a
// cleanup registered before the connection string is returned, so it runs
// after any cleanup the caller registers later, such as closing a pool.
func Start(t testing.TB) string {
	t.Helper()
	if !running.Load() {
		t.Fatal("testpostgres.Start needs the package TestMain to call testpostgres.Main")
	}
	maintenanceURL, err := server()
	if err != nil {
		t.Fatalf("start PostgreSQL integration container: %v", err)
	}

	name, err := databaseName(t.Name())
	if err != nil {
		t.Fatalf("name PostgreSQL integration database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := administer(ctx, maintenanceURL, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// FORCE ends connections the test leaked, which would otherwise block the drop.
		if err := administer(ctx, maintenanceURL, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("drop PostgreSQL integration database: %v", err)
		}
	})

	databaseURL, err := url.Parse(maintenanceURL)
	if err != nil {
		t.Fatalf("parse PostgreSQL integration connection string: %v", err)
	}
	databaseURL.Path = "/" + name
	return databaseURL.String()
}

func startServer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started, err := postgrescontainer.Run(
		ctx,
		image,
		postgrescontainer.WithDatabase("dac_plane_test"),
		postgrescontainer.WithUsername("dac_plane"),
		postgrescontainer.WithPassword("integration-test-only"),
		postgrescontainer.BasicWaitStrategies(),
	)
	if started != nil {
		container = started
	}
	if err != nil {
		return "", err
	}
	return started.ConnectionString(ctx, "sslmode=disable")
}

// administer runs one statement on the maintenance database. CREATE and DROP
// DATABASE cannot run inside a transaction, so it uses a plain connection.
func administer(ctx context.Context, maintenanceURL, statement string) error {
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return err
	}
	_, execErr := conn.Exec(ctx, statement)
	return errors.Join(execErr, conn.Close(ctx))
}

// databaseName derives a unique database name from the test name: a readable
// prefix plus random hex, within PostgreSQL's 63-byte identifier limit.
func databaseName(testName string) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	prefix := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, testName)
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return "t_" + prefix + "_" + hex.EncodeToString(suffix), nil
}
