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

	"github.com/dac-plane/dac-plane/adapterkit"
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

func TestClientCredentialsTellsARefusalFromAnUnavailableEndpoint(t *testing.T) {
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
		want    string
	}{
		{name: "client authentication failed", handler: answer(http.StatusUnauthorized, `{"error":"invalid_client","error_description":"canary-secret"}`), want: "refused the client credentials"},
		{name: "grant rejected", handler: answer(http.StatusBadRequest, `{"error":"unauthorized_client"}`), want: "refused the client credentials"},
		{name: "issuer starting", handler: answer(http.StatusServiceUnavailable, `Bootstrap in progress canary-secret`), want: "answered HTTP 503"},
		{name: "issuer down", closed: true, want: "could not be reached"},
		{name: "unusable answer", handler: answer(http.StatusOK, `{"access_token":`), want: "unusable response"},
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("canary-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler := c.handler
			if handler == nil {
				handler = answer(http.StatusOK, `{}`)
			}
			server := httptest.NewTLSServer(handler)
			client := server.Client()
			url := server.URL
			if c.closed {
				server.Close()
			} else {
				defer server.Close()
			}
			source, err := adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: url, ClientID: "adapter-1", SecretFile: secretFile, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "canary-secret") {
				t.Fatalf("error = %v, want one containing %q and not the secret or body", err, c.want)
			}
		})
	}
}
