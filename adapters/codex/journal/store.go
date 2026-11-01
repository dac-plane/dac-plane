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
	bolt "go.etcd.io/bbolt"
)

var ErrPayloadMismatch = errors.New("operation payload changed")

var (
	operationsBucket = []byte("operations")
	eventsBucket     = []byte("events")
	nativeBucket     = []byte("native_events")
	decisionsBucket  = []byte("decisions")
	requestsBucket   = []byte("native_requests")
	artifactsBucket  = []byte("artifacts")
)

// StateCancelled marks an operation fenced before it started a turn. It may name
// an operation this journal never saw, which a later delivery must not start.
const StateCancelled = "cancelled"

type Operation struct {
	ID, Tenant, Attempt, ArgumentsHash, ThreadID, TurnID, State string
	Baseline                                                    map[string]string
	// Environment is the resource running the commands, empty when they run
	// beside the adapter.
	Environment string
	CreatedAt   time.Time
}

type Decision struct {
	ID, NativeRef, Kind, State string
	RequestID                  json.RawMessage
}
type NativeRequest struct {
	NativeRef, Operation, Method, Session string
	RequestID                             json.RawMessage
}
type Artifact struct {
	NativeRef, Path, Digest, MediaType string
	Size                               uint64
}

type Store struct {
	db      *bolt.DB
	mu      sync.Mutex
	changed chan struct{}
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, changed: make(chan struct{})}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{operationsBucket, eventsBucket, nativeBucket, decisionsBucket, requestsBucket, artifactsBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) Begin(op Operation) (Operation, bool, error) {
	var got Operation
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(operationsBucket)
		raw := b.Get([]byte(op.ID))
		if raw != nil {
			if err := json.Unmarshal(raw, &got); err != nil {
				return err
			}
			if got.State == StateCancelled {
				return nil
			}
			if got.ArgumentsHash != op.ArgumentsHash || got.Tenant != op.Tenant || got.Attempt != op.Attempt {
				return ErrPayloadMismatch
			}
			return nil
		}
		op.State = "prepared"
		op.CreatedAt = time.Now().UTC()
		encoded, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if err = b.Put([]byte(op.ID), encoded); err == nil {
			got = op
			created = true
		}
		return err
	})
	return got, created, err
}
func (s *Store) Operation(id string) (Operation, error) {
	var op Operation
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(operationsBucket).Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &op)
	})
	return op, err
}
func (s *Store) OperationByThread(thread string) (Operation, error) {
	var found Operation
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(operationsBucket).ForEach(func(_, raw []byte) error {
			var op Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
			if op.ThreadID == thread {
				found = op
			}
			return nil
		})
	})
	if err == nil && found.ID == "" {
		err = os.ErrNotExist
	}
	return found, err
}
func (s *Store) OperationByAttempt(attempt string) (Operation, error) {
	var found Operation
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(operationsBucket).ForEach(func(_, raw []byte) error {
			var op Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
			if op.Attempt == attempt && op.ThreadID != "" {
				found = op
			}
			return nil
		})
	})
	if err == nil && found.ID == "" {
		err = os.ErrNotExist
	}
	return found, err
}
func (s *Store) Operations() ([]Operation, error) {
	var operations []Operation
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(operationsBucket).ForEach(func(_, raw []byte) error {
			var op Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
			operations = append(operations, op)
			return nil
		})
	})
	return operations, err
}
func (s *Store) UpdateOperation(id string, update func(*Operation) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(operationsBucket)
		raw := b.Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		var op Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if err := update(&op); err != nil {
			return err
		}
		next, err := json.Marshal(op)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), next)
	})
}

// Fence records that the operation must never start a turn.
func (s *Store) Fence(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(operationsBucket)
		op := Operation{ID: id, CreatedAt: time.Now().UTC()}
		if raw := b.Get([]byte(id)); raw != nil {
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
		}
		op.State = StateCancelled
		encoded, err := json.Marshal(op)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), encoded)
	})
}

func (s *Store) AppendEvent(operation, native, kind string, payload any) (uint64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var seq uint64
	created := false
	err = s.db.Update(func(tx *bolt.Tx) error {
		nativeKey := []byte(operation + "\x00" + native)
		n := tx.Bucket(nativeBucket)
		if existing := n.Get(nativeKey); existing != nil {
			seq = binary.BigEndian.Uint64(existing)
			return nil
		}
		root := tx.Bucket(eventsBucket)
		b, err := root.CreateBucketIfNotExists([]byte(operation))
		if err != nil {
			return err
		}
		seq = uint64(b.Stats().KeyN + 1)
		entry := wire.Entry{Sequence: seq, UpdatedSequence: seq, Kind: kind, PayloadHash: Hash(raw), Payload: raw}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		if err = b.Put(key, encoded); err != nil {
			return err
		}
		if err = n.Put(nativeKey, key); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err == nil && created {
		s.signal()
	}
	return seq, err
}
func (s *Store) EventsAfter(operation string, after uint64) ([]wire.Entry, error) {
	var out []wire.Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(eventsBucket).Bucket([]byte(operation))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, after+1)
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
func (s *Store) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}
func (s *Store) Changed() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }

func (s *Store) PutDecision(d Decision) error {
	encoded, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(decisionsBucket)
		if raw := b.Get([]byte(d.ID)); raw != nil {
			var old Decision
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if old.NativeRef != d.NativeRef || old.Kind != d.Kind {
				return ErrPayloadMismatch
			}
			return nil
		}
		return b.Put([]byte(d.ID), encoded)
	})
}
func (s *Store) Decision(id string) (Decision, error) {
	var d Decision
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(decisionsBucket).Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &d)
	})
	return d, err
}
func (s *Store) UpdateDecision(id, state string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(decisionsBucket)
		raw := b.Get([]byte(id))
		if raw == nil {
			return os.ErrNotExist
		}
		var d Decision
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		d.State = state
		next, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), next)
	})
}
func (s *Store) PutNativeRequest(r NativeRequest) error {
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(requestsBucket)
		if raw := b.Get([]byte(r.NativeRef)); raw != nil {
			var old NativeRequest
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if old.Operation != r.Operation || old.Method != r.Method {
				return ErrPayloadMismatch
			}
		}
		return b.Put([]byte(r.NativeRef), encoded)
	})
}
func (s *Store) NativeRequest(ref string) (NativeRequest, error) {
	var r NativeRequest
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(requestsBucket).Get([]byte(ref))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &r)
	})
	return r, err
}

func (s *Store) PutArtifact(operation string, a Artifact) error {
	if a.NativeRef == "" || a.Path == "" || a.Digest == "" {
		return errors.New("artifact is incomplete")
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(artifactsBucket)
		b, err := root.CreateBucketIfNotExists([]byte(operation))
		if err != nil {
			return err
		}
		return b.Put([]byte(a.NativeRef), encoded)
	})
}
func (s *Store) Artifact(operation, native string) (Artifact, error) {
	var a Artifact
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(artifactsBucket).Bucket([]byte(operation))
		if b == nil {
			return os.ErrNotExist
		}
		raw := b.Get([]byte(native))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &a)
	})
	return a, err
}

func Key(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
