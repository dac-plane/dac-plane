package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthDialHostUsesLoopbackOnlyForWildcardBinds(t *testing.T) {
	for input, want := range map[string]string{
		"":             "127.0.0.1",
		"0.0.0.0":      "127.0.0.1",
		"::":           "::1",
		"10.0.0.5":     "10.0.0.5",
		"ops.internal": "ops.internal",
	} {
		if got := healthDialHost(input); got != want {
			t.Errorf("healthDialHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHealthServerNameMakesWildcardConcrete(t *testing.T) {
	if got := healthServerName(&x509.Certificate{DNSNames: []string{"*.ops.example.com"}}); got != "health.ops.example.com" {
		t.Fatalf("healthServerName() = %q, want health.ops.example.com", got)
	}
	if got := healthServerName(&x509.Certificate{IPAddresses: []net.IP{net.ParseIP("10.0.0.5")}}); got != "10.0.0.5" {
		t.Fatalf("healthServerName() = %q, want 10.0.0.5", got)
	}
}

func TestPinnedTLSConfigUsesStandardVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	certificate := server.Certificate()
	certFile := filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := pinnedTLSConfig(certFile)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := tls.Dial("tcp", server.Listener.Addr().String(), config)
	if err != nil {
		t.Fatalf("pinned certificate did not verify: %v", err)
	}
	_ = connection.Close()

	wrongName := config.Clone()
	wrongName.ServerName = "not-the-configured-server.invalid"
	if connection, err = tls.Dial("tcp", server.Listener.Addr().String(), wrongName); err == nil {
		_ = connection.Close()
		t.Fatal("the pinned certificate verified for another server name")
	}
}
