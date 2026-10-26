// Package adapterkit connects a provider adapter to the dac-plane control
// plane (ADR-0008). The adapter dials the control plane over HTTPS, upgrades
// to one WebSocket carrying ControlFrames, authenticates with an OAuth 2.0
// client-credentials access token, and negotiates the protocol version before
// any work. The kit then hands each request, cancel and subscription to the
// adapter's Handler, keeps the connection alive with pings, and reconnects
// with exponential backoff and full jitter, renewing the connection before its
// token expires.
//
// The kit adds no outcome semantics. A request is answered only with the
// Handler's reply; a connection lost before the reply leaves the outcome for
// the control plane to reconcile from observations. A Handler must treat the
// operation identifier as its idempotency key. An adapter may advertise
// wire.CapabilityOperationReplay only when it can answer a replacement
// delivery from what it already did, across reconnects and process failure.
package adapterkit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
)

// Handler is the provider side of the protocol.
type Handler interface {
	// Send performs one delivery of a request and returns its reply. The
	// reply must name the request's operation and delivery. An error means
	// the outcome is unknown; the kit answers it as failed with unknown
	// durability, never as rejected.
	Send(ctx context.Context, request wire.Request) (wire.Reply, error)
	// Cancel asks the operation to stop. A stop is confirmed by observation,
	// not by Cancel returning.
	Cancel(ctx context.Context, cancel wire.Cancel) error
	// Subscribe opens the stream the cursor names, starting with connected.
	Subscribe(ctx context.Context, cursor wire.Cursor) (Stream, error)
}

// Stream yields stream frames until the context ends or the stream closes.
type Stream interface {
	Next(ctx context.Context) (wire.StreamFrame, error)
	Close() error
}

// Token is an access token and its expiry.
type Token struct {
	Value  string
	Expiry time.Time
}

// TokenSource returns a fresh client-credentials access token for each
// connection.
type TokenSource func(ctx context.Context) (Token, error)

// Config is how an adapter connects.
type Config struct {
	// URL is the control plane's adapter endpoint, https or wss.
	URL string
	// Token supplies the access token sent on each upgrade request.
	Token TokenSource
	// Provider and Role are the registration this adapter speaks for; the
	// control plane establishes them from the token's binding, and the kit
	// uses them to stamp events and refuse requests of another role.
	Provider string
	Role     wire.Role
	// Versions and Offered/Required capabilities are advertised in hello.
	// Versions defaults to the protocol version this kit speaks.
	Versions []uint32
	Offered  []string
	Required []string
	Handler  Handler
	// TLS configures the client; nil uses the system roots.
	TLS *tls.Config
	// Logger receives connection events; nil discards them.
	Logger *slog.Logger

	// PingInterval and PongTimeout keep the connection alive (15s and 10s).
	PingInterval time.Duration
	PongTimeout  time.Duration
	// RenewBefore is how long before token expiry a new connection is opened
	// (60s, at most half the token's remaining lifetime).
	RenewBefore time.Duration
	// MaxFrameBytes bounds a received frame (1 MiB).
	MaxFrameBytes int64
	// BackoffBase and BackoffMax bound reconnect delays (500ms and 30s).
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// HandshakeTimeout bounds dial and negotiation (10s).
	HandshakeTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if len(c.Versions) == 0 {
		c.Versions = []uint32{wire.ProtocolVersion}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	defaults := []struct {
		field *time.Duration
		value time.Duration
	}{
		{&c.PingInterval, 15 * time.Second}, {&c.PongTimeout, 10 * time.Second}, {&c.RenewBefore, time.Minute},
		{&c.BackoffBase, 500 * time.Millisecond}, {&c.BackoffMax, 30 * time.Second}, {&c.HandshakeTimeout, 10 * time.Second},
	}
	for _, d := range defaults {
		if *d.field <= 0 {
			*d.field = d.value
		}
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = wire.DefaultMaxFrameBytes
	}
	return c
}

func (c Config) validate() error {
	endpoint, err := url.Parse(c.URL)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "wss") || endpoint.Host == "" {
		return errors.New("adapterkit: URL must be an https or wss URL")
	}
	if c.Token == nil || c.Handler == nil {
		return errors.New("adapterkit: token source and handler are required")
	}
	if c.Provider == "" || len(wire.Methods(c.Role)) == 0 {
		return errors.New("adapterkit: provider and a known role are required")
	}
	hello := wire.Frame{Kind: wire.KindLifecycle, Lifecycle: &wire.Lifecycle{Phase: wire.PhaseHello, Versions: c.Versions, Offered: c.Offered, Required: c.Required}}
	if err := wire.Validate(hello); err != nil {
		return fmt.Errorf("adapterkit: hello: %w", err)
	}
	return nil
}

// Run connects and serves until ctx ends, reconnecting after every loss. It
// returns ctx's error, or a configuration error before connecting.
func Run(ctx context.Context, config Config) error {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config.TLS, ForceAttemptHTTP2: false}}
	backoff := newBackoff(config.BackoffBase, config.BackoffMax)
	renew := make(chan struct{}, 1)
	for {
		s, err := connect(ctx, config, client)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := backoff.next()
			config.Logger.Warn("adapter connection failed", "error", err, "retry_in", delay)
			if !sleep(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		backoff.reset()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.serve(ctx, renew)
		}()
		select {
		case <-ctx.Done():
			<-done
			return ctx.Err()
		case <-done:
			// The session ended; connect again after a short jittered delay
			// so a fleet of adapters does not reconnect in lockstep.
			if !sleep(ctx, backoff.next()) {
				return ctx.Err()
			}
		case <-renew:
			// The token is close to expiry: open the next connection now and
			// let this one drain until the control plane closes it.
			config.Logger.Info("renewing adapter connection before token expiry")
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
