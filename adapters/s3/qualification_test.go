//go:build s3qualification

package s3adapter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// qualification is the live store the runner started. Every check goes
// through the adapter's own code against that store; raw SDK calls are used
// only to act as the operator or as a misbehaving key holder.
type qualification struct {
	cfg      S3Config
	operator S3Config
	httpc    *http.Client
	raw      *s3.Client
	rawOp    *s3.Client
	store    *S3Store
	opStore  *S3Store
}

func liveStore(t *testing.T) *qualification {
	t.Helper()
	get := func(name string) string {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s is required; run just qualify-artifact-store", name)
		}
		return value
	}
	secret := func(name string) string {
		raw, err := os.ReadFile(get(name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(raw))
	}
	pem, err := os.ReadFile(get("DAC_S3_QUALIFY_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("CA file has no certificates")
	}
	httpc := &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	base := S3Config{Endpoint: get("DAC_S3_QUALIFY_ENDPOINT"), Region: "us-east-1", StagingBucket: get("DAC_S3_QUALIFY_STAGING_BUCKET"), ObjectsBucket: get("DAC_S3_QUALIFY_OBJECTS_BUCKET"), HTTPClient: httpc}
	adapter, operator := base, base
	adapter.AccessKey, adapter.SecretKey = get("DAC_S3_QUALIFY_ADAPTER_KEY_ID"), secret("DAC_S3_QUALIFY_ADAPTER_SECRET_FILE")
	operator.AccessKey, operator.SecretKey = get("DAC_S3_QUALIFY_OPERATOR_KEY_ID"), secret("DAC_S3_QUALIFY_OPERATOR_SECRET_FILE")
	q := &qualification{cfg: adapter, operator: operator, httpc: httpc}
	if q.store, err = NewS3Store(adapter); err != nil {
		t.Fatal(err)
	}
	if q.opStore, err = NewS3Store(operator); err != nil {
		t.Fatal(err)
	}
	q.raw, q.rawOp = q.store.client, q.opStore.client
	return q
}

func timed(t *testing.T, name string, check func(t *testing.T)) {
	t.Helper()
	start := time.Now()
	t.Run(name, func(t *testing.T) {
		check(t)
		t.Logf("observed in %s", time.Since(start).Round(time.Millisecond))
	})
}

// put performs the runtime's upload exactly as the Codex adapter does: a plain
// PUT with Content-Type and Content-Length and nothing else.
func (q *qualification) put(t *testing.T, url, contentType string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	resp, err := q.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// liveManifest gives each check its own artifact, so checks never share keys.
func liveManifest(data []byte, epoch uint64, n int) manifest {
	m := manifestFor(data, epoch)
	m.ArtifactID = fmt.Sprintf("019a0d5b-f74c-7000-8000-%012d", 900000+n)
	m.OperationID = fmt.Sprintf("019a0d5b-f74c-7000-8001-%012d", 900000+n*10+int(epoch))
	return m
}

func authorizeOp(m manifest) string { return strings.Replace(m.OperationID, "-8001-", "-8002-", 1) }

func (q *qualification) adapter(t *testing.T, store ObjectStore, ttl time.Duration) *Adapter {
	t.Helper()
	a, err := New(Config{Store: store, ProviderID: "s3-qualification", UploadTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (q *qualification) frozenBytes(t *testing.T, m manifest, version string) []byte {
	t.Helper()
	out, err := q.rawOp.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), VersionId: aws.String(version)})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// lostReply applies the real freeze and then reports a transport failure.
type lostReply struct {
	*S3Store
	once sync.Once
}

func (l *lostReply) Freeze(ctx context.Context, stagingKey, version, objectKey string, metadata map[string]string) (Object, error) {
	o, err := l.S3Store.Freeze(ctx, stagingKey, version, objectKey, metadata)
	lost := false
	l.once.Do(func() { lost = true })
	if err == nil && lost {
		return Object{}, errors.New("reply lost after freeze")
	}
	return o, err
}

func TestQualifyArtifactStore(t *testing.T) {
	q := liveStore(t)
	ctx := context.Background()
	retention := Retention{ObjectDays: 1, StagingDays: 2}

	timed(t, "provision is safe to repeat", func(t *testing.T) {
		for range 2 {
			if err := q.opStore.Provision(ctx, retention); err != nil {
				t.Fatal(err)
			}
		}
	})
	timed(t, "adapter key passes preflight", func(t *testing.T) {
		if err := q.store.Preflight(ctx); err != nil {
			t.Fatal(err)
		}
	})

	a := q.adapter(t, q.store, 0)
	data := []byte("diff --git a/qualification b/qualification\n")

	var first committed
	m := liveManifest(data, 1, 1)
	timed(t, "commit freezes the uploaded bytes", func(t *testing.T) {
		upload := authorize(t, a, m, authorizeOp(m))
		if got := commitReply(t, a, m); got.Outcome != "retry" || got.Reason != "missing_bytes" {
			t.Fatalf("before upload: %+v", got)
		}
		if status := q.put(t, upload, m.MediaType, bytes.ToUpper(data)); status != http.StatusOK {
			t.Fatalf("upload returned %d", status)
		}
		if got := commitReply(t, a, m); got.Outcome != "retry" || got.Reason != "digest_mismatch" {
			t.Fatalf("wrong bytes: %+v", got)
		}
		if status := q.put(t, upload, m.MediaType, data); status != http.StatusOK {
			t.Fatalf("upload returned %d", status)
		}
		stat := send(t, a, "stat_object", authorizeOp(m), m)
		conform(t, "artifact-stored", stat.Result)
		first = commitReply(t, a, m)
		if first.Outcome != "committed" || !bytes.Equal(q.frozenBytes(t, m, first.ObjectVersion), data) {
			t.Fatalf("commit: %+v", first)
		}
	})
	timed(t, "replay and restart return the recorded receipt", func(t *testing.T) {
		if got := commitReply(t, a, m); got != first {
			t.Fatalf("replay: %+v", got)
		}
		if got := commitReply(t, q.adapter(t, q.store, 0), m); got != first {
			t.Fatalf("restart: %+v", got)
		}
		upload := authorize(t, a, m, authorizeOp(m))
		if status := q.put(t, upload, m.MediaType, bytes.Repeat([]byte("x"), len(data))); status != http.StatusOK {
			t.Fatalf("late upload returned %d", status)
		}
		if _, err := q.raw.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), Body: strings.NewReader("overwrite")}); err != nil {
			t.Logf("adapter key overwrite refused: %v", err)
		}
		if got := commitReply(t, a, m); got != first {
			t.Fatalf("after a late upload and an overwrite: %+v", got)
		}
		if !bytes.Equal(q.frozenBytes(t, m, first.ObjectVersion), data) {
			t.Fatal("committed bytes changed")
		}
	})

	timed(t, "capabilities are bound to one upload", func(t *testing.T) {
		scoped := liveManifest(data, 1, 2)
		upload := authorize(t, a, scoped, authorizeOp(scoped))
		other := liveManifest(data, 1, 3)
		for name, url := range map[string]string{
			"other artifact": strings.Replace(upload, scoped.ArtifactID, other.ArtifactID, 1),
			"other attempt":  strings.Replace(upload, scoped.AttemptID, "019a0d5b-f74c-7000-8000-000000000199", 1),
			"other tenant":   strings.Replace(upload, scoped.TenantID, "019a0d5b-f74c-7000-8000-000000000198", 1),
		} {
			if status := q.put(t, url, scoped.MediaType, data); status != http.StatusForbidden {
				t.Fatalf("%s: %d", name, status)
			}
		}
		if status := q.put(t, upload, "application/octet-stream", data); status != http.StatusForbidden {
			t.Fatalf("other content type: %d", status)
		}
		if status := q.put(t, upload, scoped.MediaType, append(data, '!')); status != http.StatusForbidden {
			t.Fatalf("other length: %d", status)
		}
		short := q.adapter(t, q.store, 2*time.Second)
		expiring := liveManifest(data, 1, 4)
		url := authorize(t, short, expiring, authorizeOp(expiring))
		time.Sleep(3 * time.Second)
		if status := q.put(t, url, expiring.MediaType, data); status != http.StatusForbidden {
			t.Fatalf("expired capability: %d", status)
		}
	})

	timed(t, "stale writers are fenced", func(t *testing.T) {
		stale, current := liveManifest(data, 1, 5), liveManifest(data, 2, 5)
		staleURL := authorize(t, a, stale, authorizeOp(stale))
		currentURL := authorize(t, a, current, authorizeOp(current))
		q.put(t, staleURL, stale.MediaType, data)
		q.put(t, currentURL, current.MediaType, data)
		if got := commitReply(t, a, stale); got.Outcome != "fenced" {
			t.Fatalf("stale: %+v", got)
		}
		won := commitReply(t, a, current)
		if won.Outcome != "committed" {
			t.Fatalf("current: %+v", won)
		}
		if got := commitReply(t, a, stale); got.Outcome != "fenced" {
			t.Fatalf("stale after the winner: %+v", got)
		}
	})

	timed(t, "concurrent replicas agree on one object", func(t *testing.T) {
		c := liveManifest(data, 1, 6)
		q.put(t, authorize(t, a, c, authorizeOp(c)), c.MediaType, data)
		results := make([]committed, 4)
		var wg sync.WaitGroup
		for i := range results {
			replica := q.adapter(t, q.store, 0)
			wg.Go(func() { results[i] = commitReply(t, replica, c) })
		}
		wg.Wait()
		for _, got := range results {
			if got != results[0] || got.Outcome != "committed" {
				t.Fatalf("replicas disagree: %+v", results)
			}
		}
		versions, err := q.rawOp.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(q.cfg.ObjectsBucket), Prefix: aws.String(objectKey(c))})
		if err != nil || len(versions.Versions) != 1 {
			t.Fatalf("frozen versions: %v %d", err, len(versions.Versions))
		}
	})

	timed(t, "a lost commit reply replays to the same receipt", func(t *testing.T) {
		l := liveManifest(data, 1, 7)
		q.put(t, authorize(t, a, l, authorizeOp(l)), l.MediaType, data)
		lossy := q.adapter(t, &lostReply{S3Store: q.store}, 0)
		if _, err := lossy.Send(ctx, request(t, "commit_manifest", l.OperationID, l)); err == nil {
			t.Fatal("lost reply reported an outcome")
		}
		got := commitReply(t, q.adapter(t, q.store, 0), l)
		if got.Outcome != "committed" || got.ObjectVersion == "" {
			t.Fatalf("recovery: %+v", got)
		}
	})

	timed(t, "the adapter key cannot delete committed objects", func(t *testing.T) {
		if _, err := q.raw.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m))}); err == nil {
			t.Fatal("adapter key placed a delete marker")
		}
		if _, err := q.raw.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), VersionId: aws.String(first.ObjectVersion), BypassGovernanceRetention: aws.Bool(true)}); err == nil {
			t.Fatal("adapter key deleted a locked version")
		}
		if got := commitReply(t, a, m); got != first {
			t.Fatalf("receipt after refused deletes: %+v", got)
		}
	})

	if container := strings.TrimSpace(os.Getenv("DAC_S3_QUALIFY_CONTAINER")); container != "" {
		timed(t, "a store restart keeps every receipt", func(t *testing.T) {
			if out, err := exec.Command("docker", "restart", container).CombinedOutput(); err != nil {
				t.Fatalf("restart: %v %s", err, out)
			}
			deadline := time.Now().Add(time.Minute)
			for q.store.Preflight(ctx) != nil {
				if time.Now().After(deadline) {
					t.Fatal("store did not return")
				}
				time.Sleep(time.Second)
			}
			if got := commitReply(t, q.adapter(t, q.store, 0), m); got != first {
				t.Fatalf("after store restart: %+v", got)
			}
		})
	}

	timed(t, "operator deletion is a two-step procedure", func(t *testing.T) {
		if _, err := q.rawOp.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), VersionId: aws.String(first.ObjectVersion), BypassGovernanceRetention: aws.Bool(true)}); err == nil {
			t.Fatal("the policy did not hold against the operator")
		}
		if _, err := q.rawOp.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(q.cfg.ObjectsBucket)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.rawOp.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), VersionId: aws.String(first.ObjectVersion), BypassGovernanceRetention: aws.Bool(true)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.rawOp.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(m)), VersionId: aws.String(first.ObjectVersion)}); err == nil {
			t.Fatal("deleted version is still readable")
		}
		// Record, not assert: what a replay of the deleted commit answers.
		reply, err := a.Send(ctx, request(t, "commit_manifest", m.OperationID, m))
		t.Logf("replay after operator deletion: outcome=%s err=%v result=%s", reply.Outcome, err, reply.Result)
		// A fresh artifact has no overwrite to fall back on: its one version
		// goes, while its staged bytes and fence remain.
		sole := liveManifest(data, 1, 8)
		if status := q.put(t, authorize(t, a, sole, authorizeOp(sole)), sole.MediaType, data); status != http.StatusOK {
			t.Fatalf("upload returned %d", status)
		}
		committedSole := commitReply(t, a, sole)
		if committedSole.Outcome != "committed" {
			t.Fatalf("sole commit: %+v", committedSole)
		}
		if _, err := q.rawOp.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(q.cfg.ObjectsBucket), Key: aws.String(objectKey(sole)), VersionId: aws.String(committedSole.ObjectVersion), BypassGovernanceRetention: aws.Bool(true)}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Send(ctx, request(t, "commit_manifest", sole.OperationID, sole)); err == nil || !strings.Contains(err.Error(), "deleted") {
			t.Fatalf("replay of the deleted commit: %v", err)
		}
		if _, err := q.store.FirstObject(ctx, objectKey(sole)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the replay recreated the deleted commit: %v", err)
		}
		if err := q.opStore.Provision(ctx, retention); err != nil {
			t.Fatal(err)
		}
	})

}
