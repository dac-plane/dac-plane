package adapterkit_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

// floodHandler serves streams that emit heartbeats as fast as the connection
// takes them, so an unsubscribe nearly always arrives while one is written.
type floodHandler struct{ echoHandler }

func (*floodHandler) Subscribe(context.Context, wire.Cursor) (adapterkit.Stream, error) {
	return floodStream{}, nil
}

type floodStream struct{}

func (floodStream) Next(ctx context.Context) (wire.StreamFrame, error) {
	if ctx.Err() != nil {
		return wire.StreamFrame{}, io.EOF
	}
	return wire.StreamFrame{Kind: wire.StreamHeartbeat, ServerTime: time.Now().UTC()}, nil
}

func (floodStream) Close() error { return nil }

// TestEndingAStreamKeepsTheConnection ends streams while their events are
// being written. Ending one stream must not end the session that carries it.
func TestEndingAStreamKeepsTheConnection(t *testing.T) {
	conns := make(chan *websocket.Conn, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
		if err != nil {
			return
		}
		conns <- conn
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = adapterkit.Run(ctx, adapterkit.Config{
			URL: strings.Replace(server.URL, "https://", "wss://", 1), Token: testToken(), Provider: provider,
			Role: wire.RoleRuntime, Handler: &floodHandler{},
			TLS: &tls.Config{RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12},
		})
	}()
	defer func() { cancel(); <-done }()

	var conn *websocket.Conn
	select {
	case conn = <-conns:
	case <-ctx.Done():
		t.Fatal("the adapter did not connect")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(wire.DefaultMaxFrameBytes)
	if _, data, err := conn.Read(ctx); err != nil {
		t.Fatalf("reading hello: %v", err)
	} else if hello, err := wire.Decode(data); err != nil || hello.Kind != wire.KindLifecycle || hello.Lifecycle.Phase != wire.PhaseHello {
		t.Fatalf("first frame = %+v, %v; want a hello", hello, err)
	}
	send := func(frame wire.Frame) {
		t.Helper()
		data, err := wire.Encode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatalf("writing %s: %v", frame.Kind, err)
		}
	}
	send(wire.Frame{Kind: wire.KindLifecycle, Lifecycle: &wire.Lifecycle{Phase: wire.PhaseReady, Version: wire.ProtocolVersion}})

	// One reader owns the connection's reads, which also answers pings.
	lost := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				lost <- err
				return
			}
		}
	}()
	cursor := *fixture().Cursor
	for range 50 {
		send(wire.Frame{Kind: wire.KindSubscribe, Subscribe: &wire.Subscribe{Cursor: cursor}})
		time.Sleep(2 * time.Millisecond)
		send(wire.Frame{Kind: wire.KindUnsubscribe, Unsubscribe: &wire.Unsubscribe{Operation: cursor.Operation, Stream: cursor.Stream}})
	}
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	if err := conn.Ping(pctx); err != nil {
		t.Fatalf("the session ended while its streams ended: %v", err)
	}
	select {
	case err := <-lost:
		t.Fatalf("the adapter closed the connection: %v", err)
	case <-conns:
		t.Fatal("the adapter opened a new connection")
	default:
	}
}
