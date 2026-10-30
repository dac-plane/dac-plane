package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

const reconcileStream = "dispatch"

// Reconcile inspects uncertain effects and never dispatches them again. It also
// repeats interrupts, which are requests rather than effects.
func (w *Worker) Reconcile(ctx context.Context) error {
	tenants, err := store.ListTenants(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return err
		}
		operations, err := store.UncertainOperations(ctx, w.pool, tenant)
		if err != nil {
			return err
		}
		for _, op := range operations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.reconcileOperation(ctx, tenant, op); err != nil {
				w.logger.Error("operation reconcile failed", "operation", op.ID.String(), "error", err)
			}
		}
		if err := w.repeatInterrupts(ctx, tenant); err != nil {
			w.logger.Error("interrupt repeat failed", "tenant", tenant.String(), "error", err)
		}
	}
	return nil
}

// repeatInterrupts asks again for every runtime stop still unobserved. An
// interrupt is acknowledged when its frame is written, so an adapter that lost
// it would otherwise leave the Attempt waiting forever. It is a request to stop
// rather than an effect, so repeating it is safe; a failed repeat waits for the
// next pass.
func (w *Worker) repeatInterrupts(ctx context.Context, tenant ids.TenantID) error {
	operations, err := store.InterruptedOperations(ctx, w.pool, tenant, w.cfg.BatchLimit)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		port := w.port(tenant, operation.Provider, contracts.RoleRuntime)
		if port == nil {
			w.stats.SkippedNoPort.Add(1)
			continue
		}
		delivery, err := ids.New[ids.Delivery]()
		if err != nil {
			return err
		}
		sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
		err = port.Cancel(sendCtx, contracts.Cancel{Operation: operation.ID, Delivery: delivery})
		cancel()
		if err != nil {
			w.logger.Warn("interrupt repeat not delivered", "operation", operation.ID.String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) reconcileOperation(ctx context.Context, tenant ids.TenantID, operation lifecycle.ProviderOperation) error {
	if yes, err := store.IsArtifactOperation(ctx, w.pool, tenant, operation.ID); err != nil {
		return err
	} else if yes {
		return nil
	}
	if action, err := store.GetGovernedAction(ctx, w.pool, tenant, operation.ID); err == nil {
		return w.reconcileStreamOperation(ctx, tenant, operation, &action)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return w.reconcileStreamOperation(ctx, tenant, operation, nil)
}

func (w *Worker) reconcileStreamOperation(ctx context.Context, tenant ids.TenantID, operation lifecycle.ProviderOperation, action *store.GovernedAction) error {
	// A governed action is executed and observed by the enforcement role.
	role := contracts.RoleRuntime
	if action != nil {
		role = contracts.RoleEnforcement
	} else if row, err := store.GetOutboxRow(ctx, w.pool, tenant, operation.ID.Untyped()); err == nil {
		if body, parseErr := ParseDispatchBody(row.Payload); parseErr == nil && body.Role != "" {
			role = body.Role
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	port := w.port(tenant, operation.Provider, role)
	if port == nil {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}
	cursor, err := store.GetCursor(ctx, w.pool, tenant, operation.ID, reconcileStream)
	if errors.Is(err, store.ErrNotFound) {
		cursor = contracts.Cursor{Tenant: tenant, Attempt: operation.Attempt, Operation: operation.ID, Stream: reconcileStream, Generation: 1}
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
	// Replay may start below the durable high-water mark after cursor loss.
	// Keep that watermark separate and reject a concurrent change to it.
	var replayWatermark *contracts.Cursor
	for {
		frame, err := stream.Next(sctx)
		if err != nil {
			return nil
		} // The next pass resumes from durable progress.
		ingested, settled, err := w.reconcileFrame(sctx, operation, role, cursor, frame, replayWatermark, action)
		if err != nil {
			return err
		}
		if settled {
			return nil
		}
		if ingested.Stale {
			// This subscription still belongs to its original generation.
			// Do not relabel subsequent old frames with the newer stored cursor.
			return nil
		}
		cursor = ingested.Cursor
		if ingested.Rehydrate {
			if replayWatermark != nil {
				return nil
			} // One reset per pass, never a busy loop.
			watermark := cursor
			replayWatermark = &watermark
			cursor.After = 0
			_ = stream.Close()
			stream = nil
			stream, err = port.Subscribe(sctx, cursor)
			if err != nil {
				return nil
			}
		}
	}
}

// reconcileFrame validates and applies one observation under the durable
// scope/cursor locks. Outcomes come only from accepted entries, never the raw
// frame. Cursor and lifecycle movement share one transaction.
func (w *Worker) reconcileFrame(ctx context.Context, operation lifecycle.ProviderOperation, role contracts.Role, cursor contracts.Cursor, frame contracts.StreamFrame, replayWatermark *contracts.Cursor, action *store.GovernedAction) (contracts.Ingestion, bool, error) {
	event := contracts.Event{Tenant: operation.Tenant, Attempt: operation.Attempt, Provider: operation.Provider, Operation: operation.ID, Stream: cursor.Stream, Generation: cursor.Generation, Body: frame}
	bound, failure := contracts.BindEvent(contracts.Peer{Tenant: operation.Tenant, Provider: operation.Provider}, event, operation)
	if failure != nil {
		return contracts.Ingestion{}, false, failure
	}
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return contracts.Ingestion{}, false, err
	}
	defer func() { _ = u.Rollback(context.WithoutCancel(ctx)) }()
	if action != nil {
		if err = store.LockGovernedAction(ctx, u, operation.Tenant, operation.ID); err != nil {
			return contracts.Ingestion{}, false, err
		}
	}
	if _, _, err = store.LockResultScope(ctx, u, operation.Tenant, operation.Attempt); err != nil {
		return contracts.Ingestion{}, false, err
	}
	// An environment has no harness: its registration authorizes it, as in
	// ingestion.
	var authoritative bool
	if role == contracts.RoleEnvironment {
		_, err = store.GetProviderRegistration(ctx, u, operation.Tenant, operation.Provider, contracts.RoleEnvironment)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return contracts.Ingestion{}, false, err
		}
		authoritative, err = err == nil, nil
	} else {
		authoritative, err = store.ObserveHarness(ctx, u, bound, time.Now().UTC())
	}
	if err != nil {
		return contracts.Ingestion{}, false, err
	}
	if !authoritative {
		ingested := contracts.Ingestion{Cursor: cursor}
		// ObserveHarness holds qualification locked. Shadow replay uses only
		// the subscription's local cursor: no primary cursor is created or
		// advanced, and replayed outcomes remain comparison evidence only.
		h, err := store.GetHarness(ctx, u, operation.Tenant, operation.Provider)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return contracts.Ingestion{}, false, err
		}
		if err == nil && h.Mode == lifecycle.HarnessShadow {
			ingested, failure = contracts.Ingest(cursor, bound)
			if failure != nil {
				return contracts.Ingestion{}, false, failure
			}
		}
		return ingested, false, u.Commit(ctx)
	}
	current, err := store.GetOperation(ctx, u, operation.Tenant, operation.ID)
	if err != nil {
		return contracts.Ingestion{}, false, err
	}
	stored, err := store.LockCursor(ctx, u, cursor)
	if err != nil {
		return contracts.Ingestion{}, false, err
	}
	base := stored
	if replayWatermark != nil {
		if stored != *replayWatermark || current.Version != operation.Version {
			return contracts.Ingestion{}, false, store.ErrStaleVersion
		}
		base = cursor
	}
	ingested, failure := contracts.Ingest(base, bound)
	if failure != nil {
		return contracts.Ingestion{}, false, failure
	}
	settled := false
	for _, entry := range ingested.Entries {
		// Usage belongs to the runtime, not a governed action or an environment.
		// Account every accepted total, including entries after the settling
		// outcome, before advancing the cursor past this frame.
		if action == nil && role == contracts.RoleRuntime && entry.Kind == contracts.EntryUsageReported && !entry.PayloadOmitted {
			step := progress.Step{UOW: u, Tenant: current.Tenant, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: current.Provider}}
			_, refusal, err := step.Usage(ctx, current.Attempt, entry)
			if err != nil {
				return contracts.Ingestion{}, false, err
			}
			if refusal != nil {
				auditID, err := ids.New[ids.Audit]()
				if err != nil {
					return contracts.Ingestion{}, false, err
				}
				if err = store.RecordRefusal(ctx, u, refusal.Audit(auditID, step.Actor, step.At)); err != nil {
					return contracts.Ingestion{}, false, err
				}
			}
			continue
		}
		if settled || current.State != lifecycle.OperationUncertain {
			continue
		}
		settled, err = w.applyReconciledEntry(ctx, u, current, role, entry, action)
		if err != nil {
			return contracts.Ingestion{}, false, err
		}
	}
	durable, err := store.AdvanceCursor(ctx, u, ingested.Cursor)
	if err != nil {
		return contracts.Ingestion{}, false, err
	}
	if err = u.Commit(ctx); err != nil {
		return contracts.Ingestion{}, false, err
	}
	if replayWatermark != nil {
		*replayWatermark = durable
	}
	if settled {
		w.stats.Reconciled.Add(1)
	}
	return ingested, settled, nil
}
