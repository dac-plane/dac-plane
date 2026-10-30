package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	bbolt "go.etcd.io/bbolt"
)

var ErrPayloadMismatch = errors.New("operation payload changed")

var operations = []byte("operations")
var eventBucket = []byte("events")
var sequences = []byte("observation_sequences")
var cancellations = []byte("pre_start_cancellations")

type Operation struct {
	ID, Hash, Tenant, Attempt, Method, ResourceRef, State string
	// StopOf names the provision a task stop ends. It makes the stop intent
	// durable before the provider is called, so a restart can finish it.
	StopOf    string
	Sequence  uint64
	Receipt   json.RawMessage
	CreatedAt time.Time
}

type Store struct {
	db *bbolt.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(operations); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(eventBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(sequences)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(cancellations)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) CancelBeforeStart(operation string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(cancellations).Put([]byte(operation), []byte{1})
	})
}

func (s *Store) WasCancelledBeforeStart(operation string) (bool, error) {
	var cancelled bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		cancelled = tx.Bucket(cancellations).Get([]byte(operation)) != nil
		return nil
	})
	return cancelled, err
}

func (s *Store) RecordObservation(operation, resource string, payload func(uint64) (json.RawMessage, error)) (json.RawMessage, uint64, error) {
	var result json.RawMessage
	var sequence uint64
	err := s.db.Update(func(tx *bbolt.Tx) error {
		ob := tx.Bucket(operations)
		opRaw := ob.Get([]byte(operation))
		if opRaw == nil {
			return os.ErrNotExist
		}
		var op Operation
		if err := json.Unmarshal(opRaw, &op); err != nil {
			return err
		}
		if op.State == "observed" && len(op.Receipt) != 0 {
			result, sequence = op.Receipt, op.Sequence
			return nil
		}
		seqBucket := tx.Bucket(sequences)
		old := seqBucket.Get([]byte(resource))
		if old != nil {
			sequence = binary.BigEndian.Uint64(old)
		}
		if sequence == ^uint64(0) {
			return errors.New("observation sequence exhausted")
		}
		sequence++
		encodedPayload, err := payload(sequence)
		if err != nil {
			return err
		}
		result = encodedPayload
		root := tx.Bucket(eventBucket)
		b, err := root.CreateBucketIfNotExists([]byte(operation))
		if err != nil {
			return err
		}
		streamSequence := uint64(b.Stats().KeyN + 1)
		entry := wire.Entry{Sequence: streamSequence, UpdatedSequence: streamSequence, Kind: "environment.observed", PayloadHash: hash(result), Payload: result}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, streamSequence)
		if err := b.Put(key, encoded); err != nil {
			return err
		}
		storedSequence := make([]byte, 8)
		binary.BigEndian.PutUint64(storedSequence, sequence)
		if err := seqBucket.Put([]byte(resource), storedSequence); err != nil {
			return err
		}
		op.Sequence, op.State, op.Receipt = sequence, "observed", result
		opRaw, err = json.Marshal(op)
		if err != nil {
			return err
		}
		return ob.Put([]byte(operation), opRaw)
	})
	return result, sequence, err
}

func (s *Store) EventsAfter(operation string, after uint64) ([]wire.Entry, error) {
	var out []wire.Entry
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(eventBucket).Bucket([]byte(operation))
		if b == nil {
			return nil
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, after+1)
		c := b.Cursor()
		for k, v := c.Seek(key); k != nil; k, v = c.Next() {
			var e wire.Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Begin(op Operation) (Operation, bool, error) {
	var got Operation
	created := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(operations)
		if raw := b.Get([]byte(op.ID)); raw != nil {
			if err := json.Unmarshal(raw, &got); err != nil {
				return err
			}
			if got.Hash != op.Hash || got.Tenant != op.Tenant || got.Attempt != op.Attempt || got.Method != op.Method {
				return ErrPayloadMismatch
			}
			return nil
		}
		op.State, op.CreatedAt = "prepared", time.Now().UTC()
		raw, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if err = b.Put([]byte(op.ID), raw); err == nil {
			got, created = op, true
		}
		return err
	})
	return got, created, err
}

func (s *Store) Operation(id string) (Operation, error) {
	var op Operation
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(operations).Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &op)
	})
	return op, err
}

func (s *Store) Update(id string, fn func(*Operation) error) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(operations)
		raw := b.Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		var op Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if err := fn(&op); err != nil {
			return err
		}
		raw, err := json.Marshal(op)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), raw)
	})
}

func (s *Store) Operations() ([]Operation, error) {
	var result []Operation
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(operations).ForEach(func(_, raw []byte) error {
			var op Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
			result = append(result, op)
			return nil
		})
	})
	return result, err
}

// PendingStops returns the task stops that have not recorded their observation.
func (s *Store) PendingStops() ([]Operation, error) {
	all, err := s.Operations()
	if err != nil {
		return nil, err
	}
	var pending []Operation
	for _, op := range all {
		if op.StopOf != "" && op.State != "observed" {
			pending = append(pending, op)
		}
	}
	return pending, nil
}
