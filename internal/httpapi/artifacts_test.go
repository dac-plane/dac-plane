package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// TestStalledStoreEndsTheDownload plays a store that sends its headers and
// part of the body, then nothing: the relay ends after the stall timeout
// instead of waiting for the client to give up.
func TestStalledStoreEndsTheDownload(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 200<<10)
	released := make(chan struct{})
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data[:100<<10])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer store.Close()
	defer close(released)
	roots := x509.NewCertPool()
	roots.AddCert(store.Certificate())
	fetcher, err := NewArtifactFetcher([]string{store.URL}, roots)
	if err != nil {
		t.Fatal(err)
	}
	fetcher.stall = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := fetcher.open(ctx, store.URL+"/objects/a?X-Amz-Signature=s")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := guardStall(response.Body, fetcher.stall, cancel)
	defer body.stop()
	sum := sha256.Sum256(data)
	record := lifecycle.Artifact{ArtifactManifest: lifecycle.ArtifactManifest{Size: uint64(len(data)), Digest: identity.DigestOf(data)}}

	var sent bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- relayChecked(&sent, body, record, sum[:]) }()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("relay ended with %v, want the cancelled read", err)
		}
		if sent.Len() >= len(data) {
			t.Fatal("the whole body was delivered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay still waits on a stalled store")
	}
}

// slowClient takes longer to accept each write than the stall timeout.
type slowClient struct {
	bytes.Buffer
	pause time.Duration
}

func (c *slowClient) Write(p []byte) (int, error) {
	time.Sleep(c.pause)
	return c.Buffer.Write(p)
}

// TestSlowClientIsNotCutShort pauses on every write for longer than the
// stall timeout while the store answers at once: only waiting on the store
// counts, so the whole body is relayed.
func TestSlowClientIsNotCutShort(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 300<<10)
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer store.Close()
	roots := x509.NewCertPool()
	roots.AddCert(store.Certificate())
	fetcher, err := NewArtifactFetcher([]string{store.URL}, roots)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := fetcher.open(ctx, store.URL+"/objects/a")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	stall := 50 * time.Millisecond
	body := guardStall(response.Body, stall, cancel)
	defer body.stop()
	sum := sha256.Sum256(data)
	record := lifecycle.Artifact{ArtifactManifest: lifecycle.ArtifactManifest{Size: uint64(len(data)), Digest: identity.DigestOf(data)}}
	client := &slowClient{pause: 3 * stall}
	if err := relayChecked(client, body, record, sum[:]); err != nil {
		t.Fatalf("a slow client was cut short: %v", err)
	}
	if !bytes.Equal(client.Bytes(), data) {
		t.Fatalf("relayed %d of %d bytes", client.Len(), len(data))
	}
}

// TestFetcherRefusesUnlistedOriginsAndRedirects keeps dac-plane on the
// operator's origins: another host is refused before any request, and a
// redirect from a listed one is not followed. Neither error names the URL.
func TestFetcherRefusesUnlistedOriginsAndRedirects(t *testing.T) {
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/secret", http.StatusFound)
	}))
	defer store.Close()
	roots := x509.NewCertPool()
	roots.AddCert(store.Certificate())
	fetcher, err := NewArtifactFetcher([]string{store.URL}, roots)
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"https://elsewhere.example/a?X-Amz-Signature=secret", store.URL + "/a?X-Amz-Signature=secret"} {
		_, err := fetcher.open(t.Context(), url)
		if err == nil {
			t.Fatalf("%s was fetched", url)
		}
		if bytes.Contains([]byte(err.Error()), []byte("secret")) {
			t.Fatalf("error names the capability: %v", err)
		}
	}
	for _, origin := range []string{"http://store:8333", "https://store:8333/path", "https://user@store"} {
		if _, err := NewArtifactFetcher([]string{origin}, nil); err == nil {
			t.Fatalf("origin %s accepted", origin)
		}
	}
}
