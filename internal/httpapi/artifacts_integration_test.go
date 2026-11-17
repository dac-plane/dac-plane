//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/authn"
	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/simulator"
	"github.com/dac-plane/dac-plane/internal/testsupport/wiretest"
	"github.com/dac-plane/dac-plane/internal/worker"
)

// downloadStore stands in for a connected artifact store adapter: the
// simulator's byte store answers authorize_download, and a TLS server plays
// the store's presigned reads. Tests change what it serves to show that the
// dac-plane checks the bytes, not the store's word.
type downloadStore struct {
	mu         sync.Mutex
	store      *simulator.ByteArtifactStore
	server     *httptest.Server
	negotiated bool
	tamper     func([]byte) []byte
	requests   atomic.Int32
	// unknownLength serves bytes without Content-Length.
	unknownLength atomic.Bool
}

func (d *downloadStore) Port(ids.TenantID, ids.ProviderID, contracts.Role) contracts.Port { return d }
func (d *downloadStore) Role() contracts.Role                                             { return contracts.RoleArtifactStore }
func (d *downloadStore) Cancel(ctx context.Context, c contracts.Cancel) error             { return nil }
func (d *downloadStore) Subscribe(context.Context, contracts.Cursor) (contracts.Stream, error) {
	return nil, contracts.ErrInvalid
}
func (d *downloadStore) Requiring(capability contracts.Capability) contracts.Port {
	d.mu.Lock()
	defer d.mu.Unlock()
	if capability == contracts.CapabilityArtifactDownload && d.negotiated {
		return d
	}
	return nil
}

func (d *downloadStore) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	d.requests.Add(1)
	reply, err := d.store.Send(ctx, r)
	if err != nil || reply.Outcome != contracts.OutcomeOK {
		return reply, err
	}
	var authorized contracts.DownloadAuthorized
	if err := json.Unmarshal(reply.Result, &authorized); err != nil {
		return contracts.Reply{}, err
	}
	authorized.Download = d.server.URL + "/objects/" + strings.TrimPrefix(authorized.Download, simulator.DownloadPrefix) + "?X-Amz-Signature=secret-capability"
	reply.Result, err = json.Marshal(authorized)
	return reply, err
}

func (d *downloadStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	artifact, err := ids.Parse[ids.Artifact](strings.TrimPrefix(r.URL.Path, "/objects/"))
	if err != nil || r.URL.Query().Get("X-Amz-Signature") != "secret-capability" {
		http.NotFound(w, r)
		return
	}
	data, ok := d.store.Bytes(artifact)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	tamper := d.tamper
	d.mu.Unlock()
	if tamper != nil {
		data = tamper(data)
	}
	// S3 states the length of every object it serves; unknownLength plays a
	// store that does not.
	if !d.unknownLength.Load() {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	}
	_, _ = w.Write(data)
}

func (d *downloadStore) set(negotiated bool, tamper func([]byte) []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.negotiated, d.tamper = negotiated, tamper
}

// TestSurfaceServesArtifactsThroughTheControlPlane commits an artifact, reads
// its record and its bytes as the owner, and shows each refusal: another
// principal, a store that did not negotiate downloads, an origin the operator
// did not list, and bytes that differ from the admitted record.
func TestSurfaceServesArtifactsThroughTheControlPlane(t *testing.T) {
	download := &downloadStore{negotiated: true}
	download.server = httptest.NewTLSServer(download)
	t.Cleanup(download.server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(download.server.Certificate())
	fetcher, err := httpapi.NewArtifactFetcher([]string{download.server.URL}, roots)
	if err != nil {
		t.Fatal(err)
	}
	s := newSurfaceWith(t, httpapi.Options{ArtifactStores: download, ArtifactDownloads: fetcher})

	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "submit-a", body: s.submitBody()}), http.StatusCreated)
	rt := s.dispatch(t, sub.Operation.ID)
	storeID := mustID[ids.Provider](t)
	if download.store, err = simulator.NewByteArtifactStore(simulator.Config{Tenant: s.tenant, Provider: storeID}); err != nil {
		t.Fatal(err)
	}
	// More than the held tail, so the relay streams some bytes before it has
	// checked the whole body.
	review := []byte(strings.Repeat("The repository is in good shape.\n", 4000))
	rt.WithArtifactExport(download.store, map[string][]byte{"review": review})
	s.observe(t, rt, sub,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryResultProposed, contracts.ResultProposed{SessionRef: "s-1", TurnRef: "t-1", SourceRevision: "source-sha-1", Artifacts: []contracts.ArtifactDeclaration{
			{NativeRef: "review", Kind: lifecycle.ArtifactAgentClaim, Digest: identity.DigestOf(review), Size: uint64(len(review)), MediaType: "text/markdown"},
		}}),
		row(3, contracts.EntryTurnFinished, contracts.TurnFinished{SessionRef: "s-1", TurnRef: "t-1", Outcome: lifecycle.TurnCompleted, EndedAt: time.Now().UTC()}),
	)
	cfg := worker.DefaultConfig()
	cfg.Policy.BaseBackoff, cfg.Policy.MaxBackoff = time.Millisecond, time.Millisecond
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "surface-artifact-downloads",
		wiretest.Ports(t, s.pool, s.tenant, map[ids.ProviderID]contracts.Port{s.provider: rt},
			wiretest.ArtifactStores(map[ids.ProviderID]worker.ArtifactStorePort{s.provider: {Provider: storeID, Port: download.store}})))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}
	taskPath := "/tasks/" + sub.Task.ID.String()
	view := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: taskPath, as: "owner"}), http.StatusOK)
	if len(view.Revisions) != 1 || len(view.Revisions[0].Revision.Artifacts) != 1 {
		t.Fatalf("task %s with revisions %+v", view.Task.State, view.Revisions)
	}
	artifactPath := taskPath + "/artifacts/" + view.Revisions[0].Revision.Artifacts[0].String()

	record := decodeInto[lifecycle.Artifact](t, s.do(t, call{method: "GET", path: artifactPath, as: "owner"}), http.StatusOK)
	if record.Digest != identity.DigestOf(review) || record.Size != uint64(len(review)) || record.MediaType != "text/markdown" || record.Task != sub.Task.ID || record.CommittedAt.IsZero() {
		t.Fatalf("record %+v", record)
	}
	if r := s.do(t, call{method: "GET", path: artifactPath, as: "reader"}); r.status != http.StatusOK {
		t.Fatalf("tenant reader: %d", r.status)
	}

	content := s.do(t, call{method: "GET", path: artifactPath + "/content", as: "owner"})
	if content.status != http.StatusOK || string(content.body) != string(review) {
		t.Fatalf("content %d, %d bytes", content.status, len(content.body))
	}
	sum := sha256.Sum256(review)
	for header, want := range map[string]string{
		"Content-Type":            "text/markdown",
		"Repr-Digest":             "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":",
		"ETag":                    `"` + string(record.Digest) + `"`,
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'",
		"Content-Disposition":     `attachment; filename="` + record.Artifact.String() + `"`,
	} {
		if got := content.header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if strings.Contains(string(content.body), "secret-capability") || strings.Contains(content.header.Get("Location"), "secret") {
		t.Fatal("the store's capability reached the surface")
	}

	// Not found stays not found: another principal, an unknown artifact, an
	// artifact under another task.
	for _, c := range []call{
		{method: "GET", path: artifactPath, as: "other"},
		{method: "GET", path: artifactPath + "/content", as: "other"},
		{method: "GET", path: taskPath + "/artifacts/" + mustID[ids.Artifact](t).String(), as: "owner"},
		{method: "GET", path: "/tasks/" + mustID[ids.Task](t).String() + "/artifacts/" + record.Artifact.String(), as: "owner"},
	} {
		if r := s.do(t, c); r.status != http.StatusNotFound {
			t.Fatalf("%s as %s: %d %s", c.path, c.as, r.status, r.body)
		}
	}

	// A store that did not negotiate downloads is never sent one.
	before := download.requests.Load()
	download.set(false, nil)
	if r := s.do(t, call{method: "GET", path: artifactPath + "/content", as: "owner"}); r.status != http.StatusServiceUnavailable || !strings.Contains(string(r.body), "artifact-unavailable") {
		t.Fatalf("without the capability: %d %s", r.status, r.body)
	}
	if download.requests.Load() != before {
		t.Fatal("authorize_download sent to a store that did not select artifact.download")
	}

	// Bytes of the right length but another digest are never delivered
	// complete: the response is aborted.
	download.set(true, func(b []byte) []byte {
		changed := append([]byte(nil), b...)
		changed[len(changed)-1] ^= 1
		return changed
	})
	if body, err := s.rawContent(t, artifactPath); err == nil {
		t.Fatalf("a body with the wrong digest was delivered complete (%d bytes)", len(body))
	}
	// Bytes of another length are refused before anything is sent.
	download.set(true, func(b []byte) []byte { return append(b, '!') })
	if r := s.do(t, call{method: "GET", path: artifactPath + "/content", as: "owner"}); r.status != http.StatusBadGateway || !strings.Contains(string(r.body), "artifact-mismatch") {
		t.Fatalf("longer bytes: %d %s", r.status, r.body)
	}
	// Without a stated length, extra bytes are found while relaying, and the
	// response is aborted instead.
	download.unknownLength.Store(true)
	if body, err := s.rawContent(t, artifactPath); err == nil {
		t.Fatalf("a longer body of unknown length was delivered complete (%d bytes)", len(body))
	}
	download.unknownLength.Store(false)
	download.set(true, nil)

	// dac-plane fetches only from listed origins.
	other, err := httpapi.NewArtifactFetcher([]string{"https://elsewhere.example"}, roots)
	if err != nil {
		t.Fatal(err)
	}
	restricted := newSurfaceOn(t, s, httpapi.Options{ArtifactStores: download, ArtifactDownloads: other})
	if r := restricted.do(t, call{method: "GET", path: artifactPath + "/content", as: "owner"}); r.status != http.StatusServiceUnavailable {
		t.Fatalf("unlisted origin: %d %s", r.status, r.body)
	}
	unconfigured := newSurfaceOn(t, s, httpapi.Options{ArtifactStores: download})
	if r := unconfigured.do(t, call{method: "GET", path: artifactPath + "/content", as: "owner"}); r.status != http.StatusServiceUnavailable {
		t.Fatalf("downloads not configured: %d %s", r.status, r.body)
	}
	if r := unconfigured.do(t, call{method: "GET", path: artifactPath, as: "owner"}); r.status != http.StatusOK {
		t.Fatalf("record without downloads: %d", r.status)
	}
}

// newSurfaceOn serves another Surface API over base's database and issuer,
// with other options.
func newSurfaceOn(t *testing.T, base *surface, options httpapi.Options) *surface {
	t.Helper()
	config := authn.Config{Issuers: []authn.IssuerConfig{{Issuer: base.issuer.URL(), Audience: audience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 3600}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator, err := authn.New(base.ctx, config, authn.StoreResolver(base.pool), logger, authn.WithHTTPClient(base.issuer.Client()))
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(base.pool, authenticator, logger, options)
	if err != nil {
		t.Fatal(err)
	}
	other := *base
	other.server = httptest.NewServer(api)
	t.Cleanup(other.server.Close)
	return &other
}

// rawContent reads a download and reports a transport error, which an
// aborted response produces.
func (s *surface) rawContent(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.server.URL+"/v1/tenants/"+s.tenant.String()+path+"/content", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token(t, "owner"))
	response, err := s.server.Client().Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}
