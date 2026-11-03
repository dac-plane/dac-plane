package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

func TestHTTPSUploaderSendsWhatARuntimeSends(t *testing.T) {
	var got struct {
		method, contentType, body string
		length                    int64
		headers                   int
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		got.method, got.contentType, got.body, got.length = r.Method, r.Header.Get("Content-Type"), string(data), r.ContentLength
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	upload := httpsUploader(&tls.Config{RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12})
	transfer := contracts.ArtifactTransfer{Manifest: lifecycle.ArtifactManifest{MediaType: "text/plain"}, Upload: server.URL + "/dac-staging/key?X-Amz-Signature=secret"}
	if err := upload(context.Background(), transfer, []byte("delegated result\n")); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPut || got.contentType != "text/plain" || got.body != "delegated result\n" || got.length != 17 {
		t.Fatalf("upload sent %+v", got)
	}
	transfer.Upload += "&fail=1"
	if err := upload(context.Background(), transfer, []byte("x")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("refused upload: %v", err)
	}
	transfer.Upload = "http://store.test/key"
	if err := upload(context.Background(), transfer, []byte("x")); err == nil {
		t.Fatal("plain HTTP capability accepted")
	}
}

func TestArtifactExportExcludesArtifactStore(t *testing.T) {
	s := scenarioFile{Tenant: "01a167fd-51d9-76ba-b563-496e2bd89646", Provider: "01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10", Role: "runtime"}
	s.ArtifactExport = &struct {
		Fixtures map[string]string `json:"fixtures"`
	}{Fixtures: map[string]string{"patch": "x"}}
	s.ArtifactStore = &struct {
		Provider string            `json:"provider_id"`
		Fixtures map[string]string `json:"fixtures"`
	}{Provider: "01a167fd-51d9-7d33-8a3b-6c5d4e3f2a11"}
	t.Setenv("DAC_ADAPTER_TOKEN_FILE", t.TempDir()+"/token")
	if _, err := s.adapters(nil, false); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("both export modes accepted: %v", err)
	}
}
