//go:build integration

package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

// askQuestion submits a task and has its runtime raise one question, with or
// without the text the runtime asked.
func askQuestion(t *testing.T, s *surface, nonce string, text *lifecycle.Question) (path string, version uint64) {
	t.Helper()
	sub := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: nonce, body: s.submitBody()}), http.StatusCreated)
	rt := s.dispatch(t, sub.Operation.ID)
	s.observe(t, rt, sub,
		row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-1"}),
		row(2, contracts.EntryAttentionRequested, contracts.AttentionRequested{NativeRef: "q-" + nonce, Kind: lifecycle.AttentionQuestion, ExpiresAt: time.Now().Add(time.Hour), Question: text}),
	)
	list := decodeInto[httpapi.AttentionListBody](t, s.do(t, call{method: "GET", path: "/attention", as: "owner"}), http.StatusOK)
	if len(list.Attention) != 1 {
		t.Fatalf("open attention = %d, want 1", len(list.Attention))
	}
	return "/attention/" + list.Attention[0].ID.String(), list.Attention[0].Version
}

func TestQuestionTextOverHTTP(t *testing.T) {
	s := newSurface(t)
	asked := &lifecycle.Question{Prompts: []lifecycle.Prompt{
		{ID: "scope", Header: "Scope", Text: "Which tests should run?", Options: []lifecycle.PromptOption{{Label: "unit"}, {Label: "all"}}},
		{ID: "why", Text: "Why does it matter?"},
	}}
	path, version := askQuestion(t, s, "q1", asked)

	// Only the task owner reads the text; tenant readers and others read 404.
	for _, actor := range []string{"other", "reader"} {
		s.do(t, call{method: "GET", path: path + "/question", as: actor}).want(t, http.StatusNotFound)
	}
	read := s.do(t, call{method: "GET", path: path + "/question", as: "owner"})
	if read.header.Get("ETag") != `"`+strconv.FormatUint(version, 10)+`"` || read.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("question headers = %v", read.header)
	}
	body := decodeInto[httpapi.QuestionBody](t, read, http.StatusOK)
	if body.AttentionVersion != version || len(body.Question.Prompts) != 2 || body.Question.Prompts[0].Text != "Which tests should run?" {
		t.Fatalf("question = %+v", body)
	}

	// The text is required, must fit the question and must match its digest.
	decisions := path + "/decisions"
	answer := lifecycle.Answer{"scope": {"unit"}, "why": {"Faster feedback"}}
	digest, err := answer.Digest()
	if err != nil {
		t.Fatal(err)
	}
	attempt := func(key string, request httpapi.DecisionRequest) reply {
		return s.do(t, call{method: "POST", path: decisions, as: "owner", key: key, ifMatch: read.header.Get("ETag"), body: request})
	}
	attempt("digest-only", httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digest}).want(t, http.StatusUnprocessableEntity)
	wrong := lifecycle.Answer{"scope": {"none"}, "why": {"x"}}
	wrongDigest, _ := wrong.Digest()
	attempt("not-an-option", httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: wrongDigest, Answer: wrong}).want(t, http.StatusUnprocessableEntity)
	attempt("wrong-digest", httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: identity.DigestOf([]byte("other")), Answer: answer}).want(t, http.StatusUnprocessableEntity)
	decided := decodeInto[httpapi.DecisionBody](t, attempt("answer", httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digest, Answer: answer}), http.StatusCreated)

	// The decision records the digest and the text sits beside it.
	decisionID, err := ids.Parse[ids.Decision](decided.Decision)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := store.GetDecisionAnswer(s.ctx, s.pool, s.tenant, decisionID)
	if err != nil {
		t.Fatalf("GetDecisionAnswer() error = %v", err)
	}
	if err := decision.VerifyDigest(digest); err != nil {
		t.Fatalf("stored text does not match the recorded digest: %v", err)
	}
}

func TestAnswerWithoutQuestionText(t *testing.T) {
	s := newSurface(t)
	path, version := askQuestion(t, s, "q2", nil)
	s.do(t, call{method: "GET", path: path + "/question", as: "owner"}).want(t, http.StatusNotFound)
	answer := lifecycle.Answer{"a": {"yes"}}
	digest, _ := answer.Digest()
	etag := `"` + strconv.FormatUint(version, 10) + `"`
	// Text for a request that has none is refused; the digest alone is accepted, as before.
	s.do(t, call{method: "POST", path: path + "/decisions", as: "owner", key: "text", ifMatch: etag, body: httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digest, Answer: answer}}).want(t, http.StatusUnprocessableEntity)
	s.do(t, call{method: "POST", path: path + "/decisions", as: "owner", key: "digest", ifMatch: etag, body: httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: digest}}).want(t, http.StatusCreated)
}
