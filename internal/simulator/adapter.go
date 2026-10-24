package simulator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
)

// Handler serves a provider port through the adapter kit, so a simulator runs
// as a reference adapter on the wire transport (ADR-0008). Frames cross as
// JSON and are validated on both sides: the kit against the frame schemas,
// this bridge against the Go contracts.
func Handler(port contracts.Port) adapterkit.Handler {
	return &handler{port: port}
}

type handler struct {
	port contracts.Port
}

func (h *handler) Send(ctx context.Context, request wire.Request) (wire.Reply, error) {
	var in contracts.Request
	if err := convert(request, &in); err != nil {
		return wire.Reply{}, err
	}
	reply, err := h.port.Send(ctx, in)
	if err != nil {
		return wire.Reply{}, err
	}
	var out wire.Reply
	return out, convert(reply, &out)
}

func (h *handler) Cancel(ctx context.Context, cancel wire.Cancel) error {
	var in contracts.Cancel
	if err := convert(cancel, &in); err != nil {
		return err
	}
	return h.port.Cancel(ctx, in)
}

func (h *handler) Subscribe(ctx context.Context, cursor wire.Cursor) (adapterkit.Stream, error) {
	var in contracts.Cursor
	if err := convert(cursor, &in); err != nil {
		return nil, err
	}
	stream, err := h.port.Subscribe(ctx, in)
	if err != nil {
		return nil, err
	}
	return &bridgedStream{stream: stream}, nil
}

type bridgedStream struct {
	stream contracts.Stream
}

func (s *bridgedStream) Next(ctx context.Context) (wire.StreamFrame, error) {
	frame, err := s.stream.Next(ctx)
	if err != nil {
		return wire.StreamFrame{}, err
	}
	var out wire.StreamFrame
	return out, convert(frame, &out)
}

func (s *bridgedStream) Close() error {
	return s.stream.Close()
}

// convert re-encodes a value between the kit and contract types, which share
// one JSON form.
func convert(from, to any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	if err := json.Unmarshal(data, to); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}
	return nil
}
