// Package wiretest runs provider ports as out-of-process adapters for the
// integration suites (ADR-0008): each port is served by the adapter kit over
// a real TLS WebSocket to the control plane's adapter endpoint, authenticated
// with a client-credentials token from a test issuer and a provisioned
// binding, and the worker resolves it through the connection registry. The
// adapter runs in the test binary, so scenario scripts, pause control and
// port decorators keep working; everything between the worker and the
// adapter is the production transport. It is test support only.
package wiretest

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/adapterwire"
	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/oidctest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// Audience is the adapter endpoint's audience in test tokens.
const Audience = "https://dac-plane.example/adapters"

// Wire is one test's adapter endpoint and the adapters connected to it.
type Wire struct {
	Registry *adapterwire.Registry

	pool   atomic.Pointer[pgxpool.Pool]
	issuer *oidctest.Issuer
	key    *rsa.PrivateKey
	server *httptest.Server
	ctx    context.Context

	logs *syncBuffer

	mu      sync.Mutex
	served  map[servedKey]*served
	stores  map[servedKey]ids.ProviderID
	clients sync.WaitGroup
	next    int
}

type servedKey struct {
	tenant   ids.TenantID
	provider ids.ProviderID
	role     contracts.Role
}

// served is the adapter currently serving a registration.
type served struct {
	port  contracts.Port
	store *ids.ProviderID
	stop  context.CancelFunc
	done  chan struct{}
}

var (
	wiresMu sync.Mutex
	wires   = map[testing.TB]*Wire{}
)

// For returns the test's wire, starting it on first use against pool. A
// test that reopens its pool to simulate a restart passes the new one, and
// bindings resolve through it from then on.
func For(t testing.TB, pool *pgxpool.Pool) *Wire {
	t.Helper()
	wiresMu.Lock()
	defer wiresMu.Unlock()
	if w := wires[t]; w != nil {
		w.pool.Store(pool)
		return w
	}
	w := start(t, pool, adapterwire.Options{})
	wires[t] = w
	t.Cleanup(func() {
		wiresMu.Lock()
		delete(wires, t)
		wiresMu.Unlock()
	})
	return w
}

// Start starts a wire with transport options, for tests of the transport
// itself.
func Start(t testing.TB, pool *pgxpool.Pool, options adapterwire.Options) *Wire {
	t.Helper()
	return start(t, pool, options)
}

func start(t testing.TB, pool *pgxpool.Pool, options adapterwire.Options) *Wire {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.Audience = Audience
	key := oidctest.NewRSAKey(t)
	issuer.Publish("k1", key)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	verifier, err := authn.NewVerifier(context.Background(),
		[]authn.IssuerConfig{{Issuer: issuer.URL(), Audience: Audience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 7200}},
		logger, authn.WithHTTPClient(issuer.Client()))
	if err != nil {
		t.Fatalf("wiretest: verifier: %v", err)
	}
	w := &Wire{issuer: issuer, key: key, logs: logs, served: map[servedKey]*served{}, stores: map[servedKey]ids.ProviderID{}}
	w.pool.Store(pool)
	resolve := func(ctx context.Context, issuer, subject string) (contracts.ProviderRegistration, error) {
		return adapterwire.StoreResolver(w.pool.Load())(ctx, issuer, subject)
	}
	registry, err := adapterwire.NewRegistry(resolve, options)
	if err != nil {
		t.Fatalf("wiretest: registry: %v", err)
	}
	endpoint, err := adapterwire.NewEndpoint(verifier, resolve, registry, logger)
	if err != nil {
		t.Fatalf("wiretest: endpoint: %v", err)
	}
	server := httptest.NewTLSServer(endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	w.Registry, w.server, w.ctx = registry, server, ctx
	t.Cleanup(func() {
		cancel()
		w.clients.Wait()
		registry.Close()
		server.Close()
	})
	return w
}

// Logs returns everything the endpoint, registry and verifier logged.
func (w *Wire) Logs() string {
	return w.logs.String()
}

// Issuer is the test issuer adapter tokens come from.
func (w *Wire) Issuer() *oidctest.Issuer {
	return w.issuer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// URL is the adapter endpoint.
func (w *Wire) URL() string {
	return strings.Replace(w.server.URL, "https://", "wss://", 1)
}

// TLS trusts the endpoint's test certificate.
func (w *Wire) TLS() *tls.Config {
	return w.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
}

// Token signs an adapter access token for subject.
func (w *Wire) Token(t testing.TB, subject string, lifetime time.Duration) string {
	t.Helper()
	return w.TokenFor(t, subject, Audience, lifetime)
}

// TokenFor signs an access token for subject with another audience.
func (w *Wire) TokenFor(t testing.TB, subject, audience string, lifetime time.Duration) string {
	t.Helper()
	now := time.Now()
	issuedAt := now.Add(-time.Second)
	if lifetime < 0 {
		issuedAt = now.Add(lifetime - time.Minute)
	}
	return w.issuer.Token(t, oidctest.Spec{Key: w.key, Claims: map[string]any{
		"sub": subject, "aud": audience, "iat": issuedAt.Unix(), "exp": now.Add(lifetime).Unix(),
	}})
}

// Bind registers provider in role and binds a fresh adapter identity to it,
// returning the identity's subject. A runtime may name its artifact store.
func (w *Wire) Bind(t testing.TB, tenant ids.TenantID, provider ids.ProviderID, role contracts.Role, artifactStore *ids.ProviderID) string {
	t.Helper()
	w.mu.Lock()
	w.next++
	subject := fmt.Sprintf("adapter-%d", w.next)
	w.mu.Unlock()
	ctx := context.Background()
	u, err := store.Begin(ctx, w.pool.Load())
	if err != nil {
		t.Fatalf("wiretest: begin: %v", err)
	}
	defer func() { _ = u.Rollback(ctx) }()
	now := time.Now().UTC()
	if err := store.PutProviderRegistration(ctx, u, contracts.ProviderRegistration{Tenant: tenant, Provider: provider, Role: role, ArtifactStore: artifactStore, UpdatedAt: now}); err != nil {
		t.Fatalf("wiretest: register provider: %v", err)
	}
	if err := store.PutAdapterBinding(ctx, u, contracts.AdapterBinding{Issuer: w.issuer.URL(), Subject: subject, Tenant: tenant, Provider: provider, Role: role, Enabled: true, UpdatedAt: now}); err != nil {
		t.Fatalf("wiretest: bind adapter: %v", err)
	}
	if err := u.Commit(ctx); err != nil {
		t.Fatalf("wiretest: commit: %v", err)
	}
	return subject
}

// Serve runs port as the adapter of provider in the port's role and waits
// until the registry holds its connection. Serving the same port again adds
// nothing unless the registration's artifact store changed, which needs a new
// connection to take effect; serving another port for the registration first
// disconnects the adapter serving it, as a test that swaps providers between
// passes expects.
func (w *Wire) Serve(t testing.TB, tenant ids.TenantID, provider ids.ProviderID, port contracts.Port) {
	t.Helper()
	key := servedKey{tenant, provider, port.Role()}
	w.mu.Lock()
	previous := w.served[key]
	var artifactStore *ids.ProviderID
	if store, ok := w.stores[key]; ok {
		artifactStore = &store
	}
	w.mu.Unlock()
	if previous != nil {
		if samePort(previous.port, port) && sameStore(previous.store, artifactStore) {
			return
		}
		previous.stop()
		<-previous.done
		w.waitDisconnected(t, key)
	}
	subject := w.Bind(t, tenant, provider, port.Role(), artifactStore)
	next := w.connect(t, subject, provider, port, 30*time.Minute)
	next.store = artifactStore
	w.mu.Lock()
	w.served[key] = next
	w.mu.Unlock()
	w.WaitConnected(t, tenant, provider, port.Role())
}

func samePort(a, b contracts.Port) bool {
	if reflect.TypeOf(a) != reflect.TypeOf(b) || !reflect.TypeOf(a).Comparable() {
		return false
	}
	return a == b
}

func sameStore(a, b *ids.ProviderID) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	default:
		return *a == *b
	}
}

func (w *Wire) waitDisconnected(t testing.TB, key servedKey) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for w.Registry.Connections(key.tenant, key.provider, key.role) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("wiretest: replaced %s adapter for provider %s never disconnected", key.role, key.provider)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Connect starts an adapter for subject serving port; it runs until the test
// ends.
func (w *Wire) Connect(t testing.TB, subject string, provider ids.ProviderID, port contracts.Port) {
	t.Helper()
	w.ConnectWithTokens(t, subject, provider, port, 30*time.Minute)
}

// ConnectWithTokens is Connect with tokens that live for lifetime, so the
// adapter renews its connection before each expiry.
func (w *Wire) ConnectWithTokens(t testing.TB, subject string, provider ids.ProviderID, port contracts.Port, lifetime time.Duration) {
	t.Helper()
	w.connect(t, subject, provider, port, lifetime)
}

func (w *Wire) connect(t testing.TB, subject string, provider ids.ProviderID, port contracts.Port, lifetime time.Duration) *served {
	t.Helper()
	config := adapterkit.Config{
		URL: w.URL(), Provider: provider.String(), Role: wire.Role(port.Role()), Handler: simulator.Handler(port),
		Token: func(context.Context) (adapterkit.Token, error) {
			return adapterkit.Token{Value: w.Token(t, subject, lifetime), Expiry: time.Now().Add(lifetime)}, nil
		},
		TLS: w.TLS(), BackoffBase: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
	}
	ctx, stop := context.WithCancel(w.ctx)
	next := &served{port: port, stop: stop, done: make(chan struct{})}
	w.clients.Go(func() {
		defer close(next.done)
		_ = adapterkit.Run(ctx, config)
	})
	return next
}

// WaitConnected waits until the registration has a live connection.
func (w *Wire) WaitConnected(t testing.TB, tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for w.Registry.Connections(tenant, provider, role) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("wiretest: %s adapter for provider %s never connected", role, provider)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Option adds ports in further roles to Ports.
type Option func(t testing.TB, w *Wire, tenant ids.TenantID)

// Environments serves environment ports by harness identity.
func Environments(ports map[ids.ProviderID]contracts.Port) Option {
	return func(t testing.TB, w *Wire, tenant ids.TenantID) {
		for provider, port := range ports {
			w.Serve(t, tenant, provider, port)
		}
	}
}

// Enforcement serves enforcement ports by provider identity.
func Enforcement(ports map[ids.ProviderID]contracts.Port) Option {
	return Environments(ports)
}

// ArtifactStores serves each runtime's artifact store and names it in the
// runtime's registration, which Ports then serves.
func ArtifactStores(stores map[ids.ProviderID]worker.ArtifactStorePort) Option {
	return func(t testing.TB, w *Wire, tenant ids.TenantID) {
		for producer, binding := range stores {
			w.Serve(t, tenant, binding.Provider, binding.Port)
			w.mu.Lock()
			w.stores[servedKey{tenant, producer, contracts.RoleRuntime}] = binding.Provider
			w.mu.Unlock()
		}
	}
}

// Ports serves every port in ports, in its own role, plus the options' ports
// as adapters of tenant over the wire, and returns the worker option that
// resolves them through the registry.
func Ports(t testing.TB, pool *pgxpool.Pool, tenant ids.TenantID, ports map[ids.ProviderID]contracts.Port, options ...Option) func(*worker.Worker) {
	t.Helper()
	w := For(t, pool)
	for _, option := range options {
		option(t, w, tenant)
	}
	for provider, port := range ports {
		w.Serve(t, tenant, provider, port)
	}
	return worker.WithPorts(w.Registry)
}
