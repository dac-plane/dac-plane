package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// ByteArtifactStore is an in-memory, byte-backed qualification fixture. Its
// commit mutex covers hashing and immutable publication, including replay.
// It is not a production artifact storage implementation.
type ByteArtifactStore struct {
	config   Config
	mu       sync.Mutex
	uploads  map[string]lifecycle.ArtifactManifest
	staged   map[ids.ArtifactID][]byte
	commits  map[ids.OperationID]contracts.ArtifactCommitted
	winners  map[ids.ArtifactID]contracts.ArtifactCommitted
	requests map[ids.OperationID]string
	effects  map[ids.OperationID]bool
	fences   map[ids.ArtifactID]uint64
	ledger   *Ledger
}

func NewByteArtifactStore(config Config) (*ByteArtifactStore, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &ByteArtifactStore{config: config, uploads: map[string]lifecycle.ArtifactManifest{}, staged: map[ids.ArtifactID][]byte{}, commits: map[ids.OperationID]contracts.ArtifactCommitted{}, winners: map[ids.ArtifactID]contracts.ArtifactCommitted{}, requests: map[ids.OperationID]string{}, effects: map[ids.OperationID]bool{}, fences: map[ids.ArtifactID]uint64{}, ledger: newLedger()}, nil
}
func (s *ByteArtifactStore) Role() contracts.Role       { return contracts.RoleArtifactStore }
func (s *ByteArtifactStore) Hello() contracts.Lifecycle { return s.config.Hello }
func (s *ByteArtifactStore) Control() *Control          { return s.config.Control }
func (s *ByteArtifactStore) Ledger() *Ledger            { return s.ledger }
func (s *ByteArtifactStore) Cancel(ctx context.Context, c contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return recordCancel(s.ledger, c)
}
func (s *ByteArtifactStore) Subscribe(ctx context.Context, c contracts.Cursor) (contracts.Stream, error) {
	return subscribeInert(s.config, s.ledger, true, c)
}

// Upload accepts bytes only for the manifest named by the issued capability.
// A committed object's bytes cannot be overwritten, including by its writer.
func (s *ByteArtifactStore) Upload(t contracts.ArtifactTransfer, data []byte) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.uploads[t.Upload]
	if !ok || m != t.Manifest {
		return contracts.ErrInvalid
	}
	if _, ok = s.winners[m.Artifact]; ok {
		return fmt.Errorf("%w: object already committed", contracts.ErrInvalid)
	}
	s.staged[m.Artifact] = append([]byte(nil), data...)
	return nil
}

func (s *ByteArtifactStore) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := checkRequest(s.config, s.Role(), r); err != nil {
		return contracts.Reply{}, err
	}
	var t contracts.ArtifactTransfer
	if err := decodeArtifactRequest(r.Arguments, &t); err != nil {
		return contracts.Reply{}, err
	}
	if err := t.Validate(); err != nil {
		return contracts.Reply{}, err
	}
	if t.Manifest.Tenant != r.Tenant || t.Manifest.Attempt != r.Attempt {
		return contracts.Reply{}, contracts.ErrInvalid
	}
	s.ledger.recordRequest(r)
	if err := s.config.Control.reach(ctx, r.Operation, BeforeEffect); err != nil {
		return contracts.Reply{}, err
	}
	s.mu.Lock()
	payload := string(r.Method) + ":" + string(r.Arguments)
	if old, ok := s.requests[r.Operation]; ok && old != payload {
		s.mu.Unlock()
		return contracts.Reply{}, contracts.ErrInvalid
	}
	s.requests[r.Operation] = payload
	var value any
	var effect EffectKind
	switch r.Method {
	case contracts.MethodAuthorizeUpload:
		if t.Manifest.WriterEpoch > s.fences[t.Manifest.Artifact] {
			s.fences[t.Manifest.Artifact] = t.Manifest.WriterEpoch
		}
		token := string(identity.DigestOf([]byte(r.Operation.String() + payload)))
		s.uploads[token] = t.Manifest
		value = contracts.UploadAuthorized{Upload: token}
		effect = UploadAuthorized
	case contracts.MethodStatObject:
		value = s.observe(t.Manifest)
	case contracts.MethodCommitManifest:
		if r.Operation != t.Manifest.Operation {
			s.mu.Unlock()
			return contracts.Reply{}, contracts.ErrInvalid
		}
		if c, ok := s.commits[r.Operation]; ok {
			value = c
			break
		}
		observed := s.observe(t.Manifest)
		c := contracts.ArtifactCommitted{Manifest: t.Manifest, Observed: observed}
		if t.Manifest.WriterEpoch != s.fences[t.Manifest.Artifact] {
			c.Outcome = lifecycle.CommitFenced
		} else if winner, ok := s.winners[t.Manifest.Artifact]; ok {
			if winner.Manifest == t.Manifest {
				c = winner
			} else {
				c.Outcome = lifecycle.CommitFenced
			}
		} else {
			admission, err := lifecycle.AdmitArtifact(t.Manifest, lifecycle.StoredObject{Present: observed.Present, Digest: observed.Digest, Size: observed.Size}, t.Manifest.WriterEpoch, nil, time.Now().UTC().Truncate(time.Microsecond))
			if err != nil {
				s.mu.Unlock()
				return contracts.Reply{}, err
			}
			c.Outcome, c.Reason = admission.Outcome, admission.Reason
			if c.Outcome == lifecycle.CommitCommitted {
				c.ObjectVersion = string(identity.DigestOf([]byte(payload)))
				c.CommittedAt = admission.Artifact.CommittedAt
				s.winners[t.Manifest.Artifact] = c
				effect = ManifestCommitted
			}
		}
		if c.Outcome != lifecycle.CommitRetry {
			s.commits[r.Operation] = c
		}
		value = c
	default:
		s.mu.Unlock()
		return contracts.Reply{}, contracts.ErrInvalid
	}
	if effect != "" && !s.effects[r.Operation] {
		s.effects[r.Operation] = true
		s.ledger.recordEffect(Effect{Role: s.Role(), Operation: r.Operation, Method: r.Method, Kind: effect})
	}
	raw, err := json.Marshal(value)
	s.mu.Unlock()
	if err != nil {
		return contracts.Reply{}, err
	}
	if err = s.config.Control.reach(ctx, r.Operation, AfterEffect); err != nil {
		return contracts.Reply{}, err
	}
	return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: raw}, nil
}

func (s *ByteArtifactStore) observe(m lifecycle.ArtifactManifest) contracts.ArtifactStored {
	data, present := s.staged[m.Artifact]
	if !present {
		return contracts.ArtifactStored{}
	}
	return contracts.ArtifactStored{Present: true, Digest: identity.DigestOf(data), Size: uint64(len(data))}
}

func decodeArtifactRequest(raw []byte, target any) error {
	// The same strict decoder as entry ingestion prevents smuggled metadata.
	var entry contracts.Entry
	entry.Payload = raw
	entry.Kind = "artifact"
	switch p := target.(type) {
	case *contracts.ArtifactTransfer:
		v, err := contracts.DecodeEntry[contracts.ArtifactTransfer](entry)
		*p = v
		return err
	}
	return contracts.ErrInvalid
}

type artifactExport struct {
	store     *ByteArtifactStore
	fixtures  map[string][]byte
	mu        sync.Mutex
	manifests map[ids.OperationID]contracts.ArtifactTransfer
}

// WithArtifactExport configures the runtime's deterministic evidence fixtures.
// Configure before driving the runtime; fixtures are copied, never aliased.
func (r *Runtime) WithArtifactExport(store *ByteArtifactStore, fixtures map[string][]byte) *Runtime {
	e := &artifactExport{store: store, fixtures: map[string][]byte{}, manifests: map[ids.OperationID]contracts.ArtifactTransfer{}}
	for ref, data := range fixtures {
		e.fixtures[ref] = append([]byte(nil), data...)
	}
	r.artifactExport = e
	return r
}
func (r *Runtime) exportArtifact(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	var t contracts.ArtifactTransfer
	if err := decodeArtifactRequest(request.Arguments, &t); err != nil {
		return contracts.Reply{}, err
	}
	if t.Manifest.Tenant != request.Tenant || t.Manifest.Attempt != request.Attempt || t.Manifest.Producer != r.config.Provider {
		return contracts.Reply{}, contracts.ErrInvalid
	}
	r.ledger.recordRequest(request)
	if err := r.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
		return contracts.Reply{}, err
	}
	e := r.artifactExport
	e.mu.Lock()
	old, done := e.manifests[request.Operation]
	if done {
		if old.Manifest != t.Manifest || old.NativeRef != t.NativeRef {
			e.mu.Unlock()
			return contracts.Reply{}, contracts.ErrInvalid
		}
	} else {
		data, ok := e.fixtures[t.NativeRef]
		if !ok {
			e.mu.Unlock()
			return contracts.Reply{}, fmt.Errorf("%w: no artifact fixture", ErrUnscripted)
		}
		if err := e.store.Upload(t, data); err != nil {
			e.mu.Unlock()
			return contracts.Reply{}, err
		}
		t.Upload = ""
		e.manifests[request.Operation] = t
		r.ledger.recordEffect(Effect{Role: contracts.RoleRuntime, Operation: request.Operation, Method: request.Method, Kind: ArtifactsExported})
	}
	e.mu.Unlock()
	if err := r.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
		return contracts.Reply{}, err
	}
	return contracts.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: json.RawMessage(`{}`)}, nil
}
