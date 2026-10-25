// Package conformance checks that an adapter speaks ControlFrame the way the
// dac-plane control plane requires (ADR-0008, ADR-0010). The harness plays the
// control plane: it listens over TLS, waits for the adapter under test to
// dial in, and drives scenarios over that connection: negotiation, request
// and reply correlation, idempotent redelivery, rejection of foreign methods,
// cancellation, subscription, resubscription replaying the cursor's stream
// with provider bodies such as cursor_too_old relayed verbatim, bounded
// silence after an unsubscribe, liveness, reconnection after a close, and
// refusal of malformed, binary, out-of-direction and mismatched frames.
//
// The harness checks protocol behavior only. Whether a provider's effects
// are correct is qualified separately with that provider's own fixtures.
package conformance

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

// Fixture describes the adapter under test and the work it can answer.
type Fixture struct {
	// Provider and Role are the registration the adapter speaks for.
	Provider string    `json:"provider_id"`
	Role     wire.Role `json:"role"`
	// Request is a valid request the adapter answers with a final reply. The
	// harness sets its delivery and deadline.
	Request wire.Request `json:"request"`
	// Cursor, when set, is a stream the adapter serves.
	Cursor *wire.Cursor `json:"cursor,omitempty"`
	// StaleCursor, when set, is a stream the adapter answers with connected
	// followed by cursor_too_old, as its provider scripts it. The resubscribe
	// scenario runs only against it.
	StaleCursor *wire.Cursor `json:"stale_cursor,omitempty"`
}

// Endpoint is where the adapter must connect.
type Endpoint struct {
	// URL is the adapter endpoint.
	URL string
	// CertificatePEM is the harness's TLS certificate; the adapter must
	// trust it. RootCAs holds the same certificate.
	CertificatePEM []byte
	RootCAs        *x509.CertPool
}

// Target is the adapter under test.
type Target struct {
	Fixture Fixture
	// Start starts the adapter so it dials the endpoint with any non-empty
	// bearer token, and returns how to stop it.
	Start func(ctx context.Context, endpoint Endpoint) (stop func(), err error)
}

// Result is the outcome of one scenario.
type Result struct {
	Name string
	Err  error
}

// Timeouts for one step of a scenario and for an adapter to reconnect; a
// reconnect allows for the adapter's backoff.
const (
	stepTimeout      = 10 * time.Second
	reconnectTimeout = 45 * time.Second
)

// Run checks target as subtests of t.
func Run(t *testing.T, target Target) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, result := range Check(ctx, target) {
		t.Run(result.Name, func(t *testing.T) {
			if result.Err != nil {
				t.Fatal(result.Err)
			}
		})
	}
}

// Check runs every scenario against target in order. Scenarios share one
// adapter process; a failed scenario that leaves no usable connection skips
// the rest.
func Check(ctx context.Context, target Target) []Result {
	h := newHarness()
	defer h.server.Close()
	stop, err := target.Start(ctx, h.endpoint())
	if err != nil {
		return []Result{{Name: "start", Err: err}}
	}
	defer stop()
	f := target.Fixture
	scenarios := []struct {
		name string
		run  func(context.Context, *harness, Fixture) error
	}{
		{"handshake negotiates before work", scenarioHandshake},
		{"reply answers its delivery", scenarioReply},
		{"redelivery is idempotent", scenarioRedelivery},
		{"foreign method is rejected", scenarioForeignMethod},
		{"cancel keeps the session", scenarioCancel},
		{"subscribe streams from the cursor", scenarioSubscribe},
		{"resubscribe replays the cursor and relays cursor_too_old", scenarioResubscribeRelay},
		{"pings are answered", scenarioLiveness},
		{"reconnects after a close", scenarioReconnect},
		{"reconnects with an open stream", scenarioReconnectWithOpenStream},
		{"malformed frame ends the session", scenarioMalformed},
		{"binary message ends the session", scenarioBinary},
		{"frame of the other direction ends the session", scenarioWrongDirection},
		{"unoffered version is refused", scenarioVersionMismatch},
	}
	var results []Result
	for i, scenario := range scenarios {
		err := scenario.run(ctx, h, f)
		results = append(results, Result{Name: scenario.name, Err: err})
		if err != nil && h.current == nil {
			for _, rest := range scenarios[i+1:] {
				results = append(results, Result{Name: rest.name, Err: errors.New("skipped: no usable connection")})
			}
			break
		}
	}
	return results
}

// harness is the stand-in control plane.
type harness struct {
	server  *httptest.Server
	conns   chan *websocket.Conn
	current *link
	first   *wire.Reply
}

// link is one adapter connection with a reader that owns its reads, so a
// ping can wait for its pong while scenarios consume frames.
type link struct {
	conn   *websocket.Conn
	frames chan received
}

type received struct {
	frame wire.Frame
	err   error
}

func newLink(conn *websocket.Conn) *link {
	l := &link{conn: conn, frames: make(chan received, 64)}
	go func() {
		defer close(l.frames)
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				l.frames <- received{err: err}
				return
			}
			if typ != websocket.MessageText {
				l.frames <- received{err: fmt.Errorf("%w: adapter sent a binary message", wire.ErrMalformed)}
				continue
			}
			frame, err := wire.Decode(data)
			l.frames <- received{frame: frame, err: err}
		}
	}()
	return l
}

// next returns the next frame, or the read error that ended the connection.
func (l *link) next(ctx context.Context) (wire.Frame, error) {
	select {
	case r, ok := <-l.frames:
		if !ok {
			return wire.Frame{}, errors.New("connection closed")
		}
		return r.frame, r.err
	case <-ctx.Done():
		return wire.Frame{}, ctx.Err()
	}
}

func (l *link) write(ctx context.Context, frame wire.Frame) error {
	data, err := wire.Encode(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	return l.conn.Write(wctx, websocket.MessageText, data)
}

func newHarness() *harness {
	h := &harness{conns: make(chan *websocket.Conn, 4)}
	h.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
		if err != nil {
			return
		}
		if conn.Subprotocol() != wire.Subprotocol {
			_ = conn.Close(websocket.StatusPolicyViolation, "the ControlFrame subprotocol is required")
			return
		}
		conn.SetReadLimit(wire.DefaultMaxFrameBytes)
		// The connection is hijacked and outlives this handler.
		h.conns <- conn
	}))
	return h
}

func (h *harness) endpoint() Endpoint {
	certificate := h.server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return Endpoint{
		URL:            strings.Replace(h.server.URL, "https://", "wss://", 1),
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}),
		RootCAs:        roots,
	}
}

// accept waits for the adapter's next connection and returns it with its
// hello, without answering.
func (h *harness) accept(ctx context.Context, timeout time.Duration) (*link, wire.Lifecycle, error) {
	h.current = nil
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var l *link
	select {
	case conn := <-h.conns:
		l = newLink(conn)
	case <-wait.Done():
		return nil, wire.Lifecycle{}, errors.New("the adapter did not connect")
	}
	step, cancelStep := context.WithTimeout(ctx, stepTimeout)
	defer cancelStep()
	frame, err := l.next(step)
	if err != nil {
		l.conn.CloseNow()
		return nil, wire.Lifecycle{}, fmt.Errorf("reading hello: %w", err)
	}
	if frame.Kind != wire.KindLifecycle || frame.Lifecycle.Phase != wire.PhaseHello {
		l.conn.CloseNow()
		return nil, wire.Lifecycle{}, fmt.Errorf("first frame is %s, want a hello", frame.Kind)
	}
	return l, *frame.Lifecycle, nil
}

// open accepts the next connection and answers its hello with ready.
func (h *harness) open(ctx context.Context, timeout time.Duration) error {
	l, hello, err := h.accept(ctx, timeout)
	if err != nil {
		return err
	}
	if !slices.Contains(hello.Versions, wire.ProtocolVersion) {
		l.conn.CloseNow()
		return fmt.Errorf("hello offers versions %v, want %d among them", hello.Versions, wire.ProtocolVersion)
	}
	if len(hello.Required) > 0 {
		l.conn.CloseNow()
		return fmt.Errorf("hello requires capabilities %v the control plane does not define", hello.Required)
	}
	if err := l.write(ctx, wire.Frame{Kind: wire.KindLifecycle, Lifecycle: &wire.Lifecycle{Phase: wire.PhaseReady, Version: wire.ProtocolVersion}}); err != nil {
		l.conn.CloseNow()
		return err
	}
	h.current = l
	return nil
}

// expectClose reads until the adapter ends the session, requiring a
// shutdown with failure code first.
func (h *harness) expectClose(ctx context.Context, code string) error {
	l := h.current
	h.current = nil
	defer l.conn.CloseNow()
	wait, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	sawShutdown := false
	for {
		frame, err := l.next(wait)
		switch {
		case wait.Err() != nil:
			return errors.New("the adapter kept the session open")
		case err != nil && errors.Is(err, wire.ErrMalformed):
			return fmt.Errorf("adapter sent an invalid frame: %w", err)
		case err != nil:
			if !sawShutdown {
				return fmt.Errorf("session closed without a %s shutdown", code)
			}
			return nil
		case frame.Kind == wire.KindLifecycle && frame.Lifecycle.Phase == wire.PhaseShutdown:
			if frame.Lifecycle.Failure == nil || frame.Lifecycle.Failure.Code != code {
				return fmt.Errorf("shutdown failure = %+v, want %s", frame.Lifecycle.Failure, code)
			}
			sawShutdown = true
		}
	}
}

// ping waits for the adapter to answer a WebSocket ping.
func (h *harness) ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	if err := h.current.conn.Ping(pctx); err != nil {
		return fmt.Errorf("ping not answered: %w", err)
	}
	return nil
}

// exchange sends a request and returns the reply to its delivery, ignoring
// events in between.
func (h *harness) exchange(ctx context.Context, request wire.Request) (wire.Reply, error) {
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindRequest, Request: &request}); err != nil {
		return wire.Reply{}, err
	}
	wait, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	for {
		frame, err := h.current.next(wait)
		if err != nil {
			return wire.Reply{}, fmt.Errorf("waiting for the reply: %w", err)
		}
		switch frame.Kind {
		case wire.KindReply:
			if frame.Reply.Operation == request.Operation && frame.Reply.Delivery == request.Delivery {
				return *frame.Reply, nil
			}
			return wire.Reply{}, fmt.Errorf("reply names delivery %s of %s, want %s of %s", frame.Reply.Delivery, frame.Reply.Operation, request.Delivery, request.Operation)
		case wire.KindEvent:
		default:
			return wire.Reply{}, fmt.Errorf("adapter sent a %s frame", frame.Kind)
		}
	}
}
