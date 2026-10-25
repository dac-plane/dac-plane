package adapterwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// ErrConnectionClosed reports a connection that ended while a request
// awaited its reply or a stream was open. The request's outcome is unknown.
var ErrConnectionClosed = errors.New("adapter connection closed")

type pendingKey struct {
	operation ids.OperationID
	delivery  ids.DeliveryID
}

type streamKey struct {
	operation ids.OperationID
	stream    string
}

// connection is one negotiated adapter connection.
type connection struct {
	registry     *Registry
	conn         *websocket.Conn
	registration contracts.ProviderRegistration
	peer         contracts.Peer
	issuer       string
	subject      string
	expiry       time.Time
	drainAt      time.Time
	logger       *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	pending  map[pendingKey]chan contracts.Reply
	streams  map[streamKey]*wireStream
	draining bool
	closed   bool
}

func newConnection(registry *Registry, conn *websocket.Conn, registration contracts.ProviderRegistration, verified authn.Verified, logger *slog.Logger) *connection {
	options := registry.options
	now := options.Now()
	drain := min(options.Drain, verified.Expiry.Sub(now)/2)
	ctx, cancel := context.WithCancel(registry.ctx)
	conn.SetReadLimit(options.MaxFrameBytes)
	return &connection{
		registry: registry, conn: conn, registration: registration,
		peer:   contracts.Peer{Tenant: registration.Tenant, Provider: registration.Provider},
		issuer: verified.Issuer, subject: verified.Subject, expiry: verified.Expiry, drainAt: verified.Expiry.Add(-drain),
		logger: logger.With("tenant", registration.Tenant.String(), "provider", registration.Provider.String(), "role", string(registration.Role)),
		ctx:    ctx, cancel: cancel,
		pending: map[pendingKey]chan contracts.Reply{}, streams: map[streamKey]*wireStream{},
	}
}

// serve negotiates, registers the connection and reads until it ends.
func (c *connection) serve() {
	defer c.close(ErrConnectionClosed)
	if err := c.negotiate(); err != nil {
		c.logger.Warn("adapter negotiation refused", "reason", reasonOf(err))
		return
	}
	c.registry.add(c)
	c.logger.Info("adapter connected", "expires_at", c.expiry)
	go c.maintain()
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				c.logger.Info("adapter connection closed", "status", int(websocket.CloseStatus(err)))
			}
			return
		}
		if typ != websocket.MessageText {
			c.refuse(contracts.FailureMalformedFrame, "frames are text messages")
			return
		}
		frame, err := decodeFrame(data)
		if err != nil {
			c.refuse(contracts.FailureMalformedFrame, err.Error())
			return
		}
		switch frame.Kind {
		case contracts.FrameReply:
			c.deliverReply(*frame.Reply)
		case contracts.FrameEvent:
			c.deliverEvent(*frame.Event)
		case contracts.FrameLifecycle:
			if frame.Lifecycle.Phase == contracts.PhaseShutdown {
				c.logger.Info("adapter shut the session down", "failure", failureCode(frame.Lifecycle.Failure))
				_ = c.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			c.refuse(contracts.FailureProtocolMismatch, "lifecycle frame after the session opened")
			return
		default:
			// ADR-0010: an adapter sends only lifecycle, reply and event frames.
			c.refuse(contracts.FailureMalformedFrame, fmt.Sprintf("%s frames are not sent by an adapter", frame.Kind))
			return
		}
	}
}

// negotiate admits the adapter's hello through the protocol gate and answers
// with ready or a shutdown.
func (c *connection) negotiate() error {
	gate, err := contracts.NewGate(c.registry.options.Hello)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.registry.options.HandshakeTimeout)
	defer cancel()
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("%w: no hello", ErrConnectionClosed)
	}
	var frame contracts.Frame
	if typ != websocket.MessageText {
		frame = contracts.Frame{Kind: "binary"}
	} else if decoded, err := decodeFrame(data); err == nil {
		frame = decoded
	}
	answer, failure := gate.Receive(frame)
	if answer != nil {
		if err := c.write(ctx, *answer); err != nil {
			return err
		}
	}
	if failure != nil {
		_ = c.conn.Close(websocket.StatusPolicyViolation, string(failure.Code))
		return failure
	}
	if answer == nil {
		// The adapter shut the session down instead of saying hello.
		return fmt.Errorf("%w: shutdown before hello", ErrConnectionClosed)
	}
	return nil
}

// maintain pings, re-checks the binding, drains before token expiry and
// closes at expiry.
func (c *connection) maintain() {
	options := c.registry.options
	ticker := time.NewTicker(options.PingInterval)
	defer ticker.Stop()
	drain := time.NewTimer(time.Until(c.drainAt))
	defer drain.Stop()
	expiry := time.NewTimer(time.Until(c.expiry))
	defer expiry.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-drain.C:
			c.mu.Lock()
			c.draining = true
			c.mu.Unlock()
			c.logger.Info("adapter connection draining before token expiry")
		case <-expiry.C:
			c.logger.Info("adapter connection closed at token expiry")
			c.shutdown(websocket.StatusNormalClosure, nil, "token expired")
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, options.PongTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				if c.ctx.Err() == nil {
					c.logger.Warn("adapter missed a pong; closing")
					c.conn.CloseNow()
				}
				return
			}
			if !c.bindingHolds() {
				c.logger.Warn("adapter binding no longer holds; closing")
				c.shutdown(websocket.StatusPolicyViolation, &contracts.Failure{Code: contracts.FailureUnauthenticated, Message: "adapter binding disabled"}, "binding disabled")
				return
			}
		}
	}
}

// bindingHolds re-resolves the binding. An operational failure keeps the
// connection: it neither proves nor disproves revocation.
func (c *connection) bindingHolds() bool {
	registration, err := c.registry.resolve(c.ctx, c.issuer, c.subject)
	if err != nil {
		return !isNotFound(err)
	}
	return registration.Tenant == c.registration.Tenant && registration.Provider == c.registration.Provider && registration.Role == c.registration.Role
}

// send writes a request and waits for its reply on this connection.
func (c *connection) send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	key := pendingKey{request.Operation, request.Delivery}
	reply := make(chan contracts.Reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return contracts.Reply{}, contracts.ErrNotDelivered
	}
	if _, duplicate := c.pending[key]; duplicate {
		c.mu.Unlock()
		return contracts.Reply{}, fmt.Errorf("%w: delivery already in flight", contracts.ErrNotDelivered)
	}
	c.pending[key] = reply
	c.mu.Unlock()
	defer c.forgetPending(key)
	if err := c.write(ctx, contracts.Frame{Kind: contracts.FrameRequest, Request: &request}); err != nil {
		return contracts.Reply{}, fmt.Errorf("adapter request write: %w", err)
	}
	select {
	case answered := <-reply:
		return answered, nil
	case <-c.ctx.Done():
		return contracts.Reply{}, ErrConnectionClosed
	case <-ctx.Done():
		return contracts.Reply{}, ctx.Err()
	}
}

func (c *connection) forgetPending(key pendingKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
}

// deliverReply hands a reply to the request that awaits it on this
// connection. A reply nothing awaits, such as a duplicate or one that came
// after its request gave up, is dropped.
func (c *connection) deliverReply(reply contracts.Reply) {
	key := pendingKey{reply.Operation, reply.Delivery}
	c.mu.Lock()
	waiting, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		c.registry.droppedReplies.Add(1)
		c.logger.Debug("dropped a reply no request awaits", "operation", reply.Operation.String())
		return
	}
	waiting <- reply
}

// subscribe opens a stream on this connection, replacing an open stream with
// the same operation and stream name.
func (c *connection) subscribe(ctx context.Context, cursor contracts.Cursor) (*wireStream, error) {
	stream := &wireStream{
		connection: c, cursor: cursor, key: streamKey{cursor.Operation, cursor.Stream},
		frames: make(chan contracts.StreamFrame, c.registry.options.StreamBuffer), ended: make(chan struct{}),
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, contracts.ErrNotDelivered
	}
	previous := c.streams[stream.key]
	c.streams[stream.key] = stream
	c.mu.Unlock()
	if previous != nil {
		previous.end(errors.New("adapter stream replaced by a new subscription"))
	}
	if err := c.write(ctx, contracts.Frame{Kind: contracts.FrameSubscribe, Subscribe: &contracts.Subscribe{Cursor: cursor}}); err != nil {
		c.forgetStream(stream)
		return nil, fmt.Errorf("adapter subscribe write: %w", err)
	}
	return stream, nil
}

// deliverEvent routes an event to its open stream after checking the claimed
// binding against this connection's peer and the subscribed cursor. Events
// that fail the check or match no stream are dropped, never applied.
func (c *connection) deliverEvent(event contracts.Event) {
	key := streamKey{event.Operation, event.Stream}
	c.mu.Lock()
	stream := c.streams[key]
	c.mu.Unlock()
	if stream == nil {
		c.registry.droppedEvents.Add(1)
		return
	}
	operation := lifecycle.ProviderOperation{ID: stream.cursor.Operation, Tenant: stream.cursor.Tenant, Attempt: stream.cursor.Attempt, Provider: c.peer.Provider}
	if _, failure := contracts.BindEvent(c.peer, event, operation); failure != nil || event.Stream != stream.cursor.Stream {
		c.registry.droppedEvents.Add(1)
		c.logger.Warn("dropped an event bound to another stream", "reason", failureCode(failure))
		return
	}
	frame := event.Body
	switch {
	case event.Generation < stream.cursor.Generation:
		// An event from an older generation is stale: Ingest would ignore
		// it, so the boundary drops it instead of letting the reconciler
		// relabel it with the subscribed cursor's generation.
		c.registry.droppedEvents.Add(1)
		c.logger.Warn("dropped an event from an older stream generation", "operation", event.Operation.String(), "event_generation", event.Generation, "cursor_generation", stream.cursor.Generation)
		return
	case event.Body.Kind == contracts.StreamCleared:
		// A cleared body survives a newer tagged generation: its NewGeneration
		// names where the durable cursor advances to, and Ingest refuses one
		// that does not exceed the cursor, so nothing else is accepted.
	case event.Generation > stream.cursor.Generation:
		// The adapter moved past the subscribed generation without a clear.
		// Signal rehydration exactly as a cursor_too_old frame would, so the
		// reconciler resubscribes from zero instead of applying rows the
		// durable cursor has not advanced to.
		frame = contracts.StreamFrame{Kind: contracts.StreamCursorTooOld}
	}
	select {
	case stream.frames <- frame:
	default:
		// The reconciler fell behind: end the stream rather than block every
		// reply behind it. It resumes from its durable cursor.
		stream.end(errors.New("adapter stream buffer overflowed"))
	}
}

func (c *connection) forgetStream(stream *wireStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams[stream.key] == stream {
		delete(c.streams, stream.key)
	}
}

// load reports whether the connection takes a new request and its in-flight
// count.
func (c *connection) load() (open bool, inFlight int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.draining && len(c.pending) < c.registry.options.MaxInFlight, len(c.pending)
}

func (c *connection) live() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

// refuse ends the session for a protocol violation.
func (c *connection) refuse(code contracts.FailureCode, message string) {
	c.logger.Warn("adapter session refused", "reason", string(code))
	c.shutdown(websocket.StatusPolicyViolation, &contracts.Failure{Code: code, Message: bounded(message)}, string(code))
}

// shutdown sends a shutdown frame and closes the connection.
func (c *connection) shutdown(status websocket.StatusCode, failure *contracts.Failure, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), c.registry.options.PongTimeout)
	defer cancel()
	_ = c.write(ctx, contracts.Frame{Kind: contracts.FrameLifecycle, Lifecycle: &contracts.Lifecycle{Phase: contracts.PhaseShutdown, Failure: failure}})
	_ = c.conn.Close(status, reason)
	c.close(ErrConnectionClosed)
}

// close ends the connection once: it leaves the registry, every request
// awaiting a reply fails, and every open stream ends.
func (c *connection) close(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	streams := c.streams
	c.streams = map[streamKey]*wireStream{}
	c.mu.Unlock()
	c.cancel()
	c.registry.remove(c)
	for _, stream := range streams {
		stream.end(cause)
	}
	c.conn.CloseNow()
}

func (c *connection) write(ctx context.Context, frame contracts.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, c.registry.options.PongTimeout)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, data)
}

// decodeFrame parses one frame strictly: one JSON object, no unknown fields,
// no trailing data, and a valid frame.
func decodeFrame(data []byte) (contracts.Frame, error) {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return contracts.Frame{}, errors.New("a frame is one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var frame contracts.Frame
	if err := decoder.Decode(&frame); err != nil {
		return contracts.Frame{}, fmt.Errorf("frame does not decode: %w", err)
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return contracts.Frame{}, errors.New("a message carries exactly one frame")
	}
	return frame, frame.Validate()
}

const maxFailureMessage = 1024

func bounded(message string) string {
	if message == "" {
		return "unspecified"
	}
	if len(message) > maxFailureMessage {
		return message[:maxFailureMessage]
	}
	return message
}

func failureCode(failure *contracts.Failure) string {
	if failure == nil {
		return ""
	}
	return string(failure.Code)
}

func reasonOf(err error) string {
	var failure *contracts.Failure
	if errors.As(err, &failure) {
		return string(failure.Code)
	}
	return "closed"
}
