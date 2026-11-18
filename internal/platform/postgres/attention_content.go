package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// contentSchemaVersion is the first schema with question and answer text
// (ADR-0026). A binary serving an earlier schema during a rollout reads no text
// and refuses to store any, and serves everything else (ADR-0011).
const contentSchemaVersion = 19

func requireContentSchema(ctx context.Context, q Querier) error {
	version, err := SchemaVersion(ctx, q)
	if err != nil {
		return err
	}
	if version < contentSchemaVersion {
		return ErrSchemaUnavailable
	}
	return nil
}

// InsertAttentionQuestion stores the text of a question once, beside its
// request. A second write of the same text is a replay; different text for the
// same request is a mismatch, never a rewrite (ADR-0026).
func InsertAttentionQuestion(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attention ids.AttentionID, question lifecycle.Question, at time.Time) error {
	if err := question.Validate(); err != nil {
		return err
	}
	if err := requireContentSchema(ctx, u); err != nil {
		return err
	}
	content, err := json.Marshal(question)
	if err != nil {
		return err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into attention_question (tenant_id, attention_id, content, created_at)
		values ($1, $2, $3::jsonb, $4)`,
		tenant.String(), attention.String(), string(content), at,
	)
	if err != nil || inserted {
		return err
	}
	stored, _, err := GetAttentionQuestion(ctx, u, tenant, attention)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, question) {
		return ErrPayloadMismatch
	}
	return nil
}

// GetAttentionQuestion reads the text of a request's question and when it was
// stored. A request without stored text, or a database before the content
// schema, is ErrNotFound.
func GetAttentionQuestion(ctx context.Context, q Querier, tenant ids.TenantID, attention ids.AttentionID) (lifecycle.Question, time.Time, error) {
	if err := requireContentSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return lifecycle.Question{}, time.Time{}, ErrNotFound
		}
		return lifecycle.Question{}, time.Time{}, err
	}
	var content []byte
	var created time.Time
	err := q.QueryRow(ctx, `
		select content::text, created_at from attention_question
		where tenant_id = $1 and attention_id = $2`,
		tenant.String(), attention.String(),
	).Scan(&content, &created)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Question{}, time.Time{}, ErrNotFound
		}
		return lifecycle.Question{}, time.Time{}, err
	}
	var question lifecycle.Question
	if err := json.Unmarshal(content, &question); err != nil {
		return lifecycle.Question{}, time.Time{}, corruptRow("attention_question.content", err)
	}
	if err := question.Validate(); err != nil {
		return lifecycle.Question{}, time.Time{}, corruptRow("attention_question.content", err)
	}
	return question, created, nil
}

// InsertDecisionAnswer stores the text of an answer once, beside the decision
// whose digest it is. The caller has already checked it against that digest.
func InsertDecisionAnswer(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, decision ids.DecisionID, attention ids.AttentionID, answer lifecycle.Answer, at time.Time) error {
	if err := answer.Validate(); err != nil {
		return err
	}
	if err := requireContentSchema(ctx, u); err != nil {
		return err
	}
	content, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into decision_answer (tenant_id, decision_id, attention_id, content, created_at)
		values ($1, $2, $3, $4::jsonb, $5)`,
		tenant.String(), decision.String(), attention.String(), string(content), at,
	)
	if err != nil || inserted {
		return err
	}
	stored, err := GetDecisionAnswer(ctx, u, tenant, decision)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, answer) {
		return ErrPayloadMismatch
	}
	return nil
}

// GetDecisionAnswer reads the text of a decision's answer. A decision without
// stored text, or a database before the content schema, is ErrNotFound.
func GetDecisionAnswer(ctx context.Context, q Querier, tenant ids.TenantID, decision ids.DecisionID) (lifecycle.Answer, error) {
	if err := requireContentSchema(ctx, q); err != nil {
		if errors.Is(err, ErrSchemaUnavailable) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var content []byte
	err := q.QueryRow(ctx, `
		select content::text from decision_answer
		where tenant_id = $1 and decision_id = $2`,
		tenant.String(), decision.String(),
	).Scan(&content)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var answer lifecycle.Answer
	if err := json.Unmarshal(content, &answer); err != nil {
		return nil, corruptRow("decision_answer.content", err)
	}
	if err := answer.Validate(); err != nil {
		return nil, corruptRow("decision_answer.content", err)
	}
	return answer, nil
}
