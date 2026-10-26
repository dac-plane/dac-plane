// Package bootstrap composes and owns the initial control-plane process.
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/AssahBismarkabah/dac-plane/internal/adapterwire"
	"github.com/AssahBismarkabah/dac-plane/internal/config"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Application owns process-scoped resources for the modular monolith.
type Application struct {
	logger   *slog.Logger
	pool     *pgxpool.Pool
	adapters *adapterwire.Registry
	worker   *worker.Worker
	owner    string
	// running is true while Run drives the process loop; liveness reports it.
	running   atomic.Bool
	closeOnce sync.Once
}

// New validates PostgreSQL connectivity and constructs the application. The
// worker resolves provider ports through the adapter registry: a provider's
// rows wait until its adapter connects to this replica (ADR-0008); the reaper
// and reconciler run from the start.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Application, error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}

	connectContext, cancelConnect := context.WithTimeout(ctx, cfg.DatabaseConnectTimeout)
	defer cancelConnect()

	pool, err := store.Open(connectContext, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	ownerID, err := ids.New[ids.Any]()
	if err != nil {
		pool.Close()
		return nil, err
	}
	adapters, err := adapterwire.NewRegistry(adapterwire.StoreResolver(pool), adapterwire.Options{})
	if err != nil {
		pool.Close()
		return nil, err
	}
	dispatchWorker, err := worker.New(pool, logger.With("component", "worker"), nil, worker.DefaultConfig(), "control-plane-"+ownerID.String(), worker.WithPorts(adapters))
	if err != nil {
		adapters.Close()
		pool.Close()
		return nil, err
	}

	return &Application{logger: logger, pool: pool, adapters: adapters, worker: dispatchWorker, owner: "control-plane-" + ownerID.String()}, nil
}

// Adapters is the registry the adapter endpoint registers connections in.
func (application *Application) Adapters() *adapterwire.Registry {
	return application.adapters
}

// Run keeps the application alive until cancellation, driving fenced
// dispatch, reconciliation and claim takeover behind this composition root.
// It waits for in-flight work before returning so Close never races a query.
// A nil worker runs nothing; tests and future surfaces use that to skip
// dispatch without a pool.
func (application *Application) Run(ctx context.Context) error {
	application.logger.Info("control plane initialized", "worker_owner", application.owner)
	application.running.Store(true)
	defer application.running.Store(false)
	if application.worker != nil {
		workerDone := make(chan error, 1)
		go func() {
			workerDone <- application.worker.Run(ctx)
		}()
		<-ctx.Done()
		if err := <-workerDone; err != nil {
			application.logger.Error("worker stopped with an error", "error", err)
			return err
		}
	} else {
		<-ctx.Done()
	}
	application.logger.Info("control plane stopping")
	return nil
}

// Close releases process-scoped resources. It is safe to call more than once.
func (application *Application) Close() {
	application.closeOnce.Do(func() {
		if application.adapters != nil {
			application.adapters.Close()
		}
		application.pool.Close()
	})
}
