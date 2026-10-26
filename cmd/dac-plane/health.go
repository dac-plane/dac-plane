package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
)

const healthTimeout = 3 * time.Second

// runHealth probes the local operations listener and exits 0 when the probe
// answers 200. It reads only the operations address and, off loopback, the
// certificate the listener serves, so it needs no database credentials.
func runHealth(args []string, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "live" && args[0] != "ready") {
		_, _ = fmt.Fprintln(stderr, "usage: dac-plane health live | ready")
		return 2
	}
	path := "/livez"
	if args[0] == "ready" {
		path = "/readyz"
	}
	cfg := config.LoadOperations(os.LookupEnv)
	if err := probe(cfg, path); err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane health: %v\n", err)
		return 1
	}
	return 0
}

func probe(cfg config.Config, path string) error {
	host, port, err := net.SplitHostPort(cfg.OpsAddr)
	if err != nil {
		return errors.New("DAC_OPS_ADDR must be a host:port listener address")
	}
	client := &http.Client{Timeout: healthTimeout}
	scheme := "http"
	if !cfg.OpsLoopback() {
		// A listener bound off loopback serves TLS; dial it on loopback
		// and accept exactly the certificate it is configured to serve.
		host = "127.0.0.1"
		tlsConfig, err := pinnedTLSConfig(cfg.TLSCertFile)
		if err != nil {
			return err
		}
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	response, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + path)
	if err != nil {
		return errors.New("the operations listener is unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", response.Status, bytes.TrimSpace(body))
	}
	return nil
}

func pinnedTLSConfig(certFile string) (*tls.Config, error) {
	leaf, err := servedLeaf(certFile)
	if err != nil {
		return nil, err
	}
	serverName := ""
	if len(leaf.DNSNames) > 0 {
		serverName = leaf.DNSNames[0]
	} else if len(leaf.IPAddresses) > 0 {
		serverName = leaf.IPAddresses[0].String()
	}
	if serverName == "" {
		return nil, errors.New("DAC_TLS_CERT_FILE certificate needs a DNS or IP subject alternative name")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName}, nil
}

func servedLeaf(certFile string) (*x509.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, errors.New("cannot read DAC_TLS_CERT_FILE")
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("DAC_TLS_CERT_FILE holds no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
