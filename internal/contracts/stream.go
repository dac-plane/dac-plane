package contracts

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Cursor is ingestion progress on one provider stream: the stream's binding,
// its generation and the highest updated sequence ingested. Acknowledging a
// cursor records ingestion only; it proves no provider effect and completes no
// lifecycle transition.
type Cursor struct {
	Tenant     ids.TenantID    `json:"tenant_id"`
	Attempt    ids.AttemptID   `json:"attempt_id"`
	Operation  ids.OperationID `json:"operation_id"`
	Stream     string          `json:"stream"`
	Generation uint64          `json:"generation"`
	After      uint64          `json:"after_updated_sequence"`
}

// Validate reports whether the cursor is complete.
func (c Cursor) Validate() error {
	if c.Tenant.IsZero() || c.Attempt.IsZero() || c.Operation.IsZero() || c.Generation == 0 || !validName(c.Stream) {
		return fmt.Errorf("%w: cursor requires its binding, a named stream and a generation", ErrInvalid)
	}
	return nil
}

// StreamKind is the kind of a stream frame.
type StreamKind string

const (
	StreamConnected    StreamKind = "connected"
	StreamRows         StreamKind = "rows"
	StreamCleared      StreamKind = "cleared"
	StreamCursorTooOld StreamKind = "cursor_too_old"
	StreamHeartbeat    StreamKind = "heartbeat"
)

// StreamKinds lists every StreamKind.
func StreamKinds() []StreamKind {
	return []StreamKind{StreamConnected, StreamRows, StreamCleared, StreamCursorTooOld, StreamHeartbeat}
}

// Entry is one revision of a logical stream entry. Sequence identifies the
// entry; UpdatedSequence orders its revisions across the generation. The
// payload is inline, and must hash to PayloadHash, or omitted with only its
// hash, to be fetched by hash.
type Entry struct {
	Sequence        uint64          `json:"sequence"`
	UpdatedSequence uint64          `json:"updated_sequence"`
	Kind            string          `json:"kind"`
	PayloadHash     identity.Digest `json:"payload_hash"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	PayloadOmitted  bool            `json:"payload_omitted,omitempty"`
}

// Validate reports whether the entry is ordered, named and carries exactly one
// of an inline payload matching its hash or an omission marker.
func (e Entry) Validate() error {
	if e.Sequence == 0 || e.UpdatedSequence < e.Sequence || !validName(e.Kind) {
		return fmt.Errorf("%w: entry requires a sequence, a later or equal updated sequence and a kind", ErrInvalid)
	}
	if err := e.PayloadHash.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if e.PayloadOmitted == (len(e.Payload) > 0) {
		return fmt.Errorf("%w: an entry carries its payload or marks it omitted", ErrInvalid)
	}
	if !e.PayloadOmitted && identity.DigestOf(e.Payload) != e.PayloadHash {
		return fmt.Errorf("%w: entry payload does not match its hash", ErrInvalid)
	}
	return nil
}

// Tombstone records that an entry was deleted at UpdatedSequence. Deletions
// are part of the log; nothing is rewritten.
type Tombstone struct {
	Sequence        uint64 `json:"sequence"`
	UpdatedSequence uint64 `json:"updated_sequence"`
}

// Validate reports whether the tombstone is ordered.
func (t Tombstone) Validate() error {
	if t.Sequence == 0 || t.UpdatedSequence < t.Sequence {
		return fmt.Errorf("%w: tombstone requires a sequence and a later or equal updated sequence", ErrInvalid)
	}
	return nil
}

// Rows is a batch of entries and tombstones, each list in strictly increasing
// updated-sequence order. Replay marks rows resent from history.
type Rows struct {
	Entries    []Entry     `json:"entries,omitempty"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
	Replay     bool        `json:"replay,omitempty"`
}

// StreamFrame is one message of a provider's event stream.
type StreamFrame struct {
	Kind          StreamKind `json:"kind"`
	ServerTime    time.Time  `json:"server_time,omitzero"`
	Rows          *Rows      `json:"rows,omitempty"`
	NewGeneration uint64     `json:"new_generation,omitempty"`
}

// Validate reports whether the fields match the kind and rows are ordered.
func (f StreamFrame) Validate() error {
	if (f.Kind == StreamRows) != (f.Rows != nil) || (f.Kind == StreamCleared) != (f.NewGeneration != 0) {
		return fmt.Errorf("%w: rows and cleared frames, and only those, carry rows or a new generation", ErrInvalid)
	}
	switch f.Kind {
	case StreamConnected, StreamHeartbeat:
		if f.ServerTime.IsZero() {
			return fmt.Errorf("%w: %s frame requires the server time", ErrInvalid, f.Kind)
		}
	case StreamRows:
		if len(f.Rows.Entries) == 0 && len(f.Rows.Tombstones) == 0 {
			return fmt.Errorf("%w: rows frame is empty", ErrInvalid)
		}
		var previous uint64
		for _, entry := range f.Rows.Entries {
			if err := entry.Validate(); err != nil {
				return err
			}
			if entry.UpdatedSequence <= previous {
				return fmt.Errorf("%w: entries are out of order", ErrInvalid)
			}
			previous = entry.UpdatedSequence
		}
		previous = 0
		for _, tombstone := range f.Rows.Tombstones {
			if err := tombstone.Validate(); err != nil {
				return err
			}
			if tombstone.UpdatedSequence <= previous {
				return fmt.Errorf("%w: tombstones are out of order", ErrInvalid)
			}
			previous = tombstone.UpdatedSequence
		}
	case StreamCleared, StreamCursorTooOld:
	default:
		return fmt.Errorf("%w: unknown stream frame kind", ErrInvalid)
	}
	return nil
}

// Event is an unsolicited provider observation: a stream frame together with
// the binding the provider claims for it. The claim is untrusted until
// BindEvent checks it.
type Event struct {
	Tenant     ids.TenantID    `json:"tenant_id"`
	Attempt    ids.AttemptID   `json:"attempt_id"`
	Provider   ids.ProviderID  `json:"provider_id"`
	Operation  ids.OperationID `json:"operation_id"`
	Stream     string          `json:"stream"`
	Generation uint64          `json:"generation"`
	Body       StreamFrame     `json:"body"`
}

// Validate reports whether the claimed binding is complete and the body valid.
func (e Event) Validate() error {
	if e.Tenant.IsZero() || e.Attempt.IsZero() || e.Provider.IsZero() || e.Operation.IsZero() ||
		e.Generation == 0 || !validName(e.Stream) {
		return fmt.Errorf("%w: event requires its full binding and a generation", ErrInvalid)
	}
	return e.Body.Validate()
}

// Peer is the provider identity established by transport authentication, not
// by anything the provider writes in a frame.
type Peer struct {
	Tenant   ids.TenantID
	Provider ids.ProviderID
}

// BoundEvent is an event whose binding has been checked against the
// authenticated peer and the Attempt's recorded provider operation. Its
// fields are unexported so lifecycle code can only receive events that went
// through BindEvent.
type BoundEvent struct {
	event Event
}

// Event returns the bound event.
func (b BoundEvent) Event() Event {
	return b.event
}

// BindEvent authenticates and binds an event before it may affect product
// state: the peer must be the provider the event names, and the event must
// belong to a provider operation the product recorded for that tenant,
// Attempt and provider.
func BindEvent(peer Peer, event Event, operation lifecycle.ProviderOperation) (BoundEvent, *Failure) {
	if err := event.Validate(); err != nil {
		return BoundEvent{}, failure(FailureMalformedFrame, "%v", err)
	}
	if peer.Provider.IsZero() || peer.Tenant.IsZero() || peer.Provider != event.Provider {
		return BoundEvent{}, failure(FailureUnauthenticated, "event is not from the authenticated provider")
	}
	if peer.Tenant != event.Tenant || operation.ID != event.Operation || operation.Tenant != event.Tenant ||
		operation.Attempt != event.Attempt || operation.Provider != event.Provider {
		return BoundEvent{}, failure(FailureUnauthorized, "event is not bound to a recorded operation of this provider")
	}
	return BoundEvent{event: event}, nil
}

// Ingestion is what one bound event contributes: the advanced cursor, the
// accepted entries and tombstones, how many items were already ingested, and
// whether the consumer must rehydrate from a fresh snapshot.
type Ingestion struct {
	Cursor     Cursor
	Entries    []Entry
	Tombstones []Tombstone
	Duplicates int
	Replay     bool
	Stale      bool
	Rehydrate  bool
}

// Ingest applies a bound event to a cursor. Items at or below the cursor are
// duplicates and are counted, never reapplied. An event from an older
// generation is stale and ignored. A cleared generation, a newer generation
// without a clear, or cursor-too-old requires rehydration.
func Ingest(cursor Cursor, bound BoundEvent) (Ingestion, *Failure) {
	if err := cursor.Validate(); err != nil {
		return Ingestion{}, failure(FailureInvalidArguments, "%v", err)
	}
	event := bound.event
	if event.Tenant != cursor.Tenant || event.Attempt != cursor.Attempt || event.Operation != cursor.Operation || event.Stream != cursor.Stream {
		return Ingestion{}, failure(FailureUnauthorized, "event belongs to another stream")
	}
	result := Ingestion{Cursor: cursor}
	switch {
	case event.Generation < cursor.Generation:
		result.Stale = true
		return result, nil
	case event.Body.Kind == StreamCleared:
		if event.Body.NewGeneration <= cursor.Generation {
			return Ingestion{}, failure(FailureMalformedFrame, "cleared generation does not advance")
		}
		result.Cursor.Generation, result.Cursor.After, result.Rehydrate = event.Body.NewGeneration, 0, true
		return result, nil
	case event.Generation > cursor.Generation, event.Body.Kind == StreamCursorTooOld:
		result.Rehydrate = true
		return result, nil
	case event.Body.Kind != StreamRows:
		return result, nil
	}
	rows := event.Body.Rows
	result.Replay = rows.Replay
	for _, entry := range rows.Entries {
		if entry.UpdatedSequence <= cursor.After {
			result.Duplicates++
			continue
		}
		result.Entries = append(result.Entries, entry)
		result.Cursor.After = max(result.Cursor.After, entry.UpdatedSequence)
	}
	for _, tombstone := range rows.Tombstones {
		if tombstone.UpdatedSequence <= cursor.After {
			result.Duplicates++
			continue
		}
		result.Tombstones = append(result.Tombstones, tombstone)
		result.Cursor.After = max(result.Cursor.After, tombstone.UpdatedSequence)
	}
	return result, nil
}
