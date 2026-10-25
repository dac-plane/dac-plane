package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AssahBismarkabah/dac-plane/internal/config"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
)

const migrateUsage = `usage: dac-plane migrate up | status | down-to VERSION

  up        apply every pending migration
  status    print the database and release schema versions
  down-to   revert to VERSION; refused where it would discard evidence

Runs hold a PostgreSQL advisory lock, so a concurrent run waits.`

// runMigrate is the only path that changes the schema (ADR-0009). It runs as
// a one-shot step before serving processes of a new release start.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	var target int64
	switch {
	case len(args) == 1 && (args[0] == "up" || args[0] == "status"):
	case len(args) == 2 && args[0] == "down-to":
		version, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || version < 0 || version >= migrations.CurrentVersion {
			_, _ = fmt.Fprintf(stderr, "dac-plane migrate: VERSION must be 0 to %d\n\n%s\n", migrations.CurrentVersion-1, migrateUsage)
			return 2
		}
		target = version
	default:
		_, _ = fmt.Fprintf(stderr, "dac-plane migrate: invalid arguments\n\n%s\n", migrateUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := migrate(ctx, args[0], target, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane migrate: %v\n", err)
		return 1
	}
	return 0
}

func migrate(ctx context.Context, command string, target int64, stdout io.Writer) error {
	processConfig, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", processConfig.DatabaseURL)
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL configuration")
	}
	defer func() { _ = db.Close() }()
	connectContext, cancel := context.WithTimeout(ctx, processConfig.DatabaseConnectTimeout)
	defer cancel()
	if err := db.PingContext(connectContext); err != nil {
		return fmt.Errorf("connect to PostgreSQL")
	}
	provider, err := migrations.NewLockedProvider(db, migrations.Source)
	if err != nil {
		return err
	}
	switch command {
	case "up":
		results, err := provider.Up(ctx)
		for _, result := range results {
			_, _ = fmt.Fprintf(stdout, "applied %d in %s\n", result.Source.Version, result.Duration)
		}
		if err != nil {
			return err
		}
	case "down-to":
		results, err := provider.DownTo(ctx, target)
		for _, result := range results {
			_, _ = fmt.Fprintf(stdout, "reverted %d in %s\n", result.Source.Version, result.Duration)
		}
		if err != nil {
			return err
		}
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "database at %d; this release serves %d to %d\n", version, migrations.MinServeVersion, migrations.CurrentVersion)
	return nil
}
