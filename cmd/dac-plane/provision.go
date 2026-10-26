package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/provision"
)

// runProvision performs one trusted provisioning operation (ADR-0007). The
// database URL is read from the same configuration as the control plane.
func runProvision(args []string, stdout, stderr io.Writer) int {
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
	return provision.Run(processContext, args, stdout, stderr, open)
}
