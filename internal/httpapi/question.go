package httpapi

import (
	"net/http"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// QuestionBody is the text a runtime asked, served to the task owner with the
// attention version the decision is made on (ADR-0026).
type QuestionBody struct {
	Attention        ids.AttentionID    `json:"attention_id"`
	AttentionVersion uint64             `json:"attention_version"`
	Question         lifecycle.Question `json:"question"`
}

func (a *API) getQuestion(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Attention](r, "attention")
	if err != nil {
		return err
	}
	view, err := application.GetQuestion(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(view.Attention.Version))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, QuestionBody{Attention: view.Attention.ID, AttentionVersion: view.Attention.Version, Question: view.Question})
	return nil
}
