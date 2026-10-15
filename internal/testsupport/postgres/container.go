//go:build integration

// Package postgres provides hermetic PostgreSQL integration-test support.
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const image = "postgres:16.15-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"

// Start starts PostgreSQL 16 and returns its connection string. Cleanup is
// registered with the test before the connection string is returned.
func Start(t testing.TB) string {
	t.Helper()

	startContext, cancelStart := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelStart()

	container, err := postgrescontainer.Run(
		startContext,
		image,
		postgrescontainer.WithDatabase("dac_plane_test"),
		postgrescontainer.WithUsername("dac_plane"),
		postgrescontainer.WithPassword("integration-test-only"),
		postgrescontainer.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL integration container: %v", err)
	}

	t.Cleanup(func() {
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if err := testcontainers.TerminateContainer(container, testcontainers.StopContext(cleanupContext)); err != nil {
			t.Errorf("terminate PostgreSQL integration container: %v", err)
		}
	})

	databaseURL, err := container.ConnectionString(startContext, "sslmode=disable")
	if err != nil {
		t.Fatalf("build PostgreSQL integration connection string: %v", err)
	}
	return databaseURL
}
