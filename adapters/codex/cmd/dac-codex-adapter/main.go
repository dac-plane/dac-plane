package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	codex "github.com/AssahBismarkabah/dac-plane/adapters/codex"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/appserver"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/journal"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dac-codex-adapter: %v\n", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	adapterURL, err := required("DAC_ADAPTER_URL")
	if err != nil {
		return err
	}
	providerID, err := required("DAC_PROVIDER_ID")
	if err != nil {
		return err
	}
	binary, err := required("DAC_CODEX_APP_SERVER")
	if err != nil {
		return err
	}
	home, err := required("DAC_CODEX_HOME")
	if err != nil {
		return err
	}
	statePath, err := required("DAC_CODEX_STATE")
	if err != nil {
		return err
	}
	workspace, err := required("DAC_CODEX_WORKSPACE")
	if err != nil {
		return err
	}
	model, err := required("DAC_CODEX_MODEL")
	if err != nil {
		return err
	}
	modelProvider, err := required("DAC_CODEX_MODEL_PROVIDER")
	if err != nil {
		return err
	}
	baseURL, err := required("DAC_CODEX_BASE_URL")
	if err != nil {
		return err
	}
	apiKeyFile, err := required("DAC_CODEX_API_KEY_FILE")
	if err != nil {
		return err
	}
	apiKey, err := readSecret(apiKeyFile)
	if err != nil {
		return errors.New("cannot read Codex provider credential")
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-app-server "+codex.AppServerVersion {
		return fmt.Errorf("Codex app-server must be exactly %s", codex.AppServerVersion)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	native, err := appserver.Start(ctx, appserver.Config{Binary: binary, Home: home, Provider: modelProvider, Model: model, BaseURL: baseURL, APIKeyEnv: "DAC_CODEX_PROVIDER_API_KEY", APIKey: apiKey, Logger: logger})
	if err != nil {
		return err
	}
	defer native.Close()
	state, err := journal.Open(statePath)
	if err != nil {
		return err
	}
	defer state.Close()
	tlsConfig, err := adapterTLS()
	if err != nil {
		return err
	}
	httpClient := clientForTLS(tlsConfig)
	handler, err := codex.New(codex.Config{Native: native, Journal: state, Workspace: workspace, Model: model, Provider: modelProvider, ProviderID: providerID, HTTPClient: httpClient, Logger: logger})
	if err != nil {
		return err
	}
	token, err := tokenSource(tlsConfig)
	if err != nil {
		return err
	}
	processFailure := make(chan error, 1)
	go func() {
		select {
		case <-native.Done():
			if ctx.Err() == nil {
				err := native.Err()
				if err == nil {
					err = errors.New("Codex app-server exited")
				}
				processFailure <- fmt.Errorf("Codex app-server exited: %w", err)
				cancel()
			}
		case err := <-handler.Fatal():
			processFailure <- fmt.Errorf("adapter event handling failed: %w", err)
			cancel()
		case <-ctx.Done():
		}
	}()
	runErr := adapterkit.Run(ctx, adapterkit.Config{URL: adapterURL, Token: token, Provider: providerID, Role: wire.RoleRuntime, Handler: handler, TLS: tlsConfig, Logger: logger})
	select {
	case failure := <-processFailure:
		return failure
	default:
		return runErr
	}
}

func tokenSource(tlsConfig *tls.Config) (adapterkit.TokenSource, error) {
	if path := strings.TrimSpace(os.Getenv("DAC_ADAPTER_TOKEN_FILE")); path != "" {
		return adapterkit.FileToken(path), nil
	}
	tokenURL := strings.TrimSpace(os.Getenv("DAC_ADAPTER_TOKEN_URL"))
	clientID := strings.TrimSpace(os.Getenv("DAC_ADAPTER_CLIENT_ID"))
	secretFile := strings.TrimSpace(os.Getenv("DAC_ADAPTER_CLIENT_SECRET_FILE"))
	scope := strings.TrimSpace(os.Getenv("DAC_ADAPTER_SCOPE"))
	if tokenURL == "" || clientID == "" || secretFile == "" || scope == "" {
		return nil, errors.New("adapter token file or complete client credentials are required")
	}
	return adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: tokenURL, ClientID: clientID, SecretFile: secretFile, Scopes: []string{scope}, HTTPClient: clientForTLS(tlsConfig)})
}

func clientForTLS(tlsConfig *tls.Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
func adapterTLS() (*tls.Config, error) {
	path := strings.TrimSpace(os.Getenv("DAC_ADAPTER_CA_FILE"))
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read adapter CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("adapter CA file has no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, nil
}
func readSecret(path string) (string, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil || len(data) > 16<<10 {
		return "", errors.New("secret is too large")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("secret is empty")
	}
	return value, nil
}
