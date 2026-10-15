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

func main() {
	os.Exit(run())
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
