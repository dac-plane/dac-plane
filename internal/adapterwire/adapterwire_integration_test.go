//go:build integration

package adapterwire_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/adapterwire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
)

type fixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	wire     *wiretest.Wire
	tenant   ids.TenantID
	provider ids.ProviderID
}

func setup(t *testing.T, options adapterwire.Options) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{ctx: ctx, pool: pool, tenant: mustID[ids.Tenant](t), provider: mustID[ids.Provider](t)}
	u, err := store.Begin(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(ctx) }()
	if err := store.InsertTenant(ctx, u, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.wire = wiretest.Start(t, pool, options)
	return f
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) request(t *testing.T) contracts.Request {
	t.Helper()
	return contracts.Request{
		Operation: mustID[ids.Operation](t), Delivery: mustID[ids.Delivery](t), Tenant: f.tenant, Attempt: mustID[ids.Attempt](t),
		Role: contracts.RoleRuntime, Method: contracts.MethodStartTurn, Demand: contracts.DemandPerson, Deadline: time.Now().Add(time.Minute).UTC(),
	}
}

// rawAdapter is a hand-driven adapter connection for fault injection.
type rawAdapter struct {
	conn   *websocket.Conn
	frames chan contracts.Frame
	closed chan struct{}
}

// dial connects as subject without negotiating.
func (f *fixture) dial(t *testing.T, token string) (*rawAdapter, *http.Response, error) {
	t.Helper()
	conn, response, err := websocket.Dial(f.ctx, f.wire.URL(), &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: f.wire.TLS()}},
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{adapterwire.Subprotocol},
	})
	if err != nil {
		return nil, response, err
	}
	a := &rawAdapter{conn: conn, frames: make(chan contracts.Frame, 64), closed: make(chan struct{})}
	go func() {
		defer close(a.closed)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var frame contracts.Frame
			if json.Unmarshal(data, &frame) == nil {
				a.frames <- frame
			}
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return a, response, nil
}

// open connects as a freshly bound adapter of provider and negotiates.
func (f *fixture) open(t *testing.T, provider ids.ProviderID) (*rawAdapter, string) {
	t.Helper()
	subject := f.wire.Bind(t, f.tenant, provider, contracts.RoleRuntime, nil)
	a, _, err := f.dial(t, f.wire.Token(t, subject, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a.send(t, contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{1}}})
	if ready := a.next(t); ready.Kind != contracts.FrameLifecycle || ready.Lifecycle.Phase != contracts.PhaseReady {
		t.Fatalf("negotiation answered %+v, want ready", ready)
	}
	f.wire.WaitConnected(t, f.tenant, provider, contracts.RoleRuntime)
	return a, subject
}

func (a *rawAdapter) send(t *testing.T, frame contracts.Frame) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	a.write(t, websocket.MessageText, data)
}

func (a *rawAdapter) write(t *testing.T, typ websocket.MessageType, data []byte) {
	t.Helper()
	if err := a.conn.Write(context.Background(), typ, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (a *rawAdapter) next(t *testing.T) contracts.Frame {
	t.Helper()
	select {
	case frame := <-a.frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame from the control plane")
		return contracts.Frame{}
	}
}

// waitClosed waits for the control plane to end the session and returns the
// shutdown failure it sent first, if any.
func (a *rawAdapter) waitClosed(t *testing.T) *contracts.Failure {
	t.Helper()
	var failure *contracts.Failure
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-a.frames:
			if frame.Kind == contracts.FrameLifecycle && frame.Lifecycle.Phase == contracts.PhaseShutdown {
				failure = frame.Lifecycle.Failure
			}
		case <-a.closed:
			for {
				select {
				case frame := <-a.frames:
					if frame.Kind == contracts.FrameLifecycle && frame.Lifecycle.Phase == contracts.PhaseShutdown {
						failure = frame.Lifecycle.Failure
					}
				default:
					return failure
				}
			}
		case <-deadline:
			t.Fatal("the control plane kept the session open")
			return nil
		}
	}
}

func TestAdapterWireNoConnectionMeansNoPort(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	if port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime); port != nil {
		t.Fatal("a registration without a live connection has a port")
	}
}

func TestAdapterWireExposesReplayOnlyAfterCapabilitySelection(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	subject := f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	a, _, err := f.dial(t, f.wire.Token(t, subject, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a.send(t, contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &contracts.Lifecycle{
		Phase: contracts.PhaseHello, Versions: []uint32{1}, Offered: []contracts.Capability{contracts.CapabilityOperationReplay},
	}})
	ready := a.next(t)
	if ready.Kind != contracts.FrameLifecycle || ready.Lifecycle.Phase != contracts.PhaseReady || len(ready.Lifecycle.Selected) != 1 {
		t.Fatalf("negotiation answered %+v, want replay selected", ready)
	}
	f.wire.WaitConnected(t, f.tenant, f.provider, contracts.RoleRuntime)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	replayer, ok := port.(contracts.StableOperationReplayer)
	if !ok {
		t.Fatal("negotiated registration did not expose stable-operation replay")
	}
	request := f.request(t)
	result := make(chan error, 1)
	go func() {
		_, err := replayer.Replay(f.ctx, request)
		result <- err
	}()
	frame := a.next(t)
	if frame.Kind != contracts.FrameRequest || frame.Request.Operation != request.Operation {
		t.Fatalf("adapter received %+v, want replay request", frame)
	}
	a.send(t, contracts.Frame{Kind: contracts.FrameReply, Reply: &contracts.Reply{
		Operation: request.Operation, Delivery: request.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted,
	}})
	if err := <-result; err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
}

func TestAdapterWireRefusesUnauthenticatedAdaptersBeforeUpgrade(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	bound := f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	surfaceToken := f.wire.TokenFor(t, bound, "https://dac-plane.example/api", time.Hour)
	for name, token := range map[string]string{
		"no token":           "",
		"unbound identity":   f.wire.Token(t, "never-bound", time.Hour),
		"surface API token":  surfaceToken,
		"expired":            f.wire.Token(t, bound, -time.Minute),
		"not a token at all": "abc.def.ghi",
	} {
		t.Run(name, func(t *testing.T) {
			_, response, err := f.dial(t, token)
			if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("dial error = %v, response %v; want 401 before upgrading", err, response)
			}
			if response.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("401 without a WWW-Authenticate challenge")
			}
		})
	}
	if f.wire.Registry.Connections(f.tenant, f.provider, contracts.RoleRuntime) != 0 {
		t.Fatal("a refused adapter was registered")
	}
	if strings.Contains(f.wire.Logs(), surfaceToken) {
		t.Fatal("a token reached the log")
	}
}

func TestAdapterWireNegotiatesBeforeWork(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	subject := f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	t.Run("work before hello", func(t *testing.T) {
		a, _, err := f.dial(t, f.wire.Token(t, subject, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		reply := contracts.Reply{Operation: mustID[ids.Operation](t), Delivery: mustID[ids.Delivery](t), Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted}
		a.send(t, contracts.Frame{Kind: contracts.FrameReply, Reply: &reply})
		if failure := a.waitClosed(t); failure == nil || failure.Code != contracts.FailureProtocolMismatch {
			t.Fatalf("shutdown failure = %+v, want protocol_mismatch", failure)
		}
	})
	t.Run("no common version", func(t *testing.T) {
		a, _, err := f.dial(t, f.wire.Token(t, subject, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		a.send(t, contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{7}}})
		if failure := a.waitClosed(t); failure == nil || failure.Code != contracts.FailureProtocolMismatch {
			t.Fatalf("shutdown failure = %+v, want protocol_mismatch", failure)
		}
	})
	if f.wire.Registry.Connections(f.tenant, f.provider, contracts.RoleRuntime) != 0 {
		t.Fatal("an adapter that never negotiated was registered")
	}
}

// TestAdapterWireLostReplyIsUncertainNeverUndelivered closes the connection
// after the request arrived and before any reply: Send must fail with an
// error the worker reconciles, never ErrNotDelivered, which would resend.
func TestAdapterWireLostReplyIsUncertainNeverUndelivered(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	a, _ := f.open(t, f.provider)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	result := make(chan error, 1)
	go func() {
		_, err := port.Send(f.ctx, f.request(t))
		result <- err
	}()
	if frame := a.next(t); frame.Kind != contracts.FrameRequest {
		t.Fatalf("adapter received %s, want the request", frame.Kind)
	}
	a.conn.CloseNow()
	select {
	case err := <-result:
		if err == nil || errors.Is(err, contracts.ErrNotDelivered) {
			t.Fatalf("Send() error = %v, want an uncertain transport error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send() still waits after the connection closed")
	}
}

func TestAdapterWireSendWithNoOpenConnectionIsNotDelivered(t *testing.T) {
	f := setup(t, adapterwire.Options{MaxInFlight: 1})
	a, _ := f.open(t, f.provider)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	go func() { _, _ = port.Send(f.ctx, f.request(t)) }()
	if frame := a.next(t); frame.Kind != contracts.FrameRequest {
		t.Fatalf("adapter received %s, want the request", frame.Kind)
	}
	if _, err := port.Send(f.ctx, f.request(t)); !errors.Is(err, contracts.ErrNotDelivered) {
		t.Fatalf("Send() over a full connection error = %v, want ErrNotDelivered", err)
	}
	select {
	case frame := <-a.frames:
		t.Fatalf("a request that was not delivered reached the adapter: %+v", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

// One request's cancellation ends that request, not the connection every other
// request shares.
func TestAdapterWireCancelledSendKeepsTheConnection(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	a, _ := f.open(t, f.provider)
	go func() {
		for {
			select {
			case <-a.frames:
			case <-f.ctx.Done():
				return
			}
		}
	}()
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	for i := range 2000 {
		ctx, cancel := context.WithCancel(f.ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = port.Send(ctx, f.request(t))
		}()
		time.Sleep(time.Duration(i%50) * time.Microsecond)
		cancel()
		<-done
		if f.wire.Registry.Connections(f.tenant, f.provider, contracts.RoleRuntime) != 1 {
			t.Fatalf("connection closed after cancelled send %d", i)
		}
	}
}

func TestAdapterWireDropsDuplicateAndUnmatchedReplies(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	a, _ := f.open(t, f.provider)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	request := f.request(t)
	result := make(chan contracts.Reply, 1)
	go func() {
		reply, err := port.Send(f.ctx, request)
		if err != nil {
			t.Errorf("Send() error = %v", err)
		}
		result <- reply
	}()
	if frame := a.next(t); frame.Kind != contracts.FrameRequest {
		t.Fatalf("adapter received %s, want the request", frame.Kind)
	}
	reply := contracts.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted}
	stranger := contracts.Reply{Operation: mustID[ids.Operation](t), Delivery: mustID[ids.Delivery](t), Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted}
	for _, r := range []contracts.Reply{reply, reply, stranger} {
		a.send(t, contracts.Frame{Kind: contracts.FrameReply, Reply: &r})
	}
	if got := <-result; got.Delivery != request.Delivery || got.Outcome != contracts.OutcomeOK {
		t.Fatalf("Send() = %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if replies, _ := f.wire.Registry.Dropped(); replies == 2 {
			break
		}
		if time.Now().After(deadline) {
			replies, _ := f.wire.Registry.Dropped()
			t.Fatalf("dropped replies = %d, want 2", replies)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.wire.Registry.Connections(f.tenant, f.provider, contracts.RoleRuntime) != 1 {
		t.Fatal("a duplicated reply ended the session")
	}
}

func TestAdapterWireMalformedFramesEndTheSession(t *testing.T) {
	f := setup(t, adapterwire.Options{MaxFrameBytes: 4096})
	for name, message := range map[string]struct {
		typ  websocket.MessageType
		data string
	}{
		"unknown kind":           {websocket.MessageText, `{"kind":"notify"}`},
		"unknown field":          {websocket.MessageText, `{"kind":"lifecycle","lifecycle":{"phase":"shutdown"},"extra":1}`},
		"two frames":             {websocket.MessageText, `{"kind":"lifecycle","lifecycle":{"phase":"shutdown"}}{}`},
		"binary":                 {websocket.MessageBinary, `{}`},
		"request to the control": {websocket.MessageText, `{"kind":"cancel","cancel":{"operation_id":"` + mustID[ids.Operation](t).String() + `","delivery_id":"` + mustID[ids.Delivery](t).String() + `"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := f.open(t, mustID[ids.Provider](t))
			a.write(t, message.typ, []byte(message.data))
			if failure := a.waitClosed(t); failure == nil || failure.Code != contracts.FailureMalformedFrame {
				t.Fatalf("shutdown failure = %+v, want malformed_frame", failure)
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		a, _ := f.open(t, mustID[ids.Provider](t))
		a.write(t, websocket.MessageText, []byte(`{"kind":"lifecycle","pad":"`+strings.Repeat("x", 8192)+`"}`))
		select {
		case <-a.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("an oversize frame did not end the session")
		}
	})
}

func TestAdapterWireEventsAreBoundToThePeerAndCursor(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	a, _ := f.open(t, f.provider)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	cursor := contracts.Cursor{Tenant: f.tenant, Attempt: mustID[ids.Attempt](t), Operation: mustID[ids.Operation](t), Stream: "dispatch", Generation: 2}
	stream, err := port.Subscribe(f.ctx, cursor)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()
	if frame := a.next(t); frame.Kind != contracts.FrameSubscribe || frame.Subscribe.Cursor != cursor {
		t.Fatalf("adapter received %+v, want the subscribe", frame)
	}
	connected := contracts.StreamFrame{Kind: contracts.StreamConnected, ServerTime: time.Now().UTC()}
	event := func(change func(*contracts.Event)) contracts.Event {
		e := contracts.Event{Tenant: f.tenant, Attempt: cursor.Attempt, Provider: f.provider, Operation: cursor.Operation, Stream: cursor.Stream, Generation: cursor.Generation, Body: connected}
		change(&e)
		return e
	}
	forged := []contracts.Event{
		event(func(e *contracts.Event) { e.Provider = mustID[ids.Provider](t) }),
		event(func(e *contracts.Event) { e.Tenant = mustID[ids.Tenant](t) }),
		event(func(e *contracts.Event) { e.Attempt = mustID[ids.Attempt](t) }),
		event(func(e *contracts.Event) { e.Operation = mustID[ids.Operation](t) }),
		event(func(e *contracts.Event) { e.Stream = "transcript" }),
	}
	for _, e := range forged {
		a.send(t, contracts.Frame{Kind: contracts.FrameEvent, Event: &e})
	}
	genuine := event(func(*contracts.Event) {})
	a.send(t, contracts.Frame{Kind: contracts.FrameEvent, Event: &genuine})
	next, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	frame, err := stream.Next(next)
	if err != nil || frame.Kind != contracts.StreamConnected {
		t.Fatalf("Next() = %+v, %v; want only the genuine connected frame", frame, err)
	}
	if _, events := f.wire.Registry.Dropped(); events != int64(len(forged)) {
		t.Fatalf("dropped events = %d, want %d", events, len(forged))
	}
	// A raw adapter may stamp events with another generation than the
	// cursor it was subscribed with. The boundary must not relabel either
	// as current: an older generation is stale and dropped, and a newer
	// one signals rehydration instead of applying its rows.
	stale := event(func(e *contracts.Event) { e.Generation = cursor.Generation - 1 })
	a.send(t, contracts.Frame{Kind: contracts.FrameEvent, Event: &stale})
	newer := event(func(e *contracts.Event) { e.Generation = cursor.Generation + 1 })
	a.send(t, contracts.Frame{Kind: contracts.FrameEvent, Event: &newer})
	next2, cancel2 := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel2()
	frame, err = stream.Next(next2)
	if err != nil || frame.Kind != contracts.StreamCursorTooOld {
		t.Fatalf("Next() = %+v, %v; want cursor_too_old for the newer generation", frame, err)
	}
	if _, events := f.wire.Registry.Dropped(); events != int64(len(forged)+1) {
		t.Fatalf("dropped events = %d, want %d (forged plus the stale generation)", events, len(forged)+1)
	}
	// A cleared body is not an observation: its NewGeneration names the
	// generation the durable cursor advances to, so the boundary must keep
	// it instead of relabeling it cursor_too_old and stalling recovery on a
	// generation the replays never reach.
	cleared := event(func(e *contracts.Event) {
		e.Generation = cursor.Generation + 1
		e.Body = contracts.StreamFrame{Kind: contracts.StreamCleared, NewGeneration: cursor.Generation + 1}
	})
	a.send(t, contracts.Frame{Kind: contracts.FrameEvent, Event: &cleared})
	next3, cancel3 := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel3()
	frame, err = stream.Next(next3)
	if err != nil || frame.Kind != contracts.StreamCleared || frame.NewGeneration != cursor.Generation+1 {
		t.Fatalf("Next() = %+v, %v; want the preserved cleared body", frame, err)
	}
	_ = stream.Close()
	if frame := a.next(t); frame.Kind != contracts.FrameUnsubscribe {
		t.Fatalf("adapter received %s after Close, want unsubscribe", frame.Kind)
	}
}

func TestAdapterWireConnectionEndsItsStreams(t *testing.T) {
	f := setup(t, adapterwire.Options{})
	a, _ := f.open(t, f.provider)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	stream, err := port.Subscribe(f.ctx, contracts.Cursor{Tenant: f.tenant, Attempt: mustID[ids.Attempt](t), Operation: mustID[ids.Operation](t), Stream: "dispatch", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	a.conn.CloseNow()
	next, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if _, err := stream.Next(next); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next() after the connection closed error = %v, want the stream to end", err)
	}
}

// TestAdapterWireDrainsThenClosesAtTokenExpiry gives a connection a short
// token: it stops taking requests at the drain point and closes at expiry.
func TestAdapterWireDrainsThenClosesAtTokenExpiry(t *testing.T) {
	f := setup(t, adapterwire.Options{Drain: time.Second})
	subject := f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	a, _, err := f.dial(t, f.wire.Token(t, subject, 3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a.send(t, contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{1}}})
	_ = a.next(t)
	f.wire.WaitConnected(t, f.tenant, f.provider, contracts.RoleRuntime)
	port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
	time.Sleep(2300 * time.Millisecond)
	if _, err := port.Send(f.ctx, f.request(t)); !errors.Is(err, contracts.ErrNotDelivered) {
		t.Fatalf("Send() on a draining connection error = %v, want ErrNotDelivered", err)
	}
	if failure := a.waitClosed(t); failure != nil {
		t.Fatalf("expiry shutdown carried failure %+v, want a normal close", failure)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.wire.Registry.Connections(f.tenant, f.provider, contracts.RoleRuntime) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("an expired connection stayed registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAdapterWireKitRenewsBeforeExpiry runs a kit adapter whose tokens live
// a few seconds: the provider stays reachable across several expiries.
func TestAdapterWireKitRenewsBeforeExpiry(t *testing.T) {
	f := setup(t, adapterwire.Options{Drain: 500 * time.Millisecond})
	subject := f.wire.Bind(t, f.tenant, f.provider, contracts.RoleRuntime, nil)
	f.wire.ConnectWithTokens(t, subject, f.provider, &acceptAll{}, 3*time.Second)
	f.wire.WaitConnected(t, f.tenant, f.provider, contracts.RoleRuntime)
	deadline := time.Now().Add(8 * time.Second)
	sent := 0
	for time.Now().Before(deadline) {
		port := f.wire.Registry.Port(f.tenant, f.provider, contracts.RoleRuntime)
		if port == nil {
			t.Fatal("the provider had no live connection between renewals")
		}
		if _, err := port.Send(f.ctx, f.request(t)); err != nil {
			t.Fatalf("Send() during renewal error = %v", err)
		}
		sent++
		time.Sleep(100 * time.Millisecond)
	}
	if sent < 50 {
		t.Fatalf("sent %d requests, want a steady stream", sent)
	}
}

func TestAdapterWireDisabledBindingClosesLiveConnections(t *testing.T) {
	f := setup(t, adapterwire.Options{PingInterval: 100 * time.Millisecond})
	a, subject := f.open(t, f.provider)
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(f.ctx) }()
	binding, err := store.GetAdapterBinding(f.ctx, u, f.wire.Issuer().URL(), subject)
	if err != nil {
		t.Fatal(err)
	}
	binding.Enabled, binding.UpdatedAt = false, time.Now().UTC()
	if err := store.PutAdapterBinding(f.ctx, u, binding); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if failure := a.waitClosed(t); failure == nil || failure.Code != contracts.FailureUnauthenticated {
		t.Fatalf("shutdown failure = %+v, want unauthenticated", failure)
	}
	if _, response, err := f.dial(t, f.wire.Token(t, subject, time.Hour)); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reconnect after unbind = %v, %v; want 401", response, err)
	}
}

// acceptAll is a runtime port that accepts every request.
type acceptAll struct {
	sent atomic.Int64
}

func (p *acceptAll) Role() contracts.Role { return contracts.RoleRuntime }

func (p *acceptAll) Send(_ context.Context, request contracts.Request) (contracts.Reply, error) {
	p.sent.Add(1)
	return contracts.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted}, nil
}

func (p *acceptAll) Cancel(context.Context, contracts.Cancel) error { return nil }

func (p *acceptAll) Subscribe(context.Context, contracts.Cursor) (contracts.Stream, error) {
	return nil, io.EOF
}
