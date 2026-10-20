package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
)

type contextWaitingMigrationPort struct{}

func (contextWaitingMigrationPort) Role() contracts.Role { return contracts.RoleRuntime }

func (contextWaitingMigrationPort) Send(ctx context.Context, _ contracts.Request) (contracts.Reply, error) {
	<-ctx.Done()
	return contracts.Reply{}, ctx.Err()
}

func (contextWaitingMigrationPort) Cancel(context.Context, contracts.Cancel) error { return nil }

func (contextWaitingMigrationPort) Subscribe(context.Context, contracts.Cursor) (contracts.Stream, error) {
	return nil, errors.New("not implemented")
}

func TestMigrationSendUsesConfiguredTimeout(t *testing.T) {
	const timeout = 20 * time.Millisecond
	started := time.Now()
	_, err := sendMigrationRequest(context.Background(), contextWaitingMigrationPort{}, contracts.Request{}, timeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migration send error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("migration send took %s with a %s timeout", elapsed, timeout)
	}
}
