// Package bootstrap composes and owns the initial control-plane process.
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Application owns process-scoped resources for the modular monolith.
type Application struct {
	logger    *slog.Logger
	pool      *pgxpool.Pool
	closeOnce sync.Once
}

// New validates PostgreSQL connectivity and constructs the application.
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

	return &Application{logger: logger, pool: pool}, nil
}

// Run keeps the application alive until cancellation. Lifecycle behavior is
// added behind this composition root by later issues.
func (application *Application) Run(ctx context.Context) error {
	application.logger.Info("control plane initialized")
	<-ctx.Done()
	application.logger.Info("control plane stopping")
	return nil
}

// Close releases process-scoped resources. It is safe to call more than once.
func (application *Application) Close() {
	application.closeOnce.Do(func() {
		application.pool.Close()
	})
}
