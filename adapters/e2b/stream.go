package e2badapter

import (
	"context"
	"io"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

type observationStream struct {
	store     *journal.Store
	operation string
	after     uint64
	connected bool
	closed    bool
}

func (s *observationStream) Next(ctx context.Context) (wire.StreamFrame, error) {
	if s.closed {
		return wire.StreamFrame{}, io.EOF
	}
	if s.connected {
		s.connected = false
		return wire.StreamFrame{Kind: wire.StreamConnected, ServerTime: time.Now().UTC()}, nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		entries, err := s.store.EventsAfter(s.operation, s.after)
		if err != nil {
			return wire.StreamFrame{}, err
		}
		if len(entries) > 0 {
			s.after = entries[len(entries)-1].UpdatedSequence
			return wire.StreamFrame{Kind: wire.StreamRows, ServerTime: time.Now().UTC(), Rows: &wire.Rows{Entries: entries}}, nil
		}
		select {
		case <-ctx.Done():
			return wire.StreamFrame{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *observationStream) Close() error { s.closed = true; return nil }
