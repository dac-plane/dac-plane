package main

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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
