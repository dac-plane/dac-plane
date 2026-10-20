package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// errInvalidConfig reports a worker that cannot drive a loop.
var errInvalidConfig = errors.New("invalid worker config")

// Config tunes the dispatch loop. Intervals come from SPEC §17 cadences
// (heartbeat 15s, suspect 60s, lease expiry 90s): the poll stays well under
// the suspect threshold and the claim TTL matches a missed-heartbeat window.
type Config struct {
	// PollInterval paces dispatch ticks; ReconcileInterval paces uncertain
	// reconciliation; ReaperInterval paces claim takeover.
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	ReaperInterval    time.Duration
	StreamTimeout     time.Duration
	SendTimeout       time.Duration
	// BatchLimit caps outbox rows read per tenant per tick.
	BatchLimit int
	// ClaimTTL is the outbox and attempt lease window a tick holds.
	ClaimTTL time.Duration
	// Policy bounds dispatch retries; BackgroundBudget caps background rows
	// per dispatch batch so person-facing work is never starved.
	Policy           ExhaustionPolicy
	BackgroundBudget int
}

// DefaultConfig is the starting point; tune by replacing fields, not by
// adding environment variables (process config stays env-only in config).
func DefaultConfig() Config {
	return Config{
		PollInterval:      5 * time.Second,
		ReconcileInterval: 15 * time.Second,
		ReaperInterval:    30 * time.Second,
		StreamTimeout:     10 * time.Second,
		SendTimeout:       30 * time.Second,
		BatchLimit:        100,
		ClaimTTL:          time.Minute,
		Policy:            DefaultExhaustionPolicy(),
		BackgroundBudget:  10,
	}
}

// Validate reports whether the config can drive a loop.
func (c Config) Validate() error {
	if c.PollInterval <= 0 || c.ReconcileInterval <= 0 || c.ReaperInterval <= 0 {
		return fmt.Errorf("worker intervals must be positive: %w", errInvalidConfig)
	}
	if c.StreamTimeout <= 0 || c.SendTimeout <= 0 || c.ClaimTTL <= 0 {
		return fmt.Errorf("worker timeouts must be positive: %w", errInvalidConfig)
	}
	if c.BatchLimit <= 0 || c.BackgroundBudget < 0 {
		return fmt.Errorf("worker batch limits must be non-negative: %w", errInvalidConfig)
	}
	return c.Policy.Validate()
}

// Stats counts loop outcomes. Every counter is monotonic within a process.
type Stats struct {
	Claims        atomic.Int64
	Acks          atomic.Int64
	Retries       atomic.Int64
	Exhausts      atomic.Int64
	Poison        atomic.Int64
	SkippedNoPort atomic.Int64
	Reconciled    atomic.Int64
	Reaped        atomic.Int64
}

// Worker drives fenced dispatch: it polls ready outbox rows, sends them
// through registered provider ports, reconciles uncertain outcomes from
// bound observations, and reaps lapsed maintenance claims. Providers with
// no registered port keep their rows pending; the worker never invents
// operations, budgets or authority.
type Worker struct {
	pool         *pgxpool.Pool
	logger       *slog.Logger
	ports        map[ids.ProviderID]contracts.Port
	environments map[ids.ProviderID]contracts.Port
	enforcement  map[ids.ProviderID]contracts.Port
	artifacts    map[ids.ProviderID]ArtifactStorePort
	owner        string
	cfg          Config
	stats        Stats
}

// New builds a worker. ports maps provider identities to their ports and
// may be empty; owner identifies this worker in claim leases.
func New(pool *pgxpool.Pool, logger *slog.Logger, ports map[ids.ProviderID]contracts.Port, cfg Config, owner string, options ...func(*Worker)) (*Worker, error) {
	if pool == nil {
		return nil, fmt.Errorf("worker requires a pool: %w", errInvalidConfig)
	}
	if logger == nil {
		return nil, fmt.Errorf("worker requires a logger: %w", errInvalidConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("worker requires an owner identity: %w", errInvalidConfig)
	}
	registry := make(map[ids.ProviderID]contracts.Port, len(ports))
	for provider, port := range ports {
		registry[provider] = port
	}
	w := &Worker{pool: pool, logger: logger, ports: registry, owner: owner, cfg: cfg}
	for _, option := range options {
		option(w)
	}
	return w, nil
}

// SystemActor attributes worker-driven transitions to the control plane.
func SystemActor() lifecycle.Actor {
	return lifecycle.Actor{Kind: lifecycle.ActorSystem}
}

// Run drives the dispatch, reconcile and reaper loops until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(3)
	go w.loop(ctx, &wg, w.cfg.PollInterval, "dispatch", func(ctx context.Context) error { return w.Tick(ctx) })
	go w.loop(ctx, &wg, w.cfg.ReconcileInterval, "reconcile", func(ctx context.Context) error { return w.Reconcile(ctx) })
	go w.loop(ctx, &wg, w.cfg.ReaperInterval, "reaper", func(ctx context.Context) error { return w.ReapClaims(ctx) })
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (w *Worker) loop(ctx context.Context, wg *sync.WaitGroup, interval time.Duration, name string, pass func(context.Context) error) {
	defer wg.Done()
	if err := pass(ctx); err != nil {
		w.logger.Error("worker pass failed", "loop", name, "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pass(ctx); err != nil {
				w.logger.Error("worker pass failed", "loop", name, "error", err)
			}
		}
	}
}

// Tick runs one dispatch pass over every tenant: read the ready batch,
// order it with the scheduler, and handle each row through an atomic claim.
func (w *Worker) Tick(ctx context.Context) error {
	tenants, err := store.ListTenants(ctx, w.pool)
	if err != nil {
		return err
	}
	var firstErr error
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.tickTenant(ctx, tenant); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.logger.Error("dispatch tick failed", "tenant", tenant.String(), "error", err)
		}
	}
	return firstErr
}

func (w *Worker) tickTenant(ctx context.Context, tenant ids.TenantID) error {
	now := time.Now().UTC()
	ready, err := store.OutboxReadyDispatch(ctx, w.pool, tenant, now, w.cfg.BatchLimit)
	if err != nil {
		return err
	}
	order := Scheduler{BackgroundBudget: w.cfg.BackgroundBudget}.Select(ready)
	for _, row := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.handleRow(ctx, tenant, row, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}
