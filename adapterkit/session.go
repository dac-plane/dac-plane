package adapterkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

// ErrUnauthenticated reports that the control plane refused the token or the
// identity it names.
var ErrUnauthenticated = errors.New("adapterkit: control plane refused the adapter's credentials")

// maxFailureMessage bounds a failure message, as the protocol does.
const maxFailureMessage = 1024

// session is one negotiated connection.
type session struct {
	config  Config
	conn    *websocket.Conn
	expiry  time.Time
	mu      sync.Mutex
	streams map[streamKey]*openStream
}

type streamKey struct{ operation, stream string }

type openStream struct{ cancel context.CancelFunc }

// connect dials, upgrades and negotiates one session.
func connect(ctx context.Context, config Config, client *http.Client) (*session, error) {
	hctx, cancel := context.WithTimeout(ctx, config.HandshakeTimeout)
	defer cancel()
	token, err := config.Token(hctx)
	if err != nil {
		return nil, fmt.Errorf("adapterkit: token: %w", err)
	}
	if token.Value == "" || !token.Expiry.After(time.Now()) {
		return nil, errors.New("adapterkit: token source returned an empty or expired token")
	}
	conn, response, err := websocket.Dial(hctx, config.URL, &websocket.DialOptions{
		HTTPClient:   client,
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token.Value}},
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthenticated
		}
		// The error names the URL and status, never the token.
		return nil, fmt.Errorf("adapterkit: dial: %w", err)
	}
	s := &session{config: config, conn: conn, expiry: token.Expiry, streams: map[streamKey]*openStream{}}
	if conn.Subprotocol() != wire.Subprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol not selected")
		return nil, errors.New("adapterkit: control plane did not select the ControlFrame subprotocol")
	}
	conn.SetReadLimit(config.MaxFrameBytes)
	if err := s.negotiate(hctx); err != nil {
		return nil, err
	}
	config.Logger.Info("adapter connected", "provider", config.Provider)
	return s, nil
}

// negotiate sends hello and accepts only a ready the hello allows.
func (s *session) negotiate(ctx context.Context) error {
	hello := wire.Lifecycle{Phase: wire.PhaseHello, Versions: s.config.Versions, Offered: s.config.Offered, Required: s.config.Required}
	if err := s.write(ctx, wire.Frame{Kind: wire.KindLifecycle, Lifecycle: &hello}); err != nil {
		s.conn.CloseNow()
		return err
	}
	frame, err := s.read(ctx)
	if err != nil {
		s.conn.CloseNow()
		return err
	}
	if frame.Kind != wire.KindLifecycle {
		return s.refuse(ctx, wire.FailureProtocolMismatch, fmt.Sprintf("%s frame before ready", frame.Kind))
	}
	ready := frame.Lifecycle
	switch ready.Phase {
	case wire.PhaseShutdown:
		s.conn.CloseNow()
		if ready.Failure != nil {
			return fmt.Errorf("adapterkit: control plane refused the session: %w", ready.Failure)
		}
		return errors.New("adapterkit: control plane refused the session")
	case wire.PhaseReady:
	default:
		return s.refuse(ctx, wire.FailureProtocolMismatch, "expected ready")
	}
	if !slices.Contains(s.config.Versions, ready.Version) {
		return s.refuse(ctx, wire.FailureProtocolMismatch, "selected version was not offered")
	}
	for _, capability := range ready.Selected {
		if !slices.Contains(s.config.Offered, capability) {
			return s.refuse(ctx, wire.FailureUnsupportedCapability, "selected capability was not offered")
		}
	}
	for _, capability := range s.config.Required {
		if !slices.Contains(ready.Selected, capability) {
			return s.refuse(ctx, wire.FailureUnsupportedCapability, "required capability was not selected")
		}
	}
	return nil
}

// refuse sends a shutdown with a typed failure, closes the connection and
// returns the failure.
func (s *session) refuse(ctx context.Context, code, message string) error {
	failure := &wire.Failure{Code: code, Message: bounded(message)}
	_ = s.write(ctx, wire.Shutdown(failure))
	_ = s.conn.Close(websocket.StatusPolicyViolation, code)
	return fmt.Errorf("adapterkit: %w", failure)
}

// serve handles frames until the connection ends. It signals renew once when
// the token nears expiry.
func (s *session) serve(ctx context.Context, renew chan<- struct{}) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.conn.CloseNow()
	go s.keepAlive(ctx)
	go s.scheduleRenewal(ctx, renew)
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		handlers.Wait()
	}()
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.config.Logger.Info("adapter connection closed", "provider", s.config.Provider, "status", websocket.CloseStatus(err))
			}
			return
		}
		if typ != websocket.MessageText {
			_ = s.refuse(ctx, wire.FailureMalformedFrame, "frames are text messages")
			return
		}
		frame, err := wire.Decode(data)
		if err != nil {
			_ = s.refuse(ctx, wire.FailureMalformedFrame, err.Error())
			return
		}
		switch frame.Kind {
		case wire.KindRequest:
			handlers.Go(func() { s.handleRequest(ctx, *frame.Request) })
		case wire.KindCancel:
			handlers.Go(func() {
				if err := s.config.Handler.Cancel(ctx, *frame.Cancel); err != nil {
					s.config.Logger.Warn("cancel failed", "operation", frame.Cancel.Operation, "error", err)
				}
			})
		case wire.KindSubscribe:
			s.subscribe(ctx, &handlers, frame.Subscribe.Cursor)
		case wire.KindUnsubscribe:
			s.unsubscribe(streamKey{frame.Unsubscribe.Operation, frame.Unsubscribe.Stream})
		case wire.KindLifecycle:
			if frame.Lifecycle.Phase == wire.PhaseShutdown {
				_ = s.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			_ = s.refuse(ctx, wire.FailureProtocolMismatch, "lifecycle frame after the session opened")
			return
		default:
			// Replies and events travel from the adapter, never to it.
			_ = s.refuse(ctx, wire.FailureMalformedFrame, fmt.Sprintf("%s frames are not sent to an adapter", frame.Kind))
			return
		}
	}
}

// handleRequest answers one delivery. A request for another role or an
// unknown method is rejected before the handler sees it.
func (s *session) handleRequest(ctx context.Context, request wire.Request) {
	if request.Role != s.config.Role || !slices.Contains(wire.Methods(s.config.Role), request.Method) {
		s.reply(ctx, failedReply(request, wire.DurabilityRejected, wire.FailureUnknownOperation, fmt.Sprintf("%s is not a %s method of this adapter", request.Method, s.config.Role)))
		return
	}
	rctx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	reply, err := s.config.Handler.Send(rctx, request)
	switch {
	case err != nil:
		reply = failedReply(request, wire.DurabilityUnknown, wire.FailureProviderFailed, err.Error())
	case reply.Operation != request.Operation || reply.Delivery != request.Delivery:
		reply = failedReply(request, wire.DurabilityUnknown, wire.FailureProviderFailed, "handler answered another delivery")
	}
	s.reply(ctx, reply)
}

func (s *session) reply(ctx context.Context, reply wire.Reply) {
	frame := wire.Frame{Kind: wire.KindReply, Reply: &reply}
	if err := wire.Validate(frame); err != nil {
		// An invalid reply would end the session at the control plane; the
		// outcome is unknown instead.
		s.config.Logger.Error("handler produced an invalid reply", "operation", reply.Operation, "error", err)
		fallback := failedReply(wire.Request{Operation: reply.Operation, Delivery: reply.Delivery}, wire.DurabilityUnknown, wire.FailureProviderFailed, "handler produced an invalid reply")
		frame = wire.Frame{Kind: wire.KindReply, Reply: &fallback}
	}
	if err := s.write(ctx, frame); err != nil {
		s.config.Logger.Warn("reply not delivered", "operation", reply.Operation, "error", err)
	}
}

func failedReply(request wire.Request, durability, code, message string) wire.Reply {
	return wire.Reply{
		Operation: request.Operation, Delivery: request.Delivery, Outcome: wire.OutcomeFailed, Durability: durability,
		Failure: &wire.Failure{Code: code, Message: bounded(message)},
	}
}

// subscribe opens a stream, replacing any open stream with the same key.
func (s *session) subscribe(ctx context.Context, handlers *sync.WaitGroup, cursor wire.Cursor) {
	key := streamKey{cursor.Operation, cursor.Stream}
	sctx, cancel := context.WithCancel(ctx)
	opened := &openStream{cancel: cancel}
	s.mu.Lock()
	if previous := s.streams[key]; previous != nil {
		previous.cancel()
	}
	s.streams[key] = opened
	s.mu.Unlock()
	handlers.Go(func() {
		defer s.forget(key, opened)
		stream, err := s.config.Handler.Subscribe(sctx, cursor)
		if err != nil {
			s.config.Logger.Warn("subscribe failed", "operation", cursor.Operation, "stream", cursor.Stream, "error", err)
			return
		}
		defer func() { _ = stream.Close() }()
		for {
			body, err := stream.Next(sctx)
			if err != nil {
				return
			}
			event := wire.Event{
				Tenant: cursor.Tenant, Attempt: cursor.Attempt, Provider: s.config.Provider, Operation: cursor.Operation,
				Stream: cursor.Stream, Generation: cursor.Generation, Body: body,
			}
			if err := s.write(sctx, wire.Frame{Kind: wire.KindEvent, Event: &event}); err != nil {
				if !errors.Is(err, wire.ErrMalformed) {
					return
				}
				s.config.Logger.Error("handler produced an invalid stream frame", "operation", cursor.Operation, "error", err)
				return
			}
		}
	})
}

func (s *session) unsubscribe(key streamKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if open := s.streams[key]; open != nil {
		open.cancel()
		delete(s.streams, key)
	}
}

func (s *session) forget(key streamKey, opened *openStream) {
	opened.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[key] == opened {
		delete(s.streams, key)
	}
}

// keepAlive pings the control plane; a missed pong ends the session.
func (s *session) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(s.config.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, s.config.PongTimeout)
			err := s.conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.config.Logger.Warn("control plane missed a pong; closing", "provider", s.config.Provider)
				s.conn.CloseNow()
				return
			}
		}
	}
}

// scheduleRenewal signals once, RenewBefore (at most half the remaining
// lifetime) before the token expires.
func (s *session) scheduleRenewal(ctx context.Context, renew chan<- struct{}) {
	remaining := time.Until(s.expiry)
	lead := min(s.config.RenewBefore, remaining/2)
	timer := time.NewTimer(remaining - lead)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		select {
		case renew <- struct{}{}:
		default:
		}
	}
}

func (s *session) write(ctx context.Context, frame wire.Frame) error {
	data, err := wire.Encode(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, s.config.PongTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

func (s *session) read(ctx context.Context) (wire.Frame, error) {
	typ, data, err := s.conn.Read(ctx)
	if err != nil {
		return wire.Frame{}, fmt.Errorf("adapterkit: read: %w", err)
	}
	if typ != websocket.MessageText {
		return wire.Frame{}, fmt.Errorf("%w: frames are text messages", wire.ErrMalformed)
	}
	return wire.Decode(data)
}

func bounded(message string) string {
	if message == "" {
		return "unspecified"
	}
	if len(message) > maxFailureMessage {
		return message[:maxFailureMessage]
	}
	return message
}
