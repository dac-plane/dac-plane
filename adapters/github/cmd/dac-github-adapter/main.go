package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
	githubadapter "github.com/dac-plane/dac-plane/adapters/github"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dac-github-adapter:", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	get := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	tenant, provider, client, callback, returnURL := get("DAC_TENANT_ID"), get("DAC_PROVIDER_ID"), get("DAC_GITHUB_CLIENT_ID"), get("DAC_GITHUB_CALLBACK_URL"), get("DAC_GITHUB_RETURN_URL")
	vaultPath, keyPath, secretPath := get("DAC_GITHUB_VAULT_FILE"), get("DAC_GITHUB_VAULT_KEY_FILE"), get("DAC_GITHUB_CLIENT_SECRET_FILE")
	if vaultPath == "" || keyPath == "" || secretPath == "" {
		return errors.New("GitHub vault file, vault key file and client secret file are required")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return errors.New("cannot read vault key file")
	}
	vault, err := githubadapter.OpenVault(vaultPath, key)
	if err != nil {
		return err
	}
	defer vault.Close()
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		return errors.New("cannot read GitHub client secret file")
	}
	handler, err := githubadapter.New(githubadapter.Config{Vault: vault, TenantID: tenant, ProviderID: provider, ClientID: client, ClientSecret: strings.TrimSpace(string(secret)), CallbackURL: callback, ReturnURL: returnURL})
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca := get("DAC_ADAPTER_CA_FILE"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return errors.New("cannot read adapter CA file")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("adapter CA file has no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	var token adapterkit.TokenSource
	if file := get("DAC_ADAPTER_TOKEN_FILE"); file != "" {
		token = adapterkit.FileToken(file)
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsConfig.Clone()
		token, err = adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{TokenURL: get("DAC_ADAPTER_TOKEN_URL"), ClientID: get("DAC_ADAPTER_CLIENT_ID"), SecretFile: get("DAC_ADAPTER_CLIENT_SECRET_FILE"), Scopes: []string{get("DAC_ADAPTER_SCOPE")}, HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second}})
		if err != nil {
			return err
		}
	}
	addr := get("DAC_GITHUB_CALLBACK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9080"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("invalid callback listen address")
	}
	cert, keyFile := get("DAC_GITHUB_TLS_CERT_FILE"), get("DAC_GITHUB_TLS_KEY_FILE")
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if (!loopback || cert != "" || keyFile != "") && (cert == "" || keyFile == "") {
		return errors.New("callback TLS certificate and key required off loopback")
	}
	parsed, err := url.Parse(callback)
	if err != nil {
		return errors.New("invalid callback URL")
	}
	mux := http.NewServeMux()
	mux.Handle(parsed.Path, http.HandlerFunc(handler.Callback))
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	errorsCh := make(chan error, 2)
	go func() {
		if cert != "" {
			errorsCh <- server.ListenAndServeTLS(cert, keyFile)
		} else {
			errorsCh <- server.ListenAndServe()
		}
	}()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	go func() {
		errorsCh <- adapterkit.Run(ctx, adapterkit.Config{URL: get("DAC_ADAPTER_URL"), Provider: provider, Role: wire.RoleConnector, Token: token, Handler: handler, TLS: tlsConfig, Logger: logger})
	}()
	var result error
	select {
	case result = <-errorsCh:
	case <-ctx.Done():
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	if errors.Is(result, http.ErrServerClosed) || errors.Is(result, context.Canceled) {
		return nil
	}
	return result
}
