package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestRunReturnsCleanlyWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	application := &Application{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := application.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
