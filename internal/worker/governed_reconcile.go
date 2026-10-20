package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

// Runtime outcomes and exact governed receipts share cursor/transaction gates,
// but not authority: a bare outcome never proves a governed action's effect.
func (w *Worker) applyReconciledEntry(ctx context.Context, u *store.UnitOfWork, current lifecycle.ProviderOperation, entry contracts.Entry, action *store.GovernedAction) (bool, error) {
	if entry.PayloadOmitted {
		return false, nil
	}
	var receipt *contracts.EnforcementReceipt
	var outcome lifecycle.OperationState
	if action != nil {
		if entry.Kind != "enforcement.receipt" {
			return false, nil
		}
		r, err := contracts.DecodeEntry[contracts.EnforcementReceipt](entry)
		if err != nil {
			return false, nil
		}
		expected, _ := json.Marshal(action.Execution)
		actual, _ := json.Marshal(r.Execution)
		if string(expected) != string(actual) {
			return false, nil
		}
		receipt = &r
		outcome = lifecycle.OperationSucceeded
	} else {
		if entry.Kind != contracts.EntryOperationOutcome {
			return false, nil
		}
		body, err := contracts.DecodeEntry[contracts.OperationOutcome](entry)
		if err != nil {
			return false, nil
		}
		outcome = body.State
	}
	tr, err := w.transition(current.Version, time.Now().UTC())
	if err != nil {
		return false, err
	}
	op, _, err := store.ApplyOperation(ctx, u, current.Tenant, current.ID, lifecycle.OperationReconcile, outcome, tr)
	if err != nil {
		return false, err
	}
	if receipt != nil {
		return true, store.InsertEnforcementReceipt(ctx, u, *receipt)
	}
	err = (progress.Step{UOW: u, Tenant: current.Tenant, Actor: SystemActor(), At: tr.At}).FollowOperation(ctx, op)
	return true, err
}
