package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
)

// ConnectorProposalBody describes the exact proposed effect to its owner.
type ConnectorProposalBody struct {
	Attention        ids.AttentionID  `json:"attention_id"`
	AttentionVersion uint64           `json:"attention_version"`
	Operation        ids.OperationID  `json:"operation_id"`
	Connection       ids.ConnectionID `json:"connection_id"`
	Tool             string           `json:"tool"`
	Input            json.RawMessage  `json:"input"`
	InputDigest      identity.Digest  `json:"input_digest"`
}

func (a *API) getConnectorProposal(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Attention](r, "attention")
	if err != nil {
		return err
	}
	proposal, err := application.GetConnectorProposal(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(proposal.Attention.Version))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ConnectorProposalBody{Attention: proposal.Attention.ID, AttentionVersion: proposal.Attention.Version, Operation: proposal.Call.Operation, Connection: proposal.Call.Connection, Tool: proposal.Call.Tool, Input: proposal.Call.Input, InputDigest: proposal.Call.InputDigest})
	return nil
}
