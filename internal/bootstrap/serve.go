package bootstrap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/adapterwire"
	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/config"
	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 15 * time.Second
	readinessTimeout  = 2 * time.Second
)

// CheckSchema refuses a database outside the schema range this release
// serves (ADR-0009). A serving process never migrates.
func (application *Application) CheckSchema(ctx context.Context) error {
	version, err := store.SchemaVersion(ctx, application.pool)
	if err != nil {
		return errors.New("cannot read the database schema version")
	}
	return migrations.CheckServable(version)
}

// ServeOption adjusts how Serve reaches its dependencies.
type ServeOption func(*serveOptions)

type serveOptions struct {
	issuerClient *http.Client
}

// WithIssuerClient sets the HTTP client used for issuer discovery and keys.
// By default the system roots apply, which a container extends through
// SSL_CERT_FILE.
func WithIssuerClient(client *http.Client) ServeOption {
	return func(o *serveOptions) { o.issuerClient = client }
}

// Serve runs every role of a serving process until ctx ends: the Surface
// API and the adapter endpoint on their TLS listeners, the operations
// listener, and the worker (ADR-0009). It returns once in-flight requests
// drained and the worker stopped.
func (application *Application) Serve(ctx context.Context, cfg config.Config, opts ...ServeOption) error {
	var settings serveOptions
	for _, opt := range opts {
		opt(&settings)
	}
	var issuerOptions []authn.Option
	if settings.issuerClient != nil {
		issuerOptions = append(issuerOptions, authn.WithHTTPClient(settings.issuerClient))
	}
	if err := cfg.ValidateServe(); err != nil {
		return err
	}
	if err := application.CheckSchema(ctx); err != nil {
		return err
	}
	identityConfig, err := authn.LoadConfig(cfg.IdentityConfigFile)
	if err != nil {
		return err
	}
	if identityConfig.Development() && cfg.Deployment != config.DeploymentDevelopment {
		return errors.New("the identity configuration trusts a development issuer; only DAC_DEPLOYMENT=development may serve it")
	}
	logger := application.logger
	authenticator, err := authn.New(ctx, identityConfig, authn.StoreResolver(application.pool), logger.With("component", "authn"), issuerOptions...)
	if err != nil {
		return err
	}
	verifier, err := authn.NewVerifier(ctx, identityConfig.AdapterIssuers, logger.With("component", "adapter-authn"), issuerOptions...)
	if err != nil {
		return err
	}
	api, err := httpapi.New(application.pool, authenticator, logger.With("component", "surface"), httpapi.Options{})
	if err != nil {
		return err
	}
	endpoint, err := adapterwire.NewEndpoint(verifier, adapterwire.StoreResolver(application.pool), application.adapters, logger.With("component", "adapters"))
	if err != nil {
		return err
	}
	certificate, err := loadCertificate(cfg.TLSCertFile, cfg.TLSKeyFile, logger)
	if err != nil {
		return err
	}

	var serving atomic.Bool
	servers := []*listener{
		{name: "surface", addr: cfg.SurfaceAddr, server: newServer(api, logger), tls: certificate.serverTLS("h2", "http/1.1")},
		{name: "adapter", addr: cfg.AdapterAddr, server: newServer(endpoint, logger), tls: certificate.serverTLS("http/1.1")},
		{name: "operations", addr: cfg.OpsAddr, server: newServer(application.operations(&serving), logger)},
	}
	if !cfg.OpsLoopback() {
		// Off loopback the operations listener serves TLS too, so nothing
		// is plaintext off loopback (ADR-0009).
		servers[2].tls = certificate.serverTLS("http/1.1")
	}
	for _, l := range servers {
		if err := l.listen(); err != nil {
			for _, opened := range servers {
				opened.close()
			}
			return err
		}
	}

	// The worker stops with ctx, or when a listener fails while ctx is live.
	runContext, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	go func() { workerDone <- application.Run(runContext) }()
	serveErrors := make(chan error, len(servers))
	for _, l := range servers {
		go func() { serveErrors <- l.serve() }()
	}
	serving.Store(true)
	logger.Info("control plane serving", "surface", cfg.SurfaceAddr, "adapter", cfg.AdapterAddr, "operations", cfg.OpsAddr)

	var failure error
	select {
	case <-ctx.Done():
	case failure = <-serveErrors:
	}
	serving.Store(false)
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, l := range servers {
		if err := l.server.Shutdown(shutdownContext); err != nil {
			_ = l.server.Close()
		}
	}
	application.adapters.Close()
	stopWorker()
	return errors.Join(failure, <-workerDone)
}

type listener struct {
	name     string
	addr     string
	server   *http.Server
	tls      *tls.Config
	listener net.Listener
}

func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

func (l *listener) listen() error {
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return fmt.Errorf("%s listener: %w", l.name, err)
	}
	l.listener = ln
	return nil
}

func (l *listener) serve() error {
	var err error
	if l.tls != nil {
		l.server.TLSConfig = l.tls
		err = l.server.ServeTLS(l.listener, "", "")
	} else {
		err = l.server.Serve(l.listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("%s listener: %w", l.name, err)
}

func (l *listener) close() {
	if l.listener != nil {
		_ = l.listener.Close()
	}
}

// operations serves liveness and readiness. Bodies carry status words only,
// never records, identities or configuration.
func (application *Application) operations(serving *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		if !application.running.Load() {
			status(w, http.StatusServiceUnavailable, "stopped")
			return
		}
		status(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		switch {
		case !application.running.Load() || !serving.Load():
			status(w, http.StatusServiceUnavailable, "not serving")
		case application.pool.Ping(ctx) != nil:
			status(w, http.StatusServiceUnavailable, "database unavailable")
		case application.CheckSchema(ctx) != nil:
			status(w, http.StatusServiceUnavailable, "schema out of range")
		default:
			status(w, http.StatusOK, "ready")
		}
	})
	return mux
}

func status(w http.ResponseWriter, code int, word string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, word)
}
