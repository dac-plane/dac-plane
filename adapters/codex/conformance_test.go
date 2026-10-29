package codexadapter

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/conformance"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/journal"
)

func TestControlFrameConformance(t *testing.T) {
	root := t.TempDir()
	state, err := journal.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	native := newFakeNative()
	adapter, err := New(Config{Native: native, Journal: state, Workspace: root, Model: "deepseek-v4-flash-0731", Provider: "camer", ProviderID: "01a18301-0000-7000-8000-000000000030"})
	if err != nil {
		t.Fatal(err)
	}
	defer close(adapter.stop)
	request := startRequest(validStart)
	request.Delivery = ""
	request.Deadline = time.Time{}
	cursor := wire.Cursor{Tenant: testTenant, Attempt: testAttempt, Operation: testOperation, Stream: "dispatch", Generation: 1, After: 2}
	stale := cursor
	stale.Generation = 2
	target := conformance.Target{Fixture: conformance.Fixture{Provider: "01a18301-0000-7000-8000-000000000030", Role: wire.RoleRuntime, Request: request, Cursor: &cursor, StaleCursor: &stale}, Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer cancel()
			_ = adapterkit.Run(runCtx, adapterkit.Config{URL: endpoint.URL, Token: func(context.Context) (adapterkit.Token, error) {
				return adapterkit.Token{Value: "fixture-token", Expiry: time.Now().Add(time.Hour)}, nil
			}, Provider: "01a18301-0000-7000-8000-000000000030", Role: wire.RoleRuntime, Handler: adapter, TLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: endpoint.RootCAs}, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), BackoffBase: time.Millisecond, BackoffMax: 10 * time.Millisecond})
		}()
		return func() { cancel(); <-done }, nil
	}}
	conformance.Run(t, target)
}
