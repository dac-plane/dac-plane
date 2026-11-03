// Package s3adapter is an artifact store adapter for the S3 API. Bytes go from
// the runtime to a staging bucket through a presigned PUT; commit hashes one
// pinned staging version and freezes exactly that version in a locked objects
// bucket. All recovery state lives in the store (ADR-0017).
package s3adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

const (
	// defaultUploadTTL bounds how long a leaked capability can write staging.
	defaultUploadTTL = 15 * time.Minute
	// maxUploadTTL is the SigV4 presign limit.
	maxUploadTTL = 7 * 24 * time.Hour
	// maxCapabilityBytes is the protocol's limit on an upload capability.
	maxCapabilityBytes = 4096
	// writeAttempts bounds retries of a lost compare-and-swap race.
	writeAttempts = 8

	metadataOperation   = "dac-operation"
	metadataManifest    = "dac-manifest"
	metadataCommittedAt = "dac-committed-at"
)

var (
	identifier = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	digest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Config struct {
	Store      ObjectStore
	ProviderID string
	// UploadTTL is the lifetime of an upload capability.
	UploadTTL time.Duration
	Now       func() time.Time
}

type Adapter struct {
	cfg   Config
	mu    sync.Mutex
	locks map[string]*artifactLock
}

type artifactLock struct {
	mu    sync.Mutex
	users int
}

// manifest mirrors the product's artifact manifest field for field, so a
// receipt can echo it exactly.
type manifest struct {
	ArtifactID     string `json:"artifact_id"`
	TenantID       string `json:"tenant_id"`
	TaskID         string `json:"task_id"`
	AttemptID      string `json:"attempt_id"`
	OperationID    string `json:"operation_id"`
	ProducerID     string `json:"producer_id"`
	Kind           string `json:"kind"`
	Digest         string `json:"digest"`
	Size           uint64 `json:"size"`
	MediaType      string `json:"media_type"`
	SourceRevision string `json:"source_revision,omitempty"`
	WriterEpoch    uint64 `json:"writer_epoch"`
}

type transfer struct {
	Manifest  manifest `json:"manifest"`
	NativeRef string   `json:"native_ref"`
	Upload    string   `json:"upload,omitempty"`
}

type stored struct {
	Present bool   `json:"present"`
	Digest  string `json:"digest,omitempty"`
	Size    uint64 `json:"size"`
}

type committed struct {
	Manifest      manifest  `json:"manifest"`
	Outcome       string    `json:"outcome"`
	Reason        string    `json:"reason,omitempty"`
	ObjectVersion string    `json:"object_version,omitempty"`
	Observed      stored    `json:"observed"`
	CommittedAt   time.Time `json:"committed_at,omitzero"`
}

func New(cfg Config) (*Adapter, error) {
	if cfg.Store == nil || cfg.ProviderID == "" {
		return nil, errors.New("S3 adapter configuration is incomplete")
	}
	if cfg.UploadTTL == 0 {
		cfg.UploadTTL = defaultUploadTTL
	}
	if cfg.UploadTTL < time.Second || cfg.UploadTTL > maxUploadTTL {
		return nil, errors.New("upload TTL must be between one second and seven days")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Adapter{cfg: cfg, locks: map[string]*artifactLock{}}, nil
}

// Send serializes all stages of one artifact, which also serializes the
// deliveries of each operation, as operation.replay requires.
func (a *Adapter) Send(ctx context.Context, r wire.Request) (wire.Reply, error) {
	if r.Role != wire.RoleArtifactStore {
		return rejected(r, wire.FailureInvalidArguments, "request is not for an artifact store"), nil
	}
	var t transfer
	if err := decodeStrict(r.Arguments, &t); err != nil || validate(t) != nil {
		return rejected(r, wire.FailureInvalidArguments, "invalid artifact transfer"), nil
	}
	if t.Manifest.TenantID != r.Tenant || t.Manifest.AttemptID != r.Attempt {
		return rejected(r, wire.FailureInvalidArguments, "artifact manifest is bound to another attempt"), nil
	}
	unlock := a.lock(t.Manifest.TenantID + "/" + t.Manifest.ArtifactID)
	defer unlock()
	switch r.Method {
	case "authorize_upload":
		return a.authorize(ctx, r, t)
	case "stat_object":
		observed, _, err := a.observe(ctx, stagingKey(t.Manifest))
		if err != nil {
			return wire.Reply{}, err
		}
		return accepted(r, observed), nil
	case "commit_manifest":
		if r.Operation != t.Manifest.OperationID {
			return rejected(r, wire.FailureInvalidArguments, "commit operation is not the manifest's"), nil
		}
		return a.commit(ctx, r, t.Manifest)
	}
	return rejected(r, wire.FailureUnknownOperation, "unknown artifact store method"), nil
}

// Cancel has nothing to stop: every stage finishes within its delivery.
func (a *Adapter) Cancel(context.Context, wire.Cancel) error { return nil }

// Subscribe refuses: the store has no native events to stream.
func (a *Adapter) Subscribe(context.Context, wire.Cursor) (adapterkit.Stream, error) {
	return nil, errors.New("the artifact store serves no streams")
}

func (a *Adapter) authorize(ctx context.Context, r wire.Request, t transfer) (wire.Reply, error) {
	m := t.Manifest
	same, err := a.bindAuthorization(ctx, r.Operation, t)
	if err != nil {
		return wire.Reply{}, err
	}
	if !same {
		return rejected(r, wire.FailureInvalidArguments, "authorize operation was replayed with other arguments"), nil
	}
	if err := a.raiseFence(ctx, m); err != nil {
		return wire.Reply{}, err
	}
	// A replay presigns the same key again, which recovers a usable capability.
	upload, err := a.cfg.Store.PresignPut(ctx, stagingKey(m), m.MediaType, int64(m.Size), a.cfg.UploadTTL)
	if err != nil {
		return wire.Reply{}, err
	}
	if upload == "" || len(upload) > maxCapabilityBytes {
		return wire.Reply{}, errors.New("presigned upload does not fit the capability limit")
	}
	return accepted(r, map[string]string{"upload": upload}), nil
}

// bindAuthorization records the first arguments of an authorize operation and
// reports whether this delivery carries the same ones.
func (a *Adapter) bindAuthorization(ctx context.Context, operation string, t transfer) (bool, error) {
	raw, err := json.Marshal(struct {
		Manifest  manifest `json:"manifest"`
		NativeRef string   `json:"native_ref"`
	}{t.Manifest, t.NativeRef})
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	digest := []byte(hex.EncodeToString(sum[:]))
	key := authorizationKey(t.Manifest, operation)
	for range writeAttempts {
		err := a.cfg.Store.PutFence(ctx, key, digest, Write{Create: true})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrPrecondition) {
			return false, err
		}
		current, _, err := a.cfg.Store.GetFence(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		return bytes.Equal(current, digest), nil
	}
	return false, errors.New("artifact authorization stayed contended")
}

// raiseFence moves the artifact's fence up to the writer's epoch. A stale
// writer is still authorized; its commit is fenced, as the simulator does.
func (a *Adapter) raiseFence(ctx context.Context, m manifest) error {
	key := fenceKey(m)
	body := []byte(strconv.FormatUint(m.WriterEpoch, 10))
	for range writeAttempts {
		current, etag, err := a.cfg.Store.GetFence(ctx, key)
		var write Write
		switch {
		case errors.Is(err, ErrNotFound):
			write = Write{Create: true}
		case err != nil:
			return err
		default:
			epoch, err := parseEpoch(current)
			if err != nil {
				return err
			}
			if epoch >= m.WriterEpoch {
				return nil
			}
			write = Write{Match: etag}
		}
		err = a.cfg.Store.PutFence(ctx, key, body, write)
		if !errors.Is(err, ErrPrecondition) {
			return err
		}
	}
	return errors.New("artifact fence stayed contended")
}

func (a *Adapter) fence(ctx context.Context, m manifest) (uint64, error) {
	current, _, err := a.cfg.Store.GetFence(ctx, fenceKey(m))
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return parseEpoch(current)
}

func (a *Adapter) commit(ctx context.Context, r wire.Request, m manifest) (wire.Reply, error) {
	for range writeAttempts {
		winner, err := a.cfg.Store.FirstObject(ctx, objectKey(m))
		switch {
		case err == nil:
			receipt, err := decodeReceipt(winner)
			if err != nil {
				return wire.Reply{}, err
			}
			if receipt.Manifest == m {
				// A crash can separate the freeze from its marker.
				if err := a.markCommitted(ctx, m); err != nil {
					return wire.Reply{}, err
				}
				return accepted(r, receipt), nil
			}
			return a.fenced(r, m), nil
		case !errors.Is(err, ErrNotFound):
			return wire.Reply{}, err
		}
		// A marker without an object means an operator deleted the commit,
		// unless another replica froze it after the read above.
		_, _, err = a.cfg.Store.GetFence(ctx, commitKey(m))
		if err == nil {
			if _, err := a.cfg.Store.FirstObject(ctx, objectKey(m)); errors.Is(err, ErrNotFound) {
				return wire.Reply{}, errors.New("committed object was deleted")
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return wire.Reply{}, err
		}
		epoch, err := a.fence(ctx, m)
		if err != nil {
			return wire.Reply{}, err
		}
		if epoch != m.WriterEpoch {
			return a.fenced(r, m), nil
		}
		observed, version, err := a.observe(ctx, stagingKey(m))
		if err != nil {
			return wire.Reply{}, err
		}
		if reason := retryReason(m, observed); reason != "" {
			// Retry is not final and is not recorded: a later upload may fix it.
			return accepted(r, committed{Manifest: m, Outcome: "retry", Reason: reason, Observed: observed}), nil
		}
		at := a.cfg.Now().UTC().Truncate(time.Microsecond)
		metadata, err := encodeReceipt(r.Operation, m, at)
		if err != nil {
			return wire.Reply{}, err
		}
		// The hashed version is immutable, so freezing it freezes the bytes
		// that were hashed.
		object, err := a.cfg.Store.Freeze(ctx, stagingKey(m), version, objectKey(m), metadata)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		if err != nil {
			return wire.Reply{}, err
		}
		if err := a.markCommitted(ctx, m); err != nil {
			return wire.Reply{}, err
		}
		return accepted(r, committed{Manifest: m, Outcome: "committed", ObjectVersion: object.Version, Observed: observed, CommittedAt: at}), nil
	}
	return wire.Reply{}, errors.New("artifact commit stayed contended")
}

// markCommitted records that the artifact has a commit, so a replay can tell a
// deleted object from one that was never frozen. The marker expires with the
// staged bytes it protects.
func (a *Adapter) markCommitted(ctx context.Context, m manifest) error {
	err := a.cfg.Store.PutFence(ctx, commitKey(m), []byte(m.OperationID), Write{Create: true})
	if errors.Is(err, ErrPrecondition) {
		return nil
	}
	return err
}

// fenced reports no observation: fenced is final, and a stale writer's upload
// capability could otherwise change the reply to a later delivery.
func (a *Adapter) fenced(r wire.Request, m manifest) wire.Reply {
	return accepted(r, committed{Manifest: m, Outcome: "fenced"})
}

// observe hashes the current staging version and returns that version, so a
// commit freezes exactly the bytes it hashed.
func (a *Adapter) observe(ctx context.Context, key string) (stored, string, error) {
	head, err := a.cfg.Store.HeadStaging(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return stored{}, "", nil
	}
	if err != nil {
		return stored{}, "", err
	}
	if head.Version == "" {
		return stored{}, "", errors.New("staging bucket is not versioned")
	}
	body, err := a.cfg.Store.GetStaging(ctx, key, head.Version)
	if errors.Is(err, ErrNotFound) {
		return stored{}, "", nil
	}
	if err != nil {
		return stored{}, "", err
	}
	defer body.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, body)
	if err != nil {
		return stored{}, "", err
	}
	return stored{Present: true, Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Size: uint64(n)}, head.Version, nil
}

// retryReason applies the product's admission order: missing bytes, then
// digest, then size.
func retryReason(m manifest, s stored) string {
	switch {
	case !s.Present:
		return "missing_bytes"
	case s.Digest != m.Digest:
		return "digest_mismatch"
	case s.Size != m.Size:
		return "size_mismatch"
	}
	return ""
}

func encodeReceipt(operation string, m manifest, at time.Time) (map[string]string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		metadataOperation:   operation,
		metadataManifest:    base64.RawURLEncoding.EncodeToString(raw),
		metadataCommittedAt: at.Format(time.RFC3339Nano),
	}, nil
}

// decodeReceipt rebuilds a commit receipt from the winning version alone, so a
// replay after a restart returns what the first delivery returned.
func decodeReceipt(o Object) (committed, error) {
	invalid := errors.New("committed object has no valid receipt")
	raw, err := base64.RawURLEncoding.DecodeString(o.Metadata[metadataManifest])
	if err != nil {
		return committed{}, invalid
	}
	var m manifest
	if decodeStrict(raw, &m) != nil || validateManifest(m) != nil || o.Metadata[metadataOperation] != m.OperationID {
		return committed{}, invalid
	}
	at, err := time.Parse(time.RFC3339Nano, o.Metadata[metadataCommittedAt])
	if err != nil || o.Version == "" || o.Size < 0 || uint64(o.Size) != m.Size {
		return committed{}, invalid
	}
	return committed{Manifest: m, Outcome: "committed", ObjectVersion: o.Version, Observed: stored{Present: true, Digest: m.Digest, Size: m.Size}, CommittedAt: at.UTC()}, nil
}

func (a *Adapter) lock(key string) func() {
	a.mu.Lock()
	l := a.locks[key]
	if l == nil {
		l = &artifactLock{}
		a.locks[key] = l
	}
	l.users++
	a.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		a.mu.Lock()
		if l.users--; l.users == 0 {
			delete(a.locks, key)
		}
		a.mu.Unlock()
	}
}

// Keys are built only from validated identifiers, so no manifest can name a
// key outside its own tenant, attempt and artifact.
func stagingKey(m manifest) string {
	return fmt.Sprintf("staging/%s/%s/%s/%d", m.TenantID, m.AttemptID, m.ArtifactID, m.WriterEpoch)
}
func fenceKey(m manifest) string  { return fmt.Sprintf("fences/%s/%s", m.TenantID, m.ArtifactID) }
func commitKey(m manifest) string { return fmt.Sprintf("commits/%s/%s", m.TenantID, m.ArtifactID) }
func authorizationKey(m manifest, operation string) string {
	return fmt.Sprintf("authorizations/%s/%s", m.TenantID, operation)
}
func objectKey(m manifest) string { return fmt.Sprintf("%s/%s", m.TenantID, m.ArtifactID) }

func validate(t transfer) error {
	if t.NativeRef == "" || len(t.NativeRef) > 512 || len(t.Upload) > maxCapabilityBytes {
		return errors.New("invalid transfer reference")
	}
	return validateManifest(t.Manifest)
}

func validateManifest(m manifest) error {
	for _, id := range []string{m.ArtifactID, m.TenantID, m.TaskID, m.AttemptID, m.OperationID, m.ProducerID} {
		if !identifier.MatchString(id) {
			return errors.New("invalid manifest identifier")
		}
	}
	if !digest.MatchString(m.Digest) || m.WriterEpoch == 0 || m.Kind == "" || m.Size > math.MaxInt64 {
		return errors.New("invalid manifest")
	}
	if _, _, err := mime.ParseMediaType(m.MediaType); err != nil || len(m.MediaType) > 255 {
		return errors.New("invalid manifest media type")
	}
	return nil
}

func parseEpoch(raw []byte) (uint64, error) {
	epoch, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil || epoch == 0 {
		return 0, errors.New("artifact fence is malformed")
	}
	return epoch, nil
}

func accepted(r wire.Request, result any) wire.Reply {
	raw, _ := json.Marshal(result)
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted, Result: raw}
}

func rejected(r wire.Request, code, message string) wire.Reply {
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeFailed, Durability: wire.DurabilityRejected, Failure: &wire.Failure{Code: code, Message: message}}
}

func decodeStrict(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
