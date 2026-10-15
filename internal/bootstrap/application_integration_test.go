//go:build integration

package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func TestNewConnectsAndApplicationStopsOnCancellation(t *testing.T) {
	databaseURL := testpostgres.Start(t)
	application, err := New(context.Background(), config.Config{
		DatabaseURL:            databaseURL,
		DatabaseConnectTimeout: 30 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer application.Close()

	runContext, cancelRun := context.WithCancel(context.Background())
	cancelRun()
	if err := application.Run(runContext); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
