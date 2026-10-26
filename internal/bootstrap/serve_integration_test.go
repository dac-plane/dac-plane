//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/oidctest"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

// serving is one control-plane process under test with its configuration.
type serving struct {
	cfg      config.Config
	issuer   *oidctest.Issuer
	db       *sql.DB
	client   *http.Client
	logs     *bytes.Buffer
	app      *Application
	stop     context.CancelFunc
	finished chan error
	result   error
	once     sync.Once
}

func newServing(t *testing.T, development bool, deployment config.Deployment) *serving {
	t.Helper()
	databaseURL := testpostgres.Start(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrations.NewLockedProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile, pool := writeServerCertificate(t, dir)
	issuer := oidctest.New(t)
	identity := map[string]any{
		"issuers":         []any{map[string]any{"issuer": issuer.URL(), "audience": issuer.Audience, "algorithms": []string{"RS256"}, "max_token_lifetime_seconds": 3600, "development": development}},
		"adapter_issuers": []any{map[string]any{"issuer": issuer.URL(), "audience": "dac-plane-adapters", "algorithms": []string{"RS256"}, "max_token_lifetime_seconds": 3600}},
	}
	identityFile := filepath.Join(dir, "identity.json")
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DatabaseURL: databaseURL, DatabaseConnectTimeout: 30 * time.Second, IdentityConfigFile: identityFile,
		SurfaceAddr: freeAddr(t), AdapterAddr: freeAddr(t), OpsAddr: freeAddr(t),
		TLSCertFile: certFile, TLSKeyFile: keyFile, Deployment: deployment,
	}
	logs := &bytes.Buffer{}
	app, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return &serving{cfg: cfg, issuer: issuer, db: db, client: client, logs: logs, app: app}
}

func (s *serving) start(t *testing.T) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	s.stop, s.finished = stop, make(chan error, 1)
	go func() { s.finished <- s.app.Serve(ctx, s.cfg, WithIssuerClient(s.issuer.Client())) }()
	t.Cleanup(func() { _ = s.wait() })
}

// wait stops the process and returns what Serve returned.
func (s *serving) wait() error {
	s.once.Do(func() {
		s.stop()
		s.result = <-s.finished
	})
	return s.result
}

// get requests path from the listener at addr, over TLS unless plain.
func (s *serving) get(t *testing.T, addr, path string, plain bool) (int, string) {
	t.Helper()
	scheme, client := "https", s.client
	if plain {
		scheme, client = "http", &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Get(scheme + "://" + addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func (s *serving) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := s.get(t, s.cfg.OpsAddr, "/readyz", true); code == http.StatusOK {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("never ready")
}

func TestServeExposesTLSListenersAndReadiness(t *testing.T) {
	s := newServing(t, false, config.DeploymentProduction)
	s.start(t)
	s.waitReady(t)

	if code, body := s.get(t, s.cfg.OpsAddr, "/livez", true); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("livez = %d %q", code, body)
	}
	if code, body := s.get(t, s.cfg.OpsAddr, "/readyz", true); code != http.StatusOK || strings.TrimSpace(body) != "ready" {
		t.Fatalf("readyz = %d %q", code, body)
	}
	// Both public listeners speak TLS only and authenticate before anything.
	if code, _ := s.get(t, s.cfg.SurfaceAddr, "/v1/tenants/01a167fd-51d9-76ba-b563-496e2bd89646/tasks/01a167fd-51d9-76be-8ef5-39a7e14723c0", false); code != http.StatusUnauthorized {
		t.Fatalf("Surface API without a token = %d, want 401", code)
	}
	if code, _ := s.get(t, s.cfg.AdapterAddr, "/", false); code != http.StatusUnauthorized {
		t.Fatalf("adapter endpoint without a token = %d, want 401", code)
	}
	if code, _ := s.get(t, s.cfg.SurfaceAddr, "/", true); code == http.StatusOK || code == http.StatusUnauthorized {
		t.Fatalf("Surface API answered plaintext HTTP with %d", code)
	}

	// Readiness follows the schema: a database moved outside the release's
	// range is not ready, and the bodies never carry configuration.
	provider, err := migrations.NewLockedProvider(s.db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(context.Background(), migrations.MinServeVersion-1); err != nil {
		t.Fatal(err)
	}
	code, body := s.get(t, s.cfg.OpsAddr, "/readyz", true)
	if code != http.StatusServiceUnavailable || strings.TrimSpace(body) != "schema out of range" {
		t.Fatalf("readyz below the range = %d %q", code, body)
	}
	for _, secret := range []string{s.cfg.DatabaseURL, s.cfg.IdentityConfigFile, s.issuer.URL()} {
		if strings.Contains(body, secret) {
			t.Fatalf("readiness body leaked configuration: %q", body)
		}
	}

	if err := s.wait(); err != nil {
		t.Fatalf("Serve() after cancellation = %v", err)
	}
	if strings.Contains(s.logs.String(), s.cfg.DatabaseURL) {
		t.Fatal("logs contain the database URL")
	}
}

func TestServeRefusesAnOutOfRangeSchema(t *testing.T) {
	s := newServing(t, false, config.DeploymentProduction)
	provider, err := migrations.NewLockedProvider(s.db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(context.Background(), migrations.MinServeVersion-1); err != nil {
		t.Fatal(err)
	}
	err = s.app.Serve(context.Background(), s.cfg, WithIssuerClient(s.issuer.Client()))
	if !errors.Is(err, migrations.ErrSchemaOutOfRange) {
		t.Fatalf("Serve() error = %v, want ErrSchemaOutOfRange", err)
	}
	if conn, err := net.DialTimeout("tcp", s.cfg.SurfaceAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("a listener opened for an out-of-range schema")
	}
}

func TestServeRefusesADevelopmentIssuerInProduction(t *testing.T) {
	s := newServing(t, true, config.DeploymentProduction)
	err := s.app.Serve(context.Background(), s.cfg, WithIssuerClient(s.issuer.Client()))
	if err == nil || !strings.Contains(err.Error(), "development issuer") {
		t.Fatalf("Serve() error = %v, want the development issuer refused", err)
	}

	development := newServing(t, true, config.DeploymentDevelopment)
	development.start(t)
	development.waitReady(t)
}

// writeServerCertificate writes a self-signed certificate for 127.0.0.1 and
// returns its files and a pool trusting it.
func writeServerCertificate(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dac-plane test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(certificate)
	return certFile, keyFile, pool
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}
