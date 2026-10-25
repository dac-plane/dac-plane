package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/AssahBismarkabah/dac-plane/internal/bootstrap"
	"github.com/AssahBismarkabah/dac-plane/internal/config"
)

// runServe runs a serving process until SIGINT or SIGTERM. It refuses to
// start outside the release's schema range and never migrates.
func runServe() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	processContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	processConfig, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}
	if err := processConfig.ValidateServe(); err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}

	application, err := bootstrap.New(processContext, processConfig, logger)
	if err != nil {
		logger.Error("control plane startup failed", "error", err)
		return 1
	}
	defer application.Close()

	if err := application.Serve(processContext, processConfig); err != nil {
		logger.Error("control plane stopped with an error", "error", err)
		return 1
	}
	return 0
}
