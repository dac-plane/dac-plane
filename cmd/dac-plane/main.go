package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/bootstrap"
	"github.com/AssahBismarkabah/dac-plane/internal/config"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/provision"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "provision" {
		os.Exit(runProvision(os.Args[2:]))
	}
	os.Exit(run())
}

// runProvision performs one trusted provisioning operation (ADR-0007). The
// database URL is read from the same configuration as the control plane.
func runProvision(args []string) int {
	processContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	open := func(ctx context.Context) (*provision.Provisioner, func(), error) {
		processConfig, err := config.Load(os.LookupEnv)
		if err != nil {
			return nil, nil, err
		}
		connectContext, cancel := context.WithTimeout(ctx, processConfig.DatabaseConnectTimeout)
		defer cancel()
		pool, err := store.Open(connectContext, processConfig.DatabaseURL)
		if err != nil {
			return nil, nil, err
		}
		provisioner, err := provision.New(pool, time.Now)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		return provisioner, pool.Close, nil
	}
	return provision.Run(processContext, args, os.Stdout, os.Stderr, open)
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	processContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	processConfig, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}

	application, err := bootstrap.New(processContext, processConfig, logger)
	if err != nil {
		logger.Error("control plane startup failed", "error", err)
		return 1
	}
	defer application.Close()

	if err := application.Run(processContext); err != nil {
		logger.Error("control plane stopped with an error", "error", err)
		return 1
	}
	return 0
}
