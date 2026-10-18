package simulator

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// StreamScript is the ordered frame history one operation's stream yields.
// Subscribe serves exactly these frames; the consumer still validates and
// binds them, so a script may deliberately carry malformed frames.
type StreamScript struct {
	Frames []contracts.StreamFrame
}

// ConnectedFrame is the opening frame of a subscription.
func ConnectedFrame(at time.Time) contracts.StreamFrame {
	return contracts.StreamFrame{Kind: contracts.StreamConnected, ServerTime: at}
}

// HeartbeatFrame is a liveness frame that carries no entries and advances no
// cursor.
func HeartbeatFrame(at time.Time) contracts.StreamFrame {
	return contracts.StreamFrame{Kind: contracts.StreamHeartbeat, ServerTime: at}
}

// ClearedFrame announces a new generation: everything before it is void and
// the consumer must rehydrate.
func ClearedFrame(generation uint64) contracts.StreamFrame {
	return contracts.StreamFrame{Kind: contracts.StreamCleared, NewGeneration: generation}
}

// CursorTooOldFrame tells the consumer its cursor fell out of the provider's
// retention and it must rehydrate.
func CursorTooOldFrame() contracts.StreamFrame {
	return contracts.StreamFrame{Kind: contracts.StreamCursorTooOld}
}

// Row is one entry in a scripted rows frame; the frame builder computes the
// payload hash from the payload.
type Row struct {
	Sequence uint64
	Updated  uint64
	Kind     string
	Payload  string
}

// Tombstone is one deletion in a scripted rows frame.
type Tombstone struct {
	Sequence uint64
	Updated  uint64
}

// RowsFrame is a batch of entries whose payload hashes match their payloads.
// It neither reorders nor validates: scripted frames are the scenario's
// business and contract validation is the consumer's job.
func RowsFrame(replay bool, rows ...Row) contracts.StreamFrame {
	entries := make([]contracts.Entry, len(rows))
	for i, row := range rows {
		payload := json.RawMessage(row.Payload)
		entries[i] = contracts.Entry{
			Sequence:        row.Sequence,
			UpdatedSequence: row.Updated,
			Kind:            row.Kind,
			PayloadHash:     identity.DigestOf(payload),
			Payload:         payload,
		}
	}
	return contracts.StreamFrame{Kind: contracts.StreamRows, Rows: &contracts.Rows{Entries: entries, Replay: replay}}
}

// TombstonesFrame is a batch of deletions.
func TombstonesFrame(replay bool, tombstones ...Tombstone) contracts.StreamFrame {
	deleted := make([]contracts.Tombstone, len(tombstones))
	for i, tombstone := range tombstones {
		deleted[i] = contracts.Tombstone{Sequence: tombstone.Sequence, UpdatedSequence: tombstone.Updated}
	}
	return contracts.StreamFrame{Kind: contracts.StreamRows, Rows: &contracts.Rows{Tombstones: deleted, Replay: replay}}
}

// scriptStream serves one scripted frame list as a contracts.Stream.
type scriptStream struct {
	control   *Control
	operation ids.OperationID
	frames    []contracts.StreamFrame
	index     int
	closed    bool
	mu        sync.Mutex
}

func (s *scriptStream) Next(ctx context.Context) (contracts.StreamFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.index >= len(s.frames) {
		return contracts.StreamFrame{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return contracts.StreamFrame{}, err
	}
	if err := s.control.reach(ctx, s.operation, BeforeFrame); err != nil {
		return contracts.StreamFrame{}, err
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (s *scriptStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
