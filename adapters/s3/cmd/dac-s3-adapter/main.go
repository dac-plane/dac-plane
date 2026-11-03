// Command dac-s3-adapter serves the artifact store role over the S3 API.
//
//	dac-s3-adapter            serve, after checking the buckets with the adapter key
//	dac-s3-adapter provision  create or complete both buckets with the operator key, then verify them
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
	s3adapter "github.com/dac-plane/dac-plane/adapters/s3"
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dac-s3-adapter: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	switch {
	case len(args) == 0:
		return serve(ctx, getenv)
	case len(args) == 1 && args[0] == "provision":
		return provision(ctx, getenv)
	}
	return errors.New("usage: dac-s3-adapter [provision]")
}

func serve(ctx context.Context, getenv func(string) string) error {
	store, err := storeFromEnv(getenv)
	if err != nil {
		return err
	}
	if err := store.Preflight(ctx); err != nil {
		return fmt.Errorf("store preflight: %w", err)
	}
	provider, err := required(getenv, "DAC_PROVIDER_ID")
	if err != nil {
		return err
	}
	adapterURL, err := required(getenv, "DAC_ADAPTER_URL")
	if err != nil {
		return err
	}
	var ttl time.Duration
	if value := strings.TrimSpace(getenv("DAC_S3_UPLOAD_TTL")); value != "" {
		if ttl, err = time.ParseDuration(value); err != nil {
			return errors.New("DAC_S3_UPLOAD_TTL must be a duration such as 15m")
		}
	}
	handler, err := s3adapter.New(s3adapter.Config{Store: store, ProviderID: provider, UploadTTL: ttl})
	if err != nil {
		return err
	}
	tlsConfig, err := caConfig(getenv("DAC_ADAPTER_CA_FILE"))
	if err != nil {
		return err
	}
	token, err := tokenSource(getenv, tlsConfig)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return adapterkit.Run(ctx, adapterkit.Config{URL: adapterURL, Token: token, Provider: provider, Role: wire.RoleArtifactStore, Handler: handler, Offered: []string{wire.CapabilityOperationReplay}, TLS: tlsConfig, Logger: logger})
}

func provision(ctx context.Context, getenv func(string) string) error {
	store, err := storeFromEnv(getenv)
	if err != nil {
		return err
	}
	objectDays, err := days(getenv, "DAC_S3_RETENTION_DAYS", 30)
	if err != nil {
		return err
	}
	stagingDays, err := days(getenv, "DAC_S3_STAGING_DAYS", 2)
	if err != nil {
		return err
	}
	return store.Provision(ctx, s3adapter.Retention{ObjectDays: objectDays, StagingDays: stagingDays})
}

func storeFromEnv(getenv func(string) string) (*s3adapter.S3Store, error) {
	cfg := s3adapter.S3Config{PublicEndpoint: strings.TrimSpace(getenv("DAC_S3_PUBLIC_ENDPOINT")), Region: strings.TrimSpace(getenv("DAC_S3_REGION"))}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	var err error
	for name, target := range map[string]*string{"DAC_S3_ENDPOINT": &cfg.Endpoint, "DAC_S3_ACCESS_KEY_ID": &cfg.AccessKey, "DAC_S3_STAGING_BUCKET": &cfg.StagingBucket, "DAC_S3_OBJECTS_BUCKET": &cfg.ObjectsBucket} {
		if *target, err = required(getenv, name); err != nil {
			return nil, err
		}
	}
	secretFile, err := required(getenv, "DAC_S3_SECRET_ACCESS_KEY_FILE")
	if err != nil {
		return nil, err
	}
	if cfg.SecretKey, err = readSecret(secretFile); err != nil {
		return nil, errors.New("cannot read S3 secret access key")
	}
	tlsConfig, err := caConfig(getenv("DAC_S3_CA_FILE"))
	if err != nil {
		return nil, err
	}
	cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	return s3adapter.NewS3Store(cfg)
}

func required(getenv func(string) string, name string) (string, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func days(getenv func(string) string, name string, fallback int32) (int32, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of days", name)
	}
	return int32(n), nil
}

// caConfig trusts the system roots, or only the given CA file when one is set.
func caConfig(path string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	path = strings.TrimSpace(path)
	if path == "" {
		return config, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file has no certificates")
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

func tokenSource(getenv func(string) string, tlsConfig *tls.Config) (adapterkit.TokenSource, error) {
	if path := strings.TrimSpace(getenv("DAC_ADAPTER_TOKEN_FILE")); path != "" {
		return adapterkit.FileToken(path), nil
	}
	tokenURL, clientID, secretFile, scope := strings.TrimSpace(getenv("DAC_ADAPTER_TOKEN_URL")), strings.TrimSpace(getenv("DAC_ADAPTER_CLIENT_ID")), strings.TrimSpace(getenv("DAC_ADAPTER_CLIENT_SECRET_FILE")), strings.TrimSpace(getenv("DAC_ADAPTER_SCOPE"))
	if tokenURL == "" || clientID == "" || secretFile == "" || scope == "" {
		return nil, errors.New("adapter token file or complete client-credentials settings are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig.Clone()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: tokenURL, ClientID: clientID, SecretFile: secretFile, Scopes: []string{scope}, HTTPClient: client})
}
