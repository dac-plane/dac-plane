package conformance

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

func scenarioHandshake(ctx context.Context, h *harness, _ Fixture) error {
	return h.open(ctx, reconnectTimeout)
}

func scenarioReply(ctx context.Context, h *harness, f Fixture) error {
	reply, err := h.exchange(ctx, delivery(f.Request))
	if err != nil {
		return err
	}
	h.first = &reply
	return nil
}

func scenarioRedelivery(ctx context.Context, h *harness, f Fixture) error {
	if h.first == nil {
		return errors.New("skipped: the first delivery got no reply")
	}
	first := *h.first
	again, err := h.exchange(ctx, delivery(f.Request))
	if err != nil {
		return err
	}
	if again.Outcome != first.Outcome || again.Durability != first.Durability {
		return fmt.Errorf("redelivery answered %s/%s, first delivery %s/%s", again.Outcome, again.Durability, first.Outcome, first.Durability)
	}
	return nil
}

func scenarioForeignMethod(ctx context.Context, h *harness, f Fixture) error {
	request := delivery(f.Request)
	request.Operation = newID()
	for _, role := range []wire.Role{wire.RoleRuntime, wire.RoleEnvironment, wire.RoleEnforcement, wire.RoleArtifactStore} {
		if role != f.Role {
			request.Role, request.Method, request.Arguments = role, wire.Methods(role)[0], nil
			break
		}
	}
	reply, err := h.exchange(ctx, request)
	if err != nil {
		return err
	}
	if reply.Outcome != wire.OutcomeFailed || reply.Durability != wire.DurabilityRejected || reply.Failure == nil || reply.Failure.Code != wire.FailureUnknownOperation {
		return fmt.Errorf("foreign method answered %s/%s %+v, want failed/rejected unknown_operation", reply.Outcome, reply.Durability, reply.Failure)
	}
	return nil
}

func scenarioCancel(ctx context.Context, h *harness, f Fixture) error {
	cancel := wire.Cancel{Operation: f.Request.Operation, Delivery: newID()}
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindCancel, Cancel: &cancel}); err != nil {
		return err
	}
	return h.ping(ctx)
}

func scenarioSubscribe(ctx context.Context, h *harness, f Fixture) error {
	if f.Cursor == nil {
		return nil
	}
	cursor := *f.Cursor
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindSubscribe, Subscribe: &wire.Subscribe{Cursor: cursor}}); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	frame, err := h.current.next(wait)
	if err != nil {
		return fmt.Errorf("waiting for the first event: %w", err)
	}
	if frame.Kind != wire.KindEvent {
		return fmt.Errorf("adapter sent a %s frame, want an event", frame.Kind)
	}
	event := frame.Event
	if event.Tenant != cursor.Tenant || event.Attempt != cursor.Attempt || event.Operation != cursor.Operation ||
		event.Stream != cursor.Stream || event.Provider != f.Provider || event.Generation != cursor.Generation {
		return fmt.Errorf("event binding %+v does not match the subscribed cursor", event)
	}
	if event.Body.Kind != wire.StreamConnected {
		return fmt.Errorf("first stream frame is %s, want connected", event.Body.Kind)
	}
	unsubscribe := wire.Unsubscribe{Operation: cursor.Operation, Stream: cursor.Stream}
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindUnsubscribe, Unsubscribe: &unsubscribe}); err != nil {
		return err
	}
	return h.ping(ctx)
}

func scenarioLiveness(ctx context.Context, h *harness, _ Fixture) error {
	return h.ping(ctx)
}

func scenarioReconnect(ctx context.Context, h *harness, _ Fixture) error {
	// A close at token expiry looks the same to the adapter.
	_ = h.current.conn.Close(websocket.StatusNormalClosure, "token expired")
	h.current = nil
	return h.open(ctx, reconnectTimeout)
}

func scenarioReconnectWithOpenStream(ctx context.Context, h *harness, f Fixture) error {
	if f.Cursor == nil {
		return nil
	}
	cursor := *f.Cursor
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindSubscribe, Subscribe: &wire.Subscribe{Cursor: cursor}}); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	frame, err := h.current.next(wait)
	if err != nil {
		return fmt.Errorf("waiting for the first event: %w", err)
	}
	if frame.Kind != wire.KindEvent || frame.Event.Body.Kind != wire.StreamConnected {
		return fmt.Errorf("adapter sent %+v, want a connected event", frame)
	}
	// The stream stays open: a supported adapter keeps reading it. The kit
	// must still reconnect after the connection closes, which requires its
	// session teardown to cancel stream handlers before waiting on them.
	_ = h.current.conn.Close(websocket.StatusNormalClosure, "token expired")
	h.current = nil
	return h.open(ctx, reconnectTimeout)
}

func scenarioMalformed(ctx context.Context, h *harness, _ Fixture) error {
	if err := h.current.conn.Write(ctx, websocket.MessageText, []byte(`{"kind":"notify"}`)); err != nil {
		return err
	}
	if err := h.expectClose(ctx, wire.FailureMalformedFrame); err != nil {
		return err
	}
	return h.open(ctx, reconnectTimeout)
}

func scenarioBinary(ctx context.Context, h *harness, _ Fixture) error {
	if err := h.current.conn.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02}); err != nil {
		return err
	}
	if err := h.expectClose(ctx, wire.FailureMalformedFrame); err != nil {
		return err
	}
	return h.open(ctx, reconnectTimeout)
}

func scenarioWrongDirection(ctx context.Context, h *harness, f Fixture) error {
	reply := wire.Reply{Operation: f.Request.Operation, Delivery: newID(), Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted}
	if err := h.current.write(ctx, wire.Frame{Kind: wire.KindReply, Reply: &reply}); err != nil {
		return err
	}
	if err := h.expectClose(ctx, wire.FailureMalformedFrame); err != nil {
		return err
	}
	return h.open(ctx, reconnectTimeout)
}

func scenarioVersionMismatch(ctx context.Context, h *harness, _ Fixture) error {
	// Leave the open session; the adapter's next connection is the test.
	_ = h.current.conn.Close(websocket.StatusNormalClosure, "")
	conn, hello, err := h.accept(ctx, reconnectTimeout)
	if err != nil {
		return err
	}
	var unoffered uint32 = 1
	for _, version := range hello.Versions {
		unoffered = max(unoffered, version+1)
	}
	if err := conn.write(ctx, wire.Frame{Kind: wire.KindLifecycle, Lifecycle: &wire.Lifecycle{Phase: wire.PhaseReady, Version: unoffered}}); err != nil {
		conn.conn.CloseNow()
		return err
	}
	h.current = conn
	if err := h.expectClose(ctx, wire.FailureProtocolMismatch); err != nil {
		return err
	}
	return h.open(ctx, reconnectTimeout)
}

// delivery returns the fixture request as a new delivery.
func delivery(request wire.Request) wire.Request {
	request.Delivery = newID()
	request.Deadline = time.Now().Add(stepTimeout).UTC().Truncate(time.Millisecond)
	return request
}

// newID returns a UUIDv7.
func newID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	binary.BigEndian.PutUint64(id[:8], uint64(time.Now().UnixMilli())<<16|uint64(id[6])<<8|uint64(id[7]))
	id[6] = 0x70 | id[6]&0x0f
	id[8] = 0x80 | id[8]&0x3f
	text := hex.EncodeToString(id[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}
