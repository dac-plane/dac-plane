//go:build codexqualification

package codexqualification

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// TestCodexQuestionQualification has a live model ask a person a question
// through Codex's request_user_input tool (ADR-0026). The owner reads the
// question over the Surface API and answers it in words; the model then writes
// the answer to a file. The answer carries a marker the prompt never names, so
// the artifact proves the typed text, not just its digest, reached the model.
func TestCodexQuestionQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client := qualificationClient(t)
	token := &tokenSource{t: t, ctx: ctx, client: client}

	submitted := submit(t, ctx, client, token,
		"Use the request_user_input tool exactly once to ask the user one question: id fruit, header Fruit, question \"Which fruit should I record?\", with no options, so the user answers in free text. Wait for the answer. Then use exec_command exactly once with sandbox_permissions require_escalated to write the answer exactly as the user gave it, followed by one newline, to answer.txt with printf. Do not use another file-writing tool.",
		"answer.txt contains the answer the user gave to the question")
	marker := "teal-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	asked := false
	var answered string
	for rounds := 0; rounds < 6; rounds++ {
		attention, done := waitAttentionOrReady(t, ctx, client, token, submitted.Task.ID)
		if done {
			break
		}
		if attention.Kind != lifecycle.AttentionQuestion {
			decide(t, ctx, client, token, attention, lifecycle.DecisionApproveOnce)
			waitDecisionConsumed(t, ctx, client, token, submitted.Task.ID, attention.ID)
			continue
		}
		asked = true
		read := request[httpapi.QuestionBody](t, ctx, client, token, http.MethodGet, "/attention/"+attention.ID.String()+"/question", nil, nil, http.StatusOK)
		prompts := read.value.Question.Prompts
		if len(prompts) != 1 {
			t.Fatalf("question = %+v, want the one prompt the task asked for", read.value.Question)
		}
		prompt := prompts[0]
		// Only free text proves the answer's words reached the model: a fixed
		// option is named in the task, so the model could write it unasked.
		if len(prompt.Options) > 0 && !prompt.Other {
			t.Fatalf("prompt = %+v, want one that accepts free text", prompt)
		}
		answered = marker
		answer := lifecycle.Answer{prompt.ID: {answered}}
		digest, err := answer.Digest()
		if err != nil {
			t.Fatal(err)
		}
		request[httpapi.DecisionBody](t, ctx, client, token, http.MethodPost, "/attention/"+attention.ID.String()+"/decisions", map[string]string{
			"Idempotency-Key": "codex-answer-" + strconv.FormatInt(time.Now().UnixNano(), 10),
			"If-Match":        read.header.Get("ETag"),
		}, httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digest, Answer: answer}, http.StatusCreated)
		waitDecisionConsumed(t, ctx, client, token, submitted.Task.ID, attention.ID)
	}
	if !asked {
		t.Fatal("the model never asked its question through request_user_input")
	}
	ready := waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskReadyForReview)
	if len(ready.value.Revisions) != 1 || len(ready.value.Revisions[0].Revision.Artifacts) != 1 {
		t.Fatalf("revisions=%+v", ready.value.Revisions)
	}
	if got, want := string(artifactEvidence(t, ctx, client, ready.value.Revisions[0].Revision.Artifacts[0])), answered+"\n"; got != want {
		t.Fatalf("artifact bytes=%q, want %q, which only the delivered answer names", got, want)
	}
	review(t, ctx, client, token, ready, lifecycle.DispositionAccept)
	_ = waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskSucceeded)
}
