package application

import (
	"context"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectorProposal is the immutable call an owner is being asked to approve,
// paired with the attention version used for the decision's If-Match.
type ConnectorProposal struct {
	Attention lifecycle.AttentionRequest
	Call      store.ConnectorCall
}

// GetConnectorProposal restricts raw arguments to the task owner. Tenant-wide
// read permission exposes safe attention metadata, never proposed tool input.
func GetConnectorProposal(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.AttentionID) (ConnectorProposal, error) {
	var proposal ConnectorProposal
	err := read(ctx, pool, actor, func(r *reader) error {
		attention, err := store.GetAttention(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		task, err := store.GetTask(ctx, r.uow, r.principal.Tenant, attention.Task)
		if err != nil {
			return err
		}
		if task.Owner != r.principal.ID {
			return ErrNotFound
		}
		call, err := store.GetConnectorCall(ctx, r.uow, r.principal.Tenant, attention.Operation)
		if err != nil {
			return err
		}
		if attention.Kind != lifecycle.AttentionActionApproval || call.Task != task.ID || call.Attempt != attention.Attempt || call.Provider != attention.Provider || call.InputDigest != attention.ActionDigest || contracts.ConnectionScope(call.Connection) != attention.Scope {
			return ErrNotFound
		}
		proposal = ConnectorProposal{Attention: attention, Call: call}
		return nil
	})
	return proposal, err
}
