package codexadapter

import (
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/dac-plane/dac-plane/adapters/codex/appserver"
	"github.com/dac-plane/dac-plane/adapters/codex/journal"
)

// userInputMethod is the native request Codex raises to ask a person something
// in words. It becomes a question attention (ADR-0026).
const userInputMethod = "item/tool/requestUserInput"

func isNativeAttention(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", userInputMethod:
		return true
	}
	return false
}

// The bounds dac-plane puts on a question. A request outside them is declined
// rather than truncated, since a person must see exactly what was asked.
const (
	maxPrompts       = 8
	maxIDChars       = 64
	maxHeaderChars   = 128
	maxTextChars     = 2048
	maxOptions       = 8
	maxLabelChars    = 256
	maxOptionDesc    = 512
	maxQuestionBytes = 16 * 1024
)

type userInputParams struct {
	IsBlocking bool `json:"isBlocking"`
	Questions  []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		IsOther  bool   `json:"isOther"`
		IsSecret bool   `json:"isSecret"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// raiseQuestion turns a native user-input request into a question attention, or
// declines it. A prompt marked secret is never carried (a secret answer must not
// reach the product's records), a request that does not block is not a wait, and
// a request outside the question bounds cannot be shown faithfully; each is
// answered with no answers so Codex does not wait on a person who was never asked.
func (a *Adapter) raiseQuestion(op journal.Operation, m appserver.Message, itemID string) {
	var p userInputParams
	if json.Unmarshal(m.Params, &p) != nil {
		a.declineUserInput(m, "invalid_user_input")
		return
	}
	question, ok := questionOf(p)
	if !p.IsBlocking || !ok {
		a.declineUserInput(m, "user_input_declined")
		return
	}
	request := journal.NativeRequest{NativeRef: itemID, Operation: op.ID, Method: m.Method, Session: a.session, RequestID: m.ID}
	if a.config.Journal.PutNativeRequest(request) != nil {
		return
	}
	_, _ = a.config.Journal.AppendEvent(op.ID, "attention:"+itemID, "attention.requested", map[string]any{
		"native_ref": itemID, "kind": "question", "question": question,
		"expires_at": a.config.Now().Add(15 * time.Minute),
	})
}

func (a *Adapter) declineUserInput(m appserver.Message, reason string) {
	a.logDecisionFailure(reason, nil)
	_ = a.config.Native.Reply(m.ID, map[string]any{"answers": map[string]any{}})
}

// questionOf maps Codex's questions to dac-plane's prompts, reporting false for
// a secret prompt or one outside the bounds.
func questionOf(p userInputParams) (map[string]any, bool) {
	if len(p.Questions) == 0 || len(p.Questions) > maxPrompts {
		return nil, false
	}
	prompts := make([]map[string]any, 0, len(p.Questions))
	ids := map[string]bool{}
	total := 0
	for _, q := range p.Questions {
		if q.IsSecret || ids[q.ID] || !textOK(q.ID, 1, maxIDChars) || !textOK(q.Question, 1, maxTextChars) || len(q.Options) > maxOptions {
			return nil, false
		}
		ids[q.ID] = true
		header := truncateChars(q.Header, maxHeaderChars)
		if !textOK(header, 0, maxHeaderChars) {
			return nil, false
		}
		prompt := map[string]any{"id": q.ID, "text": q.Question}
		if header != "" {
			prompt["header"] = header
		}
		total += len(q.ID) + len(header) + len(q.Question)
		if len(q.Options) > 0 {
			labels := map[string]bool{}
			options := make([]map[string]any, 0, len(q.Options))
			for _, o := range q.Options {
				desc := truncateChars(o.Description, maxOptionDesc)
				if labels[o.Label] || !textOK(o.Label, 1, maxLabelChars) || !textOK(desc, 0, maxOptionDesc) {
					return nil, false
				}
				labels[o.Label] = true
				option := map[string]any{"label": o.Label}
				if desc != "" {
					option["description"] = desc
				}
				options = append(options, option)
				total += len(o.Label) + len(desc)
			}
			prompt["options"] = options
			if q.IsOther {
				prompt["other"] = true
			}
		}
		prompts = append(prompts, prompt)
	}
	if total > maxQuestionBytes {
		return nil, false
	}
	return map[string]any{"prompts": prompts}, true
}

// textOK reports whether s is displayable text of min to max characters: valid
// UTF-8 without control characters other than newline, carriage return and tab.
func textOK(s string, min, max int) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < min || n > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func truncateChars(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// userInputResponse is the reply Codex expects: the answers keyed by question
// id, each a list of strings. A denial answers nothing.
func userInputResponse(d decision) map[string]any {
	answers := map[string]any{}
	if d.Kind == "answer" {
		for id, texts := range d.Answer {
			answers[id] = map[string]any{"answers": texts}
		}
	}
	return map[string]any{"answers": answers}
}
