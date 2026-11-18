package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dac-plane/dac-plane/internal/identity"
)

// Bounds on question and answer text (ADR-0026). A question is a short prompt to
// a person, not a document: an adapter that exceeds a bound has its request
// rejected rather than truncated. A *Chars bound counts Unicode code points, as
// JSON Schema's maxLength does, so the published schema and Go agree on
// non-ASCII text; the *Bytes bounds cap the whole question or answer.
const (
	MaxPrompts           = 8
	MaxPromptIDChars     = 64
	MaxPromptHeaderChars = 128
	MaxPromptTextChars   = 2048
	MaxPromptOptions     = 8
	MaxOptionLabelChars  = 256
	MaxOptionDescChars   = 512
	MaxQuestionBytes     = 16 * 1024
	MaxAnswerTextChars   = MaxPromptTextChars
	MaxAnswerBytes       = 16 * 1024
)

// PromptOption is one selectable answer to a prompt.
type PromptOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Prompt is one question put to a person. A prompt with options accepts one of
// their labels, or free text when Other is set; a prompt without options accepts
// free text.
type Prompt struct {
	ID      string         `json:"id"`
	Header  string         `json:"header,omitempty"`
	Text    string         `json:"text"`
	Options []PromptOption `json:"options,omitempty"`
	Other   bool           `json:"other,omitempty"`
}

// Question is the ordered list of prompts a runtime asked. A prompt a runtime marks
// secret is never carried here: it is declined at the adapter (ADR-0026).
type Question struct {
	Prompts []Prompt `json:"prompts"`
}

// Validate reports whether the question is complete and within its bounds.
func (q Question) Validate() error {
	if len(q.Prompts) == 0 || len(q.Prompts) > MaxPrompts {
		return fmt.Errorf("%w: a question carries 1 to %d prompts", ErrInvalid, MaxPrompts)
	}
	seen := make(map[string]bool, len(q.Prompts))
	total := 0
	for _, p := range q.Prompts {
		if err := p.validate(); err != nil {
			return err
		}
		if seen[p.ID] {
			return fmt.Errorf("%w: duplicate prompt id", ErrInvalid)
		}
		seen[p.ID] = true
		total += len(p.ID) + len(p.Header) + len(p.Text)
		for _, o := range p.Options {
			total += len(o.Label) + len(o.Description)
		}
	}
	if total > MaxQuestionBytes {
		return fmt.Errorf("%w: a question is at most %d bytes", ErrInvalid, MaxQuestionBytes)
	}
	return nil
}

func (p Prompt) validate() error {
	if p.ID == "" || utf8.RuneCountInString(p.ID) > MaxPromptIDChars || !validText(p.ID) {
		return fmt.Errorf("%w: a prompt id is 1 to %d bytes of text", ErrInvalid, MaxPromptIDChars)
	}
	if utf8.RuneCountInString(p.Header) > MaxPromptHeaderChars || !validText(p.Header) {
		return fmt.Errorf("%w: a prompt header is at most %d bytes of text", ErrInvalid, MaxPromptHeaderChars)
	}
	if p.Text == "" || utf8.RuneCountInString(p.Text) > MaxPromptTextChars || !validText(p.Text) {
		return fmt.Errorf("%w: a prompt text is 1 to %d bytes of text", ErrInvalid, MaxPromptTextChars)
	}
	if len(p.Options) > MaxPromptOptions {
		return fmt.Errorf("%w: a prompt has at most %d options", ErrInvalid, MaxPromptOptions)
	}
	if p.Other && len(p.Options) == 0 {
		return fmt.Errorf("%w: only a prompt with options can accept another answer", ErrInvalid)
	}
	labels := make(map[string]bool, len(p.Options))
	for _, o := range p.Options {
		if o.Label == "" || utf8.RuneCountInString(o.Label) > MaxOptionLabelChars || !validText(o.Label) ||
			utf8.RuneCountInString(o.Description) > MaxOptionDescChars || !validText(o.Description) {
			return fmt.Errorf("%w: an option has a label and an optional description within bounds", ErrInvalid)
		}
		if labels[o.Label] {
			return fmt.Errorf("%w: duplicate option label", ErrInvalid)
		}
		labels[o.Label] = true
	}
	return nil
}

// validText reports whether s is valid UTF-8 without control characters other
// than newline, carriage return and tab, so a question or answer is displayable
// text.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

// Answer maps a prompt id to the person's answer, one string per prompt.
type Answer map[string][]string

// UnmarshalJSON decodes an answer and rejects a null answer or a null element,
// which the default decoder would turn into an empty string and so into a
// different digest than the schema's non-string value was meant to carry.
func (a *Answer) UnmarshalJSON(data []byte) error {
	var raw map[string][]*string
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return fmt.Errorf("%w: an answer is an object of prompt id to strings", ErrInvalid)
	}
	out := make(Answer, len(raw))
	for id, texts := range raw {
		out[id] = make([]string, len(texts))
		for i, text := range texts {
			if text == nil {
				return fmt.Errorf("%w: an answer is made of strings, never null", ErrInvalid)
			}
			out[id][i] = *text
		}
	}
	*a = out
	return nil
}

// Validate reports whether the answer is well formed and within its bounds. Whether
// it fits a particular question is CheckAgainst's concern.
func (a Answer) Validate() error {
	if len(a) == 0 || len(a) > MaxPrompts {
		return fmt.Errorf("%w: an answer carries 1 to %d prompt answers", ErrInvalid, MaxPrompts)
	}
	total := 0
	for id, texts := range a {
		if id == "" || utf8.RuneCountInString(id) > MaxPromptIDChars || !validText(id) {
			return fmt.Errorf("%w: an answer names a prompt by its id", ErrInvalid)
		}
		if len(texts) != 1 || utf8.RuneCountInString(texts[0]) > MaxAnswerTextChars || !validText(texts[0]) {
			return fmt.Errorf("%w: each prompt takes one answer of at most %d bytes of text", ErrInvalid, MaxAnswerTextChars)
		}
		total += len(id) + len(texts[0])
	}
	if total > MaxAnswerBytes {
		return fmt.Errorf("%w: an answer is at most %d bytes", ErrInvalid, MaxAnswerBytes)
	}
	return nil
}

// CheckAgainst reports whether the answer answers exactly the question's
// prompts: every prompt once, no unknown id, and a prompt with options takes one
// of its labels unless it accepts another answer.
func (a Answer) CheckAgainst(q Question) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if len(a) != len(q.Prompts) {
		return fmt.Errorf("%w: an answer addresses every prompt of its question", ErrInvalid)
	}
	for _, p := range q.Prompts {
		texts, ok := a[p.ID]
		if !ok {
			return fmt.Errorf("%w: an answer addresses every prompt of its question", ErrInvalid)
		}
		if len(p.Options) == 0 || p.Other {
			continue
		}
		if !slices.ContainsFunc(p.Options, func(o PromptOption) bool { return o.Label == texts[0] }) {
			return fmt.Errorf("%w: an answer to a prompt with options is one of its labels", ErrInvalid)
		}
	}
	return nil
}

// Digest returns the SHA-256 digest of the answer in the canonical form of
// RFC 8785, so any client computes the same digest from the same answer.
func (a Answer) Digest() (identity.Digest, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	return identity.DigestOf(a.canonical()), nil
}

// canonical writes the answer as RFC 8785 JSON: keys sorted by UTF-16 code
// units, no insignificant whitespace, and strings escaped as ECMAScript's
// JSON.stringify writes them. An answer holds only strings, and validText keeps
// every control character but newline, carriage return and tab out of them, so
// no other escape and no number formatting is needed.
func (a Answer) canonical() []byte {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int {
		return slices.Compare(utf16.Encode([]rune(x)), utf16.Encode([]rune(y)))
	})
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(&b, k)
		b.WriteString(":[")
		writeJSONString(&b, a[k][0])
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return []byte(b.String())
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// ErrAnswerDigest reports an answer whose text does not match its digest.
var ErrAnswerDigest = errors.New("answer does not match its digest")

// VerifyDigest reports whether digest is the digest of the answer.
func (a Answer) VerifyDigest(digest identity.Digest) error {
	got, err := a.Digest()
	if err != nil {
		return err
	}
	if got != digest {
		return ErrAnswerDigest
	}
	return nil
}
