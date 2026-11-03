package s3adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/aws/smithy-go"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	tenant   = "019a0d5b-f74c-7000-8000-000000000101"
	task     = "019a0d5b-f74c-7000-8000-000000000102"
	attempt  = "019a0d5b-f74c-7000-8000-000000000103"
	artifact = "019a0d5b-f74c-7000-8000-000000000104"
	producer = "019a0d5b-f74c-7000-8000-000000000105"
	commitOp = "019a0d5b-f74c-7000-8000-000000000106"
)

var fixedNow = time.Date(2026, 11, 3, 12, 0, 0, 123456789, time.UTC)

func newAdapter(t *testing.T, store ObjectStore) *Adapter {
	t.Helper()
	a, err := New(Config{Store: store, ProviderID: "store-test", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func manifestFor(data []byte, epoch uint64) manifest {
	sum := sha256.Sum256(data)
	return manifest{ArtifactID: artifact, TenantID: tenant, TaskID: task, AttemptID: attempt, OperationID: commitOp, ProducerID: producer,
		Kind: "code_change", Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: uint64(len(data)), MediaType: "text/x-diff", SourceRevision: "codex-turn:t1", WriterEpoch: epoch}
}

func request(t *testing.T, method, operation string, m manifest) wire.Request {
	t.Helper()
	raw, err := json.Marshal(transfer{Manifest: m, NativeRef: "patch.diff"})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Request{Operation: operation, Delivery: "delivery-" + method, Tenant: tenant, Attempt: attempt, Role: wire.RoleArtifactStore, Method: method, Arguments: raw, Demand: "background"}
}

func send(t *testing.T, a *Adapter, method, operation string, m manifest) wire.Reply {
	t.Helper()
	reply, err := a.Send(context.Background(), request(t, method, operation, m))
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func commitReply(t *testing.T, a *Adapter, m manifest) committed {
	t.Helper()
	reply := send(t, a, "commit_manifest", m.OperationID, m)
	if reply.Outcome != wire.OutcomeOK {
		t.Fatalf("commit failed: %+v", reply.Failure)
	}
	conform(t, "artifact-committed", reply.Result)
	var c committed
	if err := decodeStrict(reply.Result, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func authorize(t *testing.T, a *Adapter, m manifest, operation string) string {
	t.Helper()
	reply := send(t, a, "authorize_upload", operation, m)
	if reply.Outcome != wire.OutcomeOK {
		t.Fatalf("authorize failed: %+v", reply.Failure)
	}
	conform(t, "upload-authorized", reply.Result)
	var u struct {
		Upload string `json:"upload"`
	}
	if err := decodeStrict(reply.Result, &u); err != nil {
		t.Fatal(err)
	}
	return u.Upload
}

var schemas sync.Map

// conform checks a reply payload against the product's JSON Schema, so the
// adapter cannot drift from the contract the control plane decodes.
func conform(t *testing.T, name string, raw []byte) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "contracts", "jsonschema", "adapter", name+".schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := schemas.Load(path)
	if !ok {
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.AssertFormat()
		schema, err := compiler.Compile(path)
		if err != nil {
			t.Fatal(err)
		}
		cached, _ = schemas.LoadOrStore(path, schema)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := cached.(*jsonschema.Schema).Validate(instance); err != nil {
		t.Fatalf("%s does not conform: %v\n%s", name, err, raw)
	}
}

func TestCommitFreezesOnlyMatchingBytesAndReplaysExactly(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("diff --git a/x b/x\n")
	m := manifestFor(data, 1)
	upload := authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	if !strings.Contains(upload, "staging/"+tenant+"/"+attempt+"/"+artifact+"/1") || !strings.Contains(upload, "length=19") {
		t.Fatalf("capability is not scoped to this upload: %s", upload)
	}
	if got := commitReply(t, a, m); got.Outcome != "retry" || got.Reason != "missing_bytes" || got.Observed.Present {
		t.Fatalf("absent bytes: %+v", got)
	}
	store.upload(stagingKey(m), []byte("diff --git a/x b/y\n"))
	if got := commitReply(t, a, m); got.Outcome != "retry" || got.Reason != "digest_mismatch" || !got.Observed.Present {
		t.Fatalf("wrong bytes: %+v", got)
	}
	store.upload(stagingKey(m), data)
	first := commitReply(t, a, m)
	if first.Outcome != "committed" || first.ObjectVersion == "" || !first.CommittedAt.Equal(fixedNow.Truncate(time.Microsecond)) || first.Observed.Digest != m.Digest {
		t.Fatalf("matching bytes: %+v", first)
	}
	// A capability can be used again until it expires; a later upload must not
	// change the committed object or its receipt.
	store.upload(stagingKey(m), []byte("later bytes"))
	if replay := commitReply(t, a, m); replay != first {
		t.Fatalf("replay changed the receipt:\n%+v\n%+v", first, replay)
	}
	if !bytes.Equal(store.frozen(objectKey(m)), data) {
		t.Fatal("frozen bytes are not the hashed version")
	}
	// A restarted adapter has no memory; the store holds the receipt.
	if restarted := commitReply(t, newAdapter(t, store), m); restarted != first {
		t.Fatalf("restart changed the receipt:\n%+v\n%+v", first, restarted)
	}
}

func TestReceiptSurvivesOverwriteOfCurrentVersion(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	m := manifestFor(data, 1)
	authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	store.upload(stagingKey(m), data)
	first := commitReply(t, a, m)
	store.overwrite(objectKey(m), []byte("overwritten"))
	if replay := commitReply(t, a, m); replay != first {
		t.Fatalf("overwrite changed the winner:\n%+v\n%+v", first, replay)
	}
}

func TestLostFreezeReplyReplaysTheRecordedCommit(t *testing.T) {
	store := newFakeStore()
	store.lostFreezeReply = true
	a := newAdapter(t, store)
	data := []byte("artifact")
	m := manifestFor(data, 1)
	authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	store.upload(stagingKey(m), data)
	if _, err := a.Send(context.Background(), request(t, "commit_manifest", m.OperationID, m)); err == nil {
		t.Fatal("a lost reply must leave the outcome unknown")
	}
	got := commitReply(t, a, m)
	if got.Outcome != "committed" || got.ObjectVersion != store.objects[objectKey(m)][0].id {
		t.Fatalf("replay did not recover the commit: %+v", got)
	}
}

func TestStaleWriterIsFencedWithoutChangingTheWinner(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	stale, current := manifestFor(data, 1), manifestFor(data, 2)
	authorize(t, a, stale, "019a0d5b-f74c-7000-8000-000000000111")
	authorize(t, a, current, "019a0d5b-f74c-7000-8000-000000000112")
	// Authorizing the stale writer again does not lower the fence.
	authorize(t, a, stale, "019a0d5b-f74c-7000-8000-000000000111")
	store.upload(stagingKey(stale), data)
	store.upload(stagingKey(current), data)
	if got := commitReply(t, a, stale); got.Outcome != "fenced" || got.ObjectVersion != "" {
		t.Fatalf("stale writer: %+v", got)
	}
	won := commitReply(t, a, current)
	if won.Outcome != "committed" {
		t.Fatalf("current writer: %+v", won)
	}
	if got := commitReply(t, a, stale); got.Outcome != "fenced" {
		t.Fatalf("stale writer after the winner: %+v", got)
	}
	if replay := commitReply(t, a, current); replay != won {
		t.Fatalf("winner changed: %+v", replay)
	}
}

func TestUnauthorizedEpochIsFenced(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	authorize(t, a, manifestFor(data, 1), "019a0d5b-f74c-7000-8000-000000000111")
	future := manifestFor(data, 3)
	store.upload(stagingKey(future), data)
	if got := commitReply(t, a, future); got.Outcome != "fenced" {
		t.Fatalf("an epoch nobody authorized committed: %+v", got)
	}
}

func TestConcurrentCommitsHaveOneWinner(t *testing.T) {
	store := newFakeStore()
	data := []byte("artifact")
	m := manifestFor(data, 1)
	a := newAdapter(t, store)
	authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	store.upload(stagingKey(m), data)
	// Two adapter processes share nothing but the store.
	replicas := []*Adapter{a, newAdapter(t, store), newAdapter(t, store), newAdapter(t, store)}
	results := make([]committed, len(replicas))
	var wg sync.WaitGroup
	for i, r := range replicas {
		wg.Go(func() { results[i] = commitReply(t, r, m) })
	}
	wg.Wait()
	for _, got := range results {
		if got != results[0] || got.Outcome != "committed" {
			t.Fatalf("replicas disagree: %+v", results)
		}
	}
	if len(store.objects[objectKey(m)]) != 1 {
		t.Fatal("more than one object was frozen")
	}
}

func TestRaceLostToAnotherManifestIsFenced(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	m := manifestFor(data, 1)
	authorize(t, a, m, "019a0d5b-f74c-7000-8000-000000000110")
	store.upload(stagingKey(m), data)
	other := m
	other.OperationID = "019a0d5b-f74c-7000-8000-000000000120"
	metadata, err := encodeReceipt(other.OperationID, other, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	store.freezeHook = func(key string) error {
		store.freezeHook = nil
		store.mu.Lock()
		store.objects[key] = []fakeVersion{store.version(data, metadata)}
		store.mu.Unlock()
		return nil
	}
	if got := commitReply(t, a, m); got.Outcome != "fenced" {
		t.Fatalf("lost race: %+v", got)
	}
}

func TestStatObservesWithoutCommitting(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	m := manifestFor(data, 1)
	reply := send(t, a, "stat_object", "019a0d5b-f74c-7000-8000-000000000113", m)
	conform(t, "artifact-stored", reply.Result)
	if string(reply.Result) != `{"present":false,"size":0}` {
		t.Fatalf("absent object: %s", reply.Result)
	}
	store.upload(stagingKey(m), data)
	reply = send(t, a, "stat_object", "019a0d5b-f74c-7000-8000-000000000113", m)
	conform(t, "artifact-stored", reply.Result)
	var s stored
	if err := decodeStrict(reply.Result, &s); err != nil || !s.Present || s.Digest != m.Digest || s.Size != m.Size {
		t.Fatalf("present object: %s", reply.Result)
	}
	if len(store.objects) != 0 {
		t.Fatal("stat froze an object")
	}
}

func TestRefusesRequestsOutsideTheirScope(t *testing.T) {
	a := newAdapter(t, newFakeStore())
	m := manifestFor([]byte("artifact"), 1)
	cases := map[string]func(*wire.Request){
		"other tenant":   func(r *wire.Request) { r.Tenant = "019a0d5b-f74c-7000-8000-000000000199" },
		"other attempt":  func(r *wire.Request) { r.Attempt = "019a0d5b-f74c-7000-8000-000000000199" },
		"other role":     func(r *wire.Request) { r.Role = wire.RoleRuntime },
		"unknown method": func(r *wire.Request) { r.Method = "delete_object" },
		"unknown field": func(r *wire.Request) {
			r.Arguments = []byte(strings.Replace(string(r.Arguments), `"native_ref"`, `"bucket":"x","native_ref"`, 1))
		},
		"path in an id": func(r *wire.Request) {
			r.Arguments = []byte(strings.Replace(string(r.Arguments), artifact, "../../other", 1))
		},
		"commit other op": func(r *wire.Request) {
			r.Method, r.Operation = "commit_manifest", "019a0d5b-f74c-7000-8000-000000000199"
		},
		"header media type": func(r *wire.Request) {
			r.Arguments = []byte(strings.Replace(string(r.Arguments), "text/x-diff", `text/plain\r\nx-amz-acl: public-read`, 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := request(t, "authorize_upload", "019a0d5b-f74c-7000-8000-000000000110", m)
			mutate(&r)
			reply, err := a.Send(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Outcome != wire.OutcomeFailed || reply.Durability != wire.DurabilityRejected {
				t.Fatalf("accepted: %+v", reply)
			}
		})
	}
}

func TestCorruptReceiptIsNotTrusted(t *testing.T) {
	store := newFakeStore()
	a := newAdapter(t, store)
	data := []byte("artifact")
	m := manifestFor(data, 1)
	store.objects[objectKey(m)] = []fakeVersion{{id: "v9999", body: data, metadata: map[string]string{metadataOperation: m.OperationID}}}
	if _, err := a.Send(context.Background(), request(t, "commit_manifest", m.OperationID, m)); err == nil {
		t.Fatal("an object without a valid receipt must not answer the commit")
	}
}

func TestFenceContentionIsBounded(t *testing.T) {
	store := &contendedFence{fakeStore: newFakeStore()}
	a := newAdapter(t, store)
	_, err := a.Send(context.Background(), request(t, "authorize_upload", "019a0d5b-f74c-7000-8000-000000000110", manifestFor([]byte("x"), 1)))
	if err == nil || !strings.Contains(err.Error(), "contended") {
		t.Fatalf("contention: %v", err)
	}
}

type contendedFence struct{ *fakeStore }

func (c *contendedFence) PutFence(context.Context, string, []byte, Write) error {
	return ErrPrecondition
}

func TestConfigurationIsChecked(t *testing.T) {
	if _, err := New(Config{ProviderID: "p"}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := New(Config{Store: newFakeStore(), ProviderID: "p", UploadTTL: 8 * 24 * time.Hour}); err == nil {
		t.Fatal("presign lifetime beyond SigV4 accepted")
	}
	if _, err := NewS3Store(S3Config{Endpoint: "http://store:8333", Region: "r", AccessKey: "a", SecretKey: "s", StagingBucket: "a", ObjectsBucket: "b"}); err == nil {
		t.Fatal("plain HTTP endpoint accepted")
	}
	if _, err := NewS3Store(S3Config{Endpoint: "https://store:8443", Region: "r", AccessKey: "a", SecretKey: "s", StagingBucket: "a", ObjectsBucket: "a"}); err == nil {
		t.Fatal("one bucket for staging and objects accepted")
	}
}

func TestClassifyMapsStoreRefusals(t *testing.T) {
	for code, want := range map[string]error{"NoSuchKey": ErrNotFound, "NoSuchVersion": ErrNotFound, "NotFound": ErrNotFound, "PreconditionFailed": ErrPrecondition, "ConditionalRequestConflict": ErrPrecondition} {
		if got := classify(&smithy.GenericAPIError{Code: code}); !errors.Is(got, want) {
			t.Fatalf("%s mapped to %v", code, got)
		}
	}
	denied := &smithy.GenericAPIError{Code: "AccessDenied"}
	if got := classify(denied); !errors.Is(got, denied) {
		t.Fatalf("AccessDenied mapped to %v", got)
	}
}
