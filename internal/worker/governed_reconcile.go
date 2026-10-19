package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// A governed operation settles only from its exact execution receipt. A bare
// operation_outcome cannot prove its effect, and action settlement does not
// resolve the parent runtime's lifecycle or unrelated uncertainty.
func (w *Worker) reconcileGoverned(ctx context.Context, tenant ids.TenantID, op lifecycle.ProviderOperation, action store.GovernedAction) error {
	port := w.enforcement[op.Provider]
	if port == nil {
		port = w.ports[op.Provider]
	}
	if port == nil || port.Role() != contracts.RoleEnforcement {
		return nil
	}
	cursor := contracts.Cursor{Tenant: tenant, Attempt: op.Attempt, Operation: op.ID, Stream: reconcileStream, Generation: 1}
	if saved, err := store.GetCursor(ctx, w.pool, tenant, op.ID, reconcileStream); err == nil {
		cursor = saved
	}
	sctx, cancel := context.WithTimeout(ctx, w.cfg.StreamTimeout)
	defer cancel()
	stream, err := port.Subscribe(sctx, cursor)
	if err != nil {
		return nil
	}
	defer func() { _ = stream.Close() }()
	for {
		frame, err := stream.Next(sctx)
		if err != nil {
			return nil
		}
		event := contracts.Event{Tenant: tenant, Attempt: op.Attempt, Provider: op.Provider, Operation: op.ID, Stream: cursor.Stream, Generation: cursor.Generation, Body: frame}
		bound, failure := contracts.BindEvent(contracts.Peer{Tenant: tenant, Provider: op.Provider}, event, op)
		if failure != nil {
			return nil
		}
		ingested, failure := contracts.Ingest(cursor, bound)
		if failure != nil || ingested.Rehydrate {
			return nil
		}
		for _, entry := range ingested.Entries {
			if entry.Kind != "enforcement.receipt" {
				continue
			}
			var receipt contracts.EnforcementReceipt
			if json.Unmarshal(entry.Payload, &receipt) != nil || receipt.Validate() != nil {
				continue
			}
			expected, _ := json.Marshal(action.Execution)
			actual, _ := json.Marshal(receipt.Execution)
			if string(expected) != string(actual) {
				continue
			}
			u, err := store.Begin(ctx, w.pool)
			if err != nil {
				return err
			}
			defer func() { _ = u.Rollback(ctx) }()
			if err = store.LockGovernedAction(ctx, u, tenant, op.ID); err != nil {
				return err
			}
			current, err := store.GetOperation(ctx, u, tenant, op.ID)
			if err != nil {
				return err
			}
			if current.State == lifecycle.OperationUncertain {
				tr, err := w.transition(current.Version, time.Now().UTC())
				if err != nil {
					return err
				}
				if _, _, err = store.ApplyOperation(ctx, u, tenant, op.ID, lifecycle.OperationReconcile, lifecycle.OperationSucceeded, tr); err != nil {
					return err
				}
				if err = store.InsertEnforcementReceipt(ctx, u, receipt); err != nil {
					return err
				}
			}
			if _, err = store.AdvanceCursor(ctx, u, ingested.Cursor); err != nil {
				return err
			}
			return u.Commit(ctx)
		}
		cursor = ingested.Cursor
	}
}
