// Package adapterwire is the control plane's side of the adapter wire
// transport (ADR-0008, ADR-0010). Adapters dial the Endpoint over HTTPS and
// upgrade to one WebSocket carrying ControlFrames. The endpoint verifies the
// adapter's client-credentials token, resolves its stored binding to the one
// provider registration it speaks for, negotiates the protocol before any
// work, and registers the connection. The Registry hands the worker a
// contracts.Port per provider registration backed by its live connections.
//
// The transport adds no outcome semantics. A request whose connection is lost
// before its reply returns an error, and the worker reconciles it from
// observations; only a request no connection could carry is ErrNotDelivered.
package adapterwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Subprotocol names the ControlFrame framing in the WebSocket handshake.
const Subprotocol = "dac-plane.controlframe.v1"

// Verifier verifies an adapter's access token; *authn.Verifier built from the
// adapter issuers is the production implementation.
type Verifier interface {
	Verify(ctx context.Context, token string) (authn.Verified, error)
}

// Resolver returns the registration an enabled binding of (issuer, subject)
// speaks for, or store.ErrNotFound.
type Resolver func(ctx context.Context, issuer, subject string) (contracts.ProviderRegistration, error)

// Options tune the transport. Zero values take the defaults.
type Options struct {
	// MaxFrameBytes bounds one received frame (1 MiB).
	MaxFrameBytes int64
	// PingInterval and PongTimeout keep a connection alive (15s and 10s); the
	// binding is re-resolved on every ping tick.
	PingInterval time.Duration
	PongTimeout  time.Duration
	// Drain is how long before token expiry a connection stops taking new
	// requests (30s, at most half the token's remaining lifetime).
	Drain time.Duration
	// MaxInFlight bounds the requests awaiting replies on one connection (64).
	MaxInFlight int
	// HandshakeTimeout bounds negotiation after the upgrade (10s).
	HandshakeTimeout time.Duration
	// StreamBuffer bounds the frames buffered for one subscription (256); a
	// subscriber that falls further behind loses its stream and resumes from
	// its durable cursor.
	StreamBuffer int
	// Hello is the control plane's side of negotiation (version 1).
	Hello contracts.Lifecycle
	// Now is the clock (time.Now).
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	durations := []struct {
		field *time.Duration
		value time.Duration
	}{
		{&o.PingInterval, 15 * time.Second}, {&o.PongTimeout, 10 * time.Second},
		{&o.Drain, 30 * time.Second}, {&o.HandshakeTimeout, 10 * time.Second},
	}
	for _, d := range durations {
		if *d.field <= 0 {
			*d.field = d.value
		}
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = 1 << 20
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = 64
	}
	if o.StreamBuffer <= 0 {
		o.StreamBuffer = 256
	}
	if o.Hello.Phase == "" {
		o.Hello = contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{1}}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Endpoint accepts adapter connections. It is an http.Handler that a TLS
// listener serves; it never serves plaintext itself.
type Endpoint struct {
	verifier Verifier
	resolve  Resolver
	registry *Registry
	logger   *slog.Logger
}

// NewEndpoint returns an endpoint that registers connections in registry.
func NewEndpoint(verifier Verifier, resolve Resolver, registry *Registry, logger *slog.Logger) (*Endpoint, error) {
	if verifier == nil || resolve == nil || registry == nil || logger == nil {
		return nil, errors.New("adapterwire: verifier, resolver, registry and logger are required")
	}
	return &Endpoint{verifier: verifier, resolve: resolve, registry: registry, logger: logger}, nil
}

// ServeHTTP authenticates before upgrading, so an unbound identity never
// reaches negotiation, then serves the connection until it closes.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := authn.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		e.logger.Warn("adapter connection refused", "reason", string(authn.ReasonMissing))
		unauthorized(w)
		return
	}
	verified, err := e.verifier.Verify(r.Context(), token)
	if err != nil {
		unauthorized(w)
		return
	}
	registration, err := e.resolve(r.Context(), verified.Issuer, verified.Subject)
	if errors.Is(err, store.ErrNotFound) {
		e.logger.Warn("adapter connection refused", "reason", string(authn.ReasonUnbound), "issuer", verified.Issuer)
		unauthorized(w)
		return
	}
	if err != nil {
		e.logger.Error("adapter binding could not be resolved", "error", err)
		http.Error(w, "adapter binding unavailable", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		e.logger.Warn("adapter upgrade failed", "provider", registration.Provider.String(), "error", err)
		return
	}
	if conn.Subprotocol() != Subprotocol {
		e.logger.Warn("adapter connection refused", "reason", "subprotocol", "provider", registration.Provider.String())
		_ = conn.Close(websocket.StatusPolicyViolation, "the "+Subprotocol+" subprotocol is required")
		return
	}
	c := newConnection(e.registry, conn, registration, verified, e.logger)
	c.serve()
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dac-plane adapters"`)
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}

// StoreResolver resolves bindings from the database on every call, so a
// disabled binding refuses the next connection and closes live ones at their
// next binding check.
func StoreResolver(q store.Querier) Resolver {
	return func(ctx context.Context, issuer, subject string) (contracts.ProviderRegistration, error) {
		registration, err := store.ResolveAdapterBinding(ctx, q, issuer, subject)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return contracts.ProviderRegistration{}, fmt.Errorf("resolve adapter binding: %w", err)
		}
		return registration, err
	}
}
