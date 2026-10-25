package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// observeStream is the stream of a runtime turn's observations: progress,
// attention requests, decision consumption, results and stops.
const observeStream = "events"

// Observe follows the event streams of accepted runtime turns (ADR-0003,
// ADR-0010): it subscribes from each durable cursor and ingests every frame
// through application.Ingest, so the same binding, forged-envelope and
// cursor rules apply as to any provider event. Only operations whose runtime
// is connected to this replica are followed. Ingestion is idempotent, so
// several replicas following one stream apply each entry once.
func (w *Worker) Observe(ctx context.Context) error {
	tenants, err := store.ListTenants(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		operations, err := store.FollowedOperations(ctx, w.pool, tenant)
		if err != nil {
			return err
		}
		for _, operation := range operations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.observeOperation(ctx, tenant, operation); err != nil {
				w.logger.Error("operation observe failed", "operation", operation.ID.String(), "error", err)
			}
		}
	}
	return nil
}

// observeOperation reads one stream until it stays quiet for ObserveIdle or
// StreamTimeout passes; the next pass resumes from the durable cursor. A
// refused event ends the pass: its refusal is already recorded.
func (w *Worker) observeOperation(ctx context.Context, tenant ids.TenantID, operation lifecycle.ProviderOperation) error {
	port := w.port(tenant, operation.Provider, contracts.RoleRuntime)
	if port == nil {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}
	cursor, err := store.GetCursor(ctx, w.pool, tenant, operation.ID, observeStream)
	if errors.Is(err, store.ErrNotFound) {
		cursor = contracts.Cursor{Tenant: tenant, Attempt: operation.Attempt, Operation: operation.ID, Stream: observeStream, Generation: 1}
	} else if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, w.cfg.StreamTimeout)
	defer cancel()
	stream, err := port.Subscribe(sctx, cursor)
	if err != nil {
		return nil
	}
	defer func() {
		if stream != nil {
			_ = stream.Close()
		}
	}()
	peer := contracts.Peer{Tenant: tenant, Provider: operation.Provider}
	rehydrated := false
	for {
		fctx, fcancel := context.WithTimeout(sctx, w.cfg.ObserveIdle)
		frame, err := stream.Next(fctx)
		fcancel()
		if err != nil {
			return nil
		}
		event := contracts.Event{Tenant: tenant, Attempt: operation.Attempt, Provider: operation.Provider, Operation: operation.ID, Stream: cursor.Stream, Generation: cursor.Generation, Body: frame}
		ingested, err := application.Ingest(sctx, w.pool, peer, event, time.Now())
		if err != nil {
			var failure *contracts.Failure
			if errors.As(err, &failure) {
				w.logger.Warn("provider event refused", "operation", operation.ID.String(), "code", string(failure.Code))
				return nil
			}
			return err
		}
		if ingested.Stale {
			return nil
		}
		if ingested.Cursor.Generation != 0 {
			cursor = ingested.Cursor
		}
		if ingested.Rehydrate {
			if rehydrated {
				return nil // One reset per pass, never a busy loop.
			}
			// Replay from the start of the generation: entries at or below
			// the durable cursor are skipped as duplicates on ingestion.
			rehydrated = true
			_ = stream.Close()
			cursor.After = 0
			if stream, err = port.Subscribe(sctx, cursor); err != nil {
				stream = nil
				return nil
			}
		}
	}
}
