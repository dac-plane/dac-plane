package codexadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
	"github.com/dac-plane/dac-plane/adapters/codex/appserver"
)

const userInputParamsJSON = `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":true,"questions":[
 {"id":"scope","header":"Scope","question":"Which tests should run?","isOther":true,"options":[{"label":"unit","description":"Fast"},{"label":"all","description":"Slow"}]},
 {"id":"why","header":"Why","question":"Why does it matter?"}]}`

func (f *fixture) raiseUserInput(t *testing.T, params string) {
	t.Helper()
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.requests <- appserver.Message{ID: json.RawMessage(`23`), Method: "item/tool/requestUserInput", Params: json.RawMessage(params)}
}

func (f *fixture) attentionEntries(t *testing.T) []map[string]any {
	t.Helper()
	entries, err := f.store.EventsAfter(testOperation, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, entry := range entries {
		if entry.Kind != "attention.requested" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, payload)
	}
	return out
}

func (f *fixture) lastReply(t *testing.T) string {
	t.Helper()
	f.native.mu.Lock()
	defer f.native.mu.Unlock()
	if len(f.native.replies) == 0 {
		return ""
	}
	var wrapped struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(f.native.replies[len(f.native.replies)-1], &wrapped); err != nil {
		t.Fatal(err)
	}
	return string(wrapped.Result)
}

func TestUserInputBecomesAQuestionAndIsAnswered(t *testing.T) {
	f := setup(t)
	f.raiseUserInput(t, userInputParamsJSON)
	var raised []map[string]any
	waitFor(t, func() bool { raised = f.attentionEntries(t); return len(raised) == 1 })
	question := raised[0]
	if question["kind"] != "question" || question["native_ref"] != "ask-1" || question["action"] != nil || question["scope"] != nil {
		t.Fatalf("attention = %v, want a question with no action", question)
	}
	prompts := question["question"].(map[string]any)["prompts"].([]any)
	first := prompts[0].(map[string]any)
	if len(prompts) != 2 || first["id"] != "scope" || first["text"] != "Which tests should run?" || first["other"] != true || len(first["options"].([]any)) != 2 {
		t.Fatalf("prompts = %v", prompts)
	}

	decision := `{"decision_id":"01a18301-0000-7000-8000-000000000010","attention":"01a18301-0000-7000-8000-000000000011","attempt":"` + testAttempt + `","native_ref":"ask-1","kind":"answer","answer_digest":"sha256:` + strings.Repeat("0", 64) + `","answer":{"scope":["unit"],"why":["Faster feedback"]},"expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	request := startRequest(decision)
	request.Method = "steer_turn"
	request.Delivery = "01a18301-0000-7000-8000-000000000012"
	reply, err := f.adapter.Send(context.Background(), request)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("steer = %+v, err = %v", reply, err)
	}
	var got struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(f.lastReply(t)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Answers["scope"].Answers[0] != "unit" || got.Answers["why"].Answers[0] != "Faster feedback" {
		t.Fatalf("native reply = %s, want the answers keyed by prompt id", f.lastReply(t))
	}
}

func TestUserInputThatCannotBeShownIsDeclined(t *testing.T) {
	long := strings.Repeat("x", 2049)
	for name, params := range map[string]string{
		"secret prompt": `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":true,"questions":[{"id":"token","header":"Token","question":"Paste the token","isSecret":true}]}`,
		"no questions":  `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":true,"questions":[]}`,
		"too long":      `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":true,"questions":[{"id":"a","header":"A","question":"` + long + `"}]}`,
		"duplicate ids": `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":true,"questions":[{"id":"a","header":"A","question":"Why?"},{"id":"a","header":"B","question":"How?"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.raiseUserInput(t, params)
			waitFor(t, func() bool { return f.lastReply(t) != "" })
			if got := f.lastReply(t); got != `{"answers":{}}` {
				t.Fatalf("native reply = %s, want no answers", got)
			}
			if raised := f.attentionEntries(t); len(raised) != 0 {
				t.Fatalf("raised %v, want no attention", raised)
			}
		})
	}
}

func TestDecisionKindMustFitTheNativeRequest(t *testing.T) {
	f := setup(t)
	f.raiseUserInput(t, userInputParamsJSON)
	waitFor(t, func() bool { return len(f.attentionEntries(t)) == 1 })
	for name, kind := range map[string]string{"approving a question": `"kind":"approve_once"`, "answer without text": `"kind":"answer","answer_digest":"sha256:` + strings.Repeat("0", 64) + `"`} {
		decision := `{"decision_id":"01a18301-0000-7000-8000-000000000010","attention":"01a18301-0000-7000-8000-000000000011","attempt":"` + testAttempt + `","native_ref":"ask-1",` + kind + `,"expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
		request := startRequest(decision)
		request.Method = "steer_turn"
		request.Delivery = "01a18301-0000-7000-8000-000000000012"
		reply, err := f.adapter.Send(context.Background(), request)
		if err != nil || reply.Durability != wire.DurabilityRejected {
			t.Errorf("%s: reply = %+v, err = %v, want rejected", name, reply, err)
		}
	}
	if got := f.lastReply(t); got != "" {
		t.Fatalf("native reply = %s, want none for a rejected decision", got)
	}
}

// Codex answers request_user_input with "unavailable in Default mode" unless the
// thread turns the feature on, so every thread must (observed live on 0.165.0).
func TestThreadsEnableRequestUserInput(t *testing.T) {
	f := setup(t)
	if _, err := f.adapter.Send(context.Background(), startRequest(validStart)); err != nil {
		t.Fatal(err)
	}
	f.native.mu.Lock()
	defer f.native.mu.Unlock()
	for _, call := range f.native.calls {
		if call.method != "thread/start" {
			continue
		}
		if !strings.Contains(string(call.params), `"features":{"default_mode_request_user_input":true}`) {
			t.Fatalf("thread/start params = %s, want request_user_input enabled", call.params)
		}
		return
	}
	t.Fatal("no thread/start call")
}

// Codex 0.165.0 sends isBlocking false for request_user_input in Default mode
// yet still waits for the reply (observed live), so it is a question like any other.
func TestNonBlockingUserInputIsStillAQuestion(t *testing.T) {
	f := setup(t)
	f.raiseUserInput(t, `{"threadId":"thread-1","turnId":"turn-1","itemId":"ask-1","isBlocking":false,"questions":[{"id":"a","header":"A","question":"Why?"}]}`)
	waitFor(t, func() bool { return len(f.attentionEntries(t)) == 1 })
	if got := f.lastReply(t); got != "" {
		t.Fatalf("native reply = %s, want none until the question is answered", got)
	}
}
