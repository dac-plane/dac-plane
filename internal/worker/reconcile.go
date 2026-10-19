package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

// reconcileStream is the per-operation stream the reconciler follows.
const reconcileStream = "dispatch"

// outcomeEntryKind is the stream entry kind that carries an operation
// outcome. Producers (loopback in tests, providers in production) report
// terminal movement through it; every other entry only advances the cursor.
const outcomeEntryKind = "operation_outcome"

// outcomeEntryBody is the outcome entry payload: the reconciled state.
type outcomeEntryBody struct {
	State lifecycle.OperationState `json:"state"`
}

// reconcileOutcomeStates are the only states reconciliation may land on.
// Uncertain never returns to recorded or dispatched: an unknown effect is
// reconciled, never resent.
func reconcileOutcomeStates() map[lifecycle.OperationState]bool {
	return map[lifecycle.OperationState]bool{
		lifecycle.OperationAccepted:  true,
		lifecycle.OperationSucceeded: true,
		lifecycle.OperationFailed:    true,
		lifecycle.OperationRejected:  true,
	}
}

// Reconcile resolves uncertain operations from bound provider observations.
// Operations with no provider, no outcome, or an unparsable stream stay
// uncertain; reconciliation never redispatches.
func (w *Worker) Reconcile(ctx context.Context) error {
	tenants, err := store.ListTenants(ctx, w.pool)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.reconcileTenant(ctx, tenant); err != nil {
			w.logger.Error("reconcile failed", "tenant", tenant.String(), "error", err)
		}
	}
	return nil
}

func (w *Worker) reconcileTenant(ctx context.Context, tenant ids.TenantID) error {
	operations, err := store.UncertainOperations(ctx, w.pool, tenant)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.reconcileOperation(ctx, tenant, operation); err != nil {
			w.logger.Error("operation reconcile failed", "operation", operation.ID.String(), "error", err)
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
		return w.reconcileGoverned(ctx, tenant, operation, action)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	port, ok := w.ports[operation.Provider]
	if !ok {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}
	cursor, err := store.GetCursor(ctx, w.pool, tenant, operation.ID, reconcileStream)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		cursor = contracts.Cursor{
			Tenant: tenant, Attempt: operation.Attempt, Operation: operation.ID,
			Stream: reconcileStream, Generation: 1,
		}
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
			if errors.Is(err, io.EOF) || sctx.Err() != nil {
				break
			}
			// A broken stream ends this pass; the next tick resumes
			// from the persisted cursor.
			break
		}
		resolved, err := w.ingestFrame(sctx, tenant, operation, cursor, frame)
		if err != nil {
			return err
		}
		cursor = resolved
		if outcome, ok := outcomeOf(frame); ok {
			// The cursor advance commits atomically with the outcome:
			// persisting it first would mark the entry consumed while
			// the operation stays uncertain if the apply then fails.
			return w.applyOutcome(ctx, tenant, operation, outcome, cursor)
		}
		if err := w.persistCursor(ctx, tenant, cursor); err != nil {
			return err
		}
	}
	return w.persistCursor(ctx, tenant, cursor)
}

// ingestFrame binds one stream frame to the recorded operation and advances
// the cursor through the contract path, so dedupe, staleness and rehydrate
// behave exactly as ingestion elsewhere.
func (w *Worker) ingestFrame(ctx context.Context, tenant ids.TenantID, operation lifecycle.ProviderOperation, cursor contracts.Cursor, frame contracts.StreamFrame) (contracts.Cursor, error) {
	event := contracts.Event{
		Tenant: tenant, Attempt: operation.Attempt, Provider: operation.Provider,
		Operation: operation.ID, Stream: cursor.Stream, Generation: cursor.Generation,
		Body: frame,
	}
	bound, failure := contracts.BindEvent(contracts.Peer{Tenant: tenant, Provider: operation.Provider}, event, operation)
	if failure != nil {
		w.logger.Error("reconcile frame not bound", "operation", operation.ID.String(), "failure", failure)
		return cursor, nil
	}
	ingested, failure := contracts.Ingest(cursor, bound)
	if failure != nil {
		w.logger.Error("reconcile ingest failed", "operation", operation.ID.String(), "failure", failure)
		return cursor, nil
	}
	return ingested.Cursor, nil
}

// outcomeOf extracts a reconciled terminal state from a rows frame. Unknown
// entries are ignored: they advance the cursor but resolve nothing.
func outcomeOf(frame contracts.StreamFrame) (lifecycle.OperationState, bool) {
	if frame.Kind != contracts.StreamRows || frame.Rows == nil {
		return "", false
	}
	for _, entry := range frame.Rows.Entries {
		if entry.Kind != outcomeEntryKind {
			continue
		}
		var body outcomeEntryBody
		if err := json.Unmarshal(entry.Payload, &body); err != nil {
			continue
		}
		if reconcileOutcomeStates()[body.State] {
			return body.State, true
		}
	}
	return "", false
}

// applyOutcome reconciles an uncertain operation to its observed terminal
// state and records shadow evidence when the harness runs in shadow.
func (w *Worker) applyOutcome(ctx context.Context, tenant ids.TenantID, operation lifecycle.ProviderOperation, outcome lifecycle.OperationState, cursor contracts.Cursor) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if _, _, err := store.LockResultScope(ctx, uow, tenant, operation.Attempt); err != nil {
		return err
	}
	current, err := store.GetOperation(ctx, uow, tenant, operation.ID)
	if err != nil {
		return err
	}
	resolved := false
	if current.State == lifecycle.OperationUncertain {
		tr, err := w.transition(current.Version, time.Now().UTC())
		if err != nil {
			return err
		}
		reconciled, _, err := store.ApplyOperation(ctx, uow, tenant, current.ID, lifecycle.OperationReconcile, outcome, tr)
		if err != nil {
			if errors.Is(err, store.ErrStaleVersion) {
				return nil
			}
			return err
		}
		// The reconciled outcome resolves the Attempt the uncertainty stopped.
		step := progress.Step{UOW: uow, Tenant: tenant, Actor: SystemActor(), At: tr.At}
		if err := step.FollowOperation(ctx, reconciled); err != nil {
			return err
		}
		if harness, err := store.GetHarness(ctx, uow, tenant, current.Provider); err == nil && harness.Mode == lifecycle.HarnessShadow {
			shadowID, err := ids.New[ids.Any]()
			if err != nil {
				return err
			}
			if _, _, err := store.RecordShadowResult(ctx, uow, store.ShadowResult{
				ID: shadowID, Tenant: tenant, Attempt: current.Attempt, CapturedAt: tr.At,
				Summary: "reconciled " + string(outcome), Comparison: "observed",
				Digest: identity.DigestOf([]byte(current.ID.String() + ":" + string(outcome))),
			}); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		resolved = true
	}
	// Someone else may have resolved the operation first; either way the
	// entry is consumed, so the cursor advances in the same commit.
	if _, err := store.AdvanceCursor(ctx, uow, cursor); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if resolved {
		w.stats.Reconciled.Add(1)
	}
	return nil
}

// persistCursor stores ingestion progress so the next tick resumes where
// this one stopped, even mid-stream. It never moves the stream behind an
// ingest that committed after this pass read its cursor.
func (w *Worker) persistCursor(ctx context.Context, tenant ids.TenantID, cursor contracts.Cursor) error {
	uow, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	if _, err := store.AdvanceCursor(ctx, uow, cursor); err != nil {
		return err
	}
	if err := uow.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
