package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QuestionView is the text of a question, paired with the attention version
// used for the decision's If-Match.
type QuestionView struct {
	Attention lifecycle.AttentionRequest
	Question  lifecycle.Question
}

// GetQuestion serves a question's text to the task owner only, as a connector
// proposal is served: tenant-wide read permission sees the attention's safe
// metadata but never the text, and everyone else reads not found. A request
// that is not a question, or has no stored text, reads as not found too
// (ADR-0026).
func GetQuestion(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.AttentionID) (QuestionView, error) {
	var view QuestionView
	err := read(ctx, pool, actor, func(r *reader) error {
		attention, err := store.GetAttention(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		task, err := store.GetTask(ctx, r.uow, r.principal.Tenant, attention.Task)
		if err != nil {
			return err
		}
		if task.Owner != r.principal.ID || attention.Kind != lifecycle.AttentionQuestion {
			return ErrNotFound
		}
		question, _, err := store.GetAttentionQuestion(ctx, r.uow, r.principal.Tenant, attention.ID)
		if err != nil {
			return err
		}
		view = QuestionView{Attention: attention, Question: question}
		return nil
	})
	return view, err
}

// checkAnswerText checks the text an answer decision carries against the
// question it answers. A question with stored text is answered with text that
// fits it; a request with none is answered by digest alone, as before the text
// was kept, and text sent for it is refused (ADR-0026).
func checkAnswerText(ctx context.Context, u *unit, tenant ids.TenantID, request lifecycle.AttentionRequest, cmd DecideAttention) error {
	question, _, err := store.GetAttentionQuestion(ctx, u.uow, tenant, request.ID)
	switch {
	case err == nil:
		if cmd.Answer == nil {
			return fmt.Errorf("%w: this question has text, so its answer carries the text", ErrInvalidCommand)
		}
		if err := cmd.Answer.CheckAgainst(question); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidCommand, err)
		}
		return nil
	case errors.Is(err, store.ErrNotFound):
		if cmd.Answer == nil {
			return nil
		}
		available, err := store.ContentSchemaAvailable(ctx, u.uow)
		if err != nil {
			return err
		}
		if !available {
			return ErrSchemaUnavailable
		}
		return fmt.Errorf("%w: this request has no question text to answer", ErrInvalidCommand)
	default:
		return err
	}
}
