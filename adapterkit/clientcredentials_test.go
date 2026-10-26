package adapterkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
)

func TestClientCredentialsFetchesATokenWithTheRotatedSecret(t *testing.T) {
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if err := r.ParseForm(); err != nil || !ok || id != "adapter-1" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "dac-plane-adapters" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		seen = append(seen, secret)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-for-" + secret, "token_type": "Bearer", "expires_in": 300})
	}))
	defer server.Close()
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{
		TokenURL: server.URL + "/token", ClientID: "adapter-1", SecretFile: secretFile,
		Scopes: []string{"dac-plane-adapters"}, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := source(context.Background())
	if err != nil || token.Value != "token-for-first" || time.Until(token.Expiry) < 4*time.Minute {
		t.Fatalf("token = %+v, %v", token, err)
	}
	if err := os.WriteFile(secretFile, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err = source(context.Background()); err != nil || token.Value != "token-for-second" {
		t.Fatalf("after rotation token = %+v, %v", token, err)
	}
	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("secrets presented = %v", seen)
	}
}

func TestClientCredentialsErrorsNeverCarryTheSecret(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_client","error_description":"bad secret canary-secret"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("canary-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: server.URL, ClientID: "adapter-1", SecretFile: secretFile, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source(context.Background()); err == nil || strings.Contains(err.Error(), "canary-secret") {
		t.Fatalf("error = %v, want a refusal without the secret", err)
	}
	if _, err := adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: server.URL}); err == nil {
		t.Fatal("ClientCredentials accepted a configuration without a client")
	}
}
