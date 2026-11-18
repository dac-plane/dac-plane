//go:build integration

package postgres_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

func TestQuestionAndAnswerTextRoundTrip(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	question := lifecycle.Question{Prompts: []lifecycle.Prompt{
		{ID: "scope", Header: "Scope", Text: "Which tests should run?", Options: []lifecycle.PromptOption{{Label: "unit"}, {Label: "all", Description: "Unit and integration"}}},
		{ID: "why", Text: "Why does it matter?"},
	}}
	answer := lifecycle.Answer{"scope": {"unit"}, "why": {"Faster feedback é"}}
	digest, err := answer.Digest()
	if err != nil {
		t.Fatal(err)
	}

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	request, err := lifecycle.NewAttentionRequest(lifecycle.AttentionRequest{
		ID: mustID[ids.Attention](t), Tenant: f.tenant, Task: task.ID, Attempt: attempt.ID,
		Kind: lifecycle.AttentionQuestion, PolicyGeneration: 1,
		Responders: []ids.PrincipalID{f.principal}, ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("NewAttentionRequest() error = %v", err)
	}
	if _, _, err := store.InsertAttention(ctx, uow, request); err != nil {
		t.Fatalf("InsertAttention() error = %v", err)
	}

	if _, _, err := store.GetAttentionQuestion(ctx, uow, f.tenant, request.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAttentionQuestion() before a write error = %v, want ErrNotFound", err)
	}
	if err := store.InsertAttentionQuestion(ctx, uow, f.tenant, request.ID, question, now); err != nil {
		t.Fatalf("InsertAttentionQuestion() error = %v", err)
	}
	if err := store.InsertAttentionQuestion(ctx, uow, f.tenant, request.ID, question, now); err != nil {
		t.Errorf("replayed InsertAttentionQuestion() error = %v, want nil", err)
	}
	other := lifecycle.Question{Prompts: []lifecycle.Prompt{{ID: "x", Text: "A different question?"}}}
	if err := store.InsertAttentionQuestion(ctx, uow, f.tenant, request.ID, other, now); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Errorf("InsertAttentionQuestion() of other text error = %v, want ErrPayloadMismatch", err)
	}
	got, stored, err := store.GetAttentionQuestion(ctx, uow, f.tenant, request.ID)
	if err != nil || stored.IsZero() || len(got.Prompts) != 2 || got.Prompts[0].Options[1].Description != "Unit and integration" {
		t.Fatalf("GetAttentionQuestion() = %v, %v, %v", got, stored, err)
	}
	if _, _, err := store.GetAttentionQuestion(ctx, uow, mustID[ids.Tenant](t), request.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAttentionQuestion() in another tenant error = %v, want ErrNotFound", err)
	}

	decision := lifecycle.Decision{
		ID: mustID[ids.Decision](t), Tenant: f.tenant, Request: request.ID, RequestVersion: 1,
		Principal: f.principal, Kind: lifecycle.DecisionAnswer, AnswerDigest: digest, DecidedAt: now,
	}
	tr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	if _, _, _, err := store.DecideAttention(ctx, uow, decision, 1, tr); err != nil {
		t.Fatalf("DecideAttention() error = %v", err)
	}
	if err := store.InsertDecisionAnswer(ctx, uow, f.tenant, decision.ID, request.ID, answer, now); err != nil {
		t.Fatalf("InsertDecisionAnswer() error = %v", err)
	}
	if err := store.InsertDecisionAnswer(ctx, uow, f.tenant, decision.ID, request.ID, answer, now); err != nil {
		t.Errorf("replayed InsertDecisionAnswer() error = %v, want nil", err)
	}
	changed := lifecycle.Answer{"scope": {"all"}, "why": {"x"}}
	if err := store.InsertDecisionAnswer(ctx, uow, f.tenant, decision.ID, request.ID, changed, now); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Errorf("InsertDecisionAnswer() of other text error = %v, want ErrPayloadMismatch", err)
	}
	read, err := store.GetDecisionAnswer(ctx, uow, f.tenant, decision.ID)
	if err != nil {
		t.Fatalf("GetDecisionAnswer() error = %v", err)
	}
	if err := read.VerifyDigest(digest); err != nil {
		t.Errorf("stored answer does not match the recorded digest: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// Both records are append-only.
	for _, stmt := range []string{
		`update attention_question set content = '{"prompts":[]}'`,
		`delete from attention_question`,
		`update decision_answer set content = '{"a":["b"]}'`,
		`delete from decision_answer`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "dac_plane_immutable") {
			t.Errorf("%s: error = %v, want dac_plane_immutable", stmt, err)
		}
	}
}

// TestQuestionTextBeforeTheContentSchema pins the rollout behaviour of
// ADR-0011: a binary serving schema 18 reads no text and refuses to store any.
func TestQuestionTextBeforeTheContentSchema(t *testing.T) {
	ctx, pool := setupTestDB(t)
	f := seedTenant(t, ctx, pool)
	if _, err := pool.Exec(ctx, `update goose_db_version set is_applied = false where version_id = 19`); err != nil {
		t.Fatal(err)
	}
	attention, decision := mustID[ids.Attention](t), mustID[ids.Decision](t)
	question := lifecycle.Question{Prompts: []lifecycle.Prompt{{ID: "a", Text: "Why?"}}}
	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	if err := store.InsertAttentionQuestion(ctx, uow, f.tenant, attention, question, time.Now().UTC()); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Errorf("InsertAttentionQuestion() error = %v, want ErrSchemaUnavailable", err)
	}
	if err := store.InsertDecisionAnswer(ctx, uow, f.tenant, decision, attention, lifecycle.Answer{"a": {"b"}}, time.Now().UTC()); !errors.Is(err, store.ErrSchemaUnavailable) {
		t.Errorf("InsertDecisionAnswer() error = %v, want ErrSchemaUnavailable", err)
	}
	if _, _, err := store.GetAttentionQuestion(ctx, uow, f.tenant, attention); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAttentionQuestion() error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetDecisionAnswer(ctx, uow, f.tenant, decision); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetDecisionAnswer() error = %v, want ErrNotFound", err)
	}
}
