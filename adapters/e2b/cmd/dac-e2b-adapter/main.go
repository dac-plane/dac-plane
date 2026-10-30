package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	e2b "github.com/AssahBismarkabah/dac-plane/adapters/e2b"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dac-e2b-adapter: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	get := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	base, err := get("DAC_E2B_BASE_URL")
	if err != nil {
		return err
	}
	keyFile, err := get("DAC_E2B_API_KEY_FILE")
	if err != nil {
		return err
	}
	key, err := readSecret(keyFile)
	if err != nil {
		return errors.New("cannot read E2B API credential")
	}
	template, err := get("DAC_E2B_TEMPLATE_ID")
	if err != nil {
		return err
	}
	provider, err := get("DAC_PROVIDER_ID")
	if err != nil {
		return err
	}
	statePath, err := get("DAC_E2B_STATE")
	if err != nil {
		return err
	}
	adapterURL, err := get("DAC_ADAPTER_URL")
	if err != nil {
		return err
	}
	journal, err := journal.Open(statePath)
	if err != nil {
		return err
	}
	defer journal.Close()
	var sandboxTimeout time.Duration
	if value := strings.TrimSpace(os.Getenv("DAC_E2B_SANDBOX_TIMEOUT")); value != "" {
		if sandboxTimeout, err = time.ParseDuration(value); err != nil {
			return errors.New("DAC_E2B_SANDBOX_TIMEOUT must be a duration such as 1h")
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	handler, err := e2b.New(e2b.Config{BaseURL: base, APIKey: key, TemplateID: template, ProviderID: provider, HTTPClient: client, Journal: journal, SandboxTimeout: sandboxTimeout})
	if err != nil {
		return err
	}
	tlsConfig, err := adapterTLS()
	if err != nil {
		return err
	}
	token, err := tokenSource(tlsConfig)
	if err != nil {
		return err
	}
	return adapterkit.Run(ctx, adapterkit.Config{URL: adapterURL, Token: token, Provider: provider, Role: wire.RoleEnvironment, Handler: handler, TLS: tlsConfig, Logger: logger})
}

// adapterTLS configures the connections to the control plane and its token
// issuer only. The E2B API client keeps the system roots.
func adapterTLS() (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	path := strings.TrimSpace(os.Getenv("DAC_ADAPTER_CA_FILE"))
	if path == "" {
		return config, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read adapter CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("adapter CA file has no certificates")
	}
	config.RootCAs = roots
	return config, nil
}

func readSecret(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("secret file must be regular")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("secret file is empty")
	}
	return value, nil
}

func tokenSource(tlsConfig *tls.Config) (adapterkit.TokenSource, error) {
	if path := strings.TrimSpace(os.Getenv("DAC_ADAPTER_TOKEN_FILE")); path != "" {
		return adapterkit.FileToken(path), nil
	}
	tokenURL, clientID, secretFile, scope := strings.TrimSpace(os.Getenv("DAC_ADAPTER_TOKEN_URL")), strings.TrimSpace(os.Getenv("DAC_ADAPTER_CLIENT_ID")), strings.TrimSpace(os.Getenv("DAC_ADAPTER_CLIENT_SECRET_FILE")), strings.TrimSpace(os.Getenv("DAC_ADAPTER_SCOPE"))
	if tokenURL == "" || clientID == "" || secretFile == "" || scope == "" {
		return nil, errors.New("adapter token file or complete client-credentials settings are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig.Clone()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: tokenURL, ClientID: clientID, SecretFile: secretFile, Scopes: []string{scope}, HTTPClient: client})
}
