package adapterkit_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/conformance"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

const (
	tenant    = "01a167fd-51d9-76ba-b563-496e2bd89646"
	attempt   = "01a167fd-51d9-76be-8ef5-39a7e14723c0"
	operation = "01a167fd-51d9-7c11-9f2e-5b4b3c2a1d00"
	provider  = "01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10"
)

// echoHandler is a minimal runtime adapter: it accepts each operation once
// and answers redeliveries from the first reply, and serves a stream that
// starts with connected. Its stale stream follows connected with
// cursor_too_old, as a provider that lost the cursor would.
type echoHandler struct {
	mu      sync.Mutex
	replies map[string]wire.Reply
	cancels int
}

func (h *echoHandler) Send(_ context.Context, request wire.Request) (wire.Reply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.replies == nil {
		h.replies = map[string]wire.Reply{}
	}
	reply, ok := h.replies[request.Operation]
	if !ok {
		reply = wire.Reply{Operation: request.Operation, Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted}
		h.replies[request.Operation] = reply
	}
	reply.Delivery = request.Delivery
	return reply, nil
}

func (h *echoHandler) Cancel(context.Context, wire.Cancel) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancels++
	return nil
}

func (h *echoHandler) Subscribe(_ context.Context, cursor wire.Cursor) (adapterkit.Stream, error) {
	stream := &scriptedStream{script: []wire.StreamFrame{{Kind: wire.StreamConnected, ServerTime: time.Now().UTC()}}}
	if cursor.Stream == staleStream {
		stream.script = append(stream.script, wire.StreamFrame{Kind: wire.StreamCursorTooOld})
	}
	return stream, nil
}

const staleStream = "stale"

// scriptedStream serves its script, then waits for cancellation.
type scriptedStream struct{ script []wire.StreamFrame }

func (s *scriptedStream) Next(ctx context.Context) (wire.StreamFrame, error) {
	if len(s.script) > 0 {
		frame := s.script[0]
		s.script = s.script[1:]
		return frame, nil
	}
	<-ctx.Done()
	return wire.StreamFrame{}, io.EOF
}

func (s *scriptedStream) Close() error { return nil }

func testToken() adapterkit.TokenSource {
	return func(context.Context) (adapterkit.Token, error) {
		return adapterkit.Token{Value: "conformance", Expiry: time.Now().Add(time.Hour)}, nil
	}
}

func fixture() conformance.Fixture {
	return conformance.Fixture{
		Provider: provider, Role: wire.RoleRuntime,
		Request: wire.Request{
			Operation: operation, Tenant: tenant, Attempt: attempt, Role: wire.RoleRuntime, Method: "start_turn",
			Arguments: json.RawMessage(`{"prompt":"hello"}`), Demand: "person",
		},
		Cursor:      &wire.Cursor{Tenant: tenant, Attempt: attempt, Operation: operation, Stream: "dispatch", Generation: 1},
		StaleCursor: &wire.Cursor{Tenant: tenant, Attempt: attempt, Operation: operation, Stream: staleStream, Generation: 1},
	}
}

func TestKitAdapterPassesConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{
		Fixture: fixture(),
		Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				go func() {
					select {
					case <-stop:
						cancel()
					case <-ctx.Done():
					}
				}()
				_ = adapterkit.Run(ctx, adapterkit.Config{
					URL: endpoint.URL, Token: testToken(), Provider: provider, Role: wire.RoleRuntime,
					Handler: &echoHandler{}, TLS: &tls.Config{RootCAs: endpoint.RootCAs, MinVersion: tls.VersionTLS12},
					BackoffBase: 50 * time.Millisecond, BackoffMax: 500 * time.Millisecond,
				})
			}()
			return func() { close(stop); <-done }, nil
		},
	})
}

// TestConformanceFailsAnAdapterThatIgnoresRedelivery shows the suite is not
// vacuous: an adapter that answers a redelivery differently fails it.
func TestConformanceFailsAnAdapterThatIgnoresRedelivery(t *testing.T) {
	results := conformance.Check(context.Background(), conformance.Target{
		Fixture: fixture(),
		Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				go func() {
					select {
					case <-stop:
						cancel()
					case <-ctx.Done():
					}
				}()
				_ = adapterkit.Run(ctx, adapterkit.Config{
					URL: endpoint.URL, Token: testToken(), Provider: provider, Role: wire.RoleRuntime,
					Handler: &forgetfulHandler{}, TLS: &tls.Config{RootCAs: endpoint.RootCAs, MinVersion: tls.VersionTLS12},
					BackoffBase: 50 * time.Millisecond, BackoffMax: 500 * time.Millisecond,
				})
			}()
			return func() { close(stop); <-done }, nil
		},
	})
	for _, result := range results {
		if result.Name == "redelivery is idempotent" {
			if result.Err == nil {
				t.Fatal("an adapter that applies a redelivery as new work passed the redelivery scenario")
			}
			return
		}
	}
	t.Fatalf("redelivery scenario did not run: %v", results)
}

// forgetfulHandler accepts the first delivery and rejects every other one,
// as an adapter keyed by delivery instead of operation would.
type forgetfulHandler struct {
	echoHandler
	seen bool
}

func (h *forgetfulHandler) Send(_ context.Context, request wire.Request) (wire.Reply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen {
		return wire.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: wire.OutcomeFailed, Durability: wire.DurabilityRejected,
			Failure: &wire.Failure{Code: wire.FailureResourceBusy, Message: "already running"}}, nil
	}
	h.seen = true
	return wire.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted}, nil
}

func TestFileTokenReadsExpiryWithoutVerifying(t *testing.T) {
	expiry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d,"sub":"adapter-1"}`, expiry.Unix()))
	path := t.TempDir() + "/token"
	if err := writeFile(path, "eyJhbGciOiJSUzI1NiJ9."+payload+".c2ln\n"); err != nil {
		t.Fatal(err)
	}
	token, err := adapterkit.FileToken(path)(context.Background())
	if err != nil || !token.Expiry.Equal(expiry) || token.Value != "eyJhbGciOiJSUzI1NiJ9."+payload+".c2ln" {
		t.Fatalf("FileToken() = %+v, %v", token, err)
	}
	if err := writeFile(path, "not-a-jwt"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapterkit.FileToken(path)(context.Background()); err == nil {
		t.Fatal("FileToken() accepted a token without exp")
	}
}
