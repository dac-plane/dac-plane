package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdapterTLSTrustsOnlyTheConfiguredCA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(valid, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.crt")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DAC_ADAPTER_CA_FILE", "")
	if config, err := adapterTLS(); err != nil || config.RootCAs != nil {
		t.Fatalf("unset CA = %+v, %v; want system roots", config, err)
	}
	t.Setenv("DAC_ADAPTER_CA_FILE", valid)
	if config, err := adapterTLS(); err != nil || config.RootCAs == nil {
		t.Fatalf("configured CA = %+v, %v; want a private root pool", config, err)
	}
	for _, path := range []string{empty, filepath.Join(dir, "missing.crt")} {
		t.Setenv("DAC_ADAPTER_CA_FILE", path)
		if _, err := adapterTLS(); err == nil {
			t.Fatalf("CA file %s was accepted", path)
		}
	}
}
