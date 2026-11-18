package lifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/testsupport/schematest"
)

func testQuestion() Question {
	return Question{Prompts: []Prompt{
		{ID: "scope", Header: "Scope", Text: "Which tests should run?", Options: []PromptOption{{Label: "unit"}, {Label: "all", Description: "Unit and integration"}}},
		{ID: "why", Text: "Why does it matter?"},
	}}
}

func testAnswer() Answer { return Answer{"scope": {"unit"}, "why": {"Faster feedback"}} }

func TestQuestionValidate(t *testing.T) {
	if err := testQuestion().Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	tooMany := Question{}
	for i := range MaxPrompts + 1 {
		tooMany.Prompts = append(tooMany.Prompts, Prompt{ID: string(rune('a' + i)), Text: "x"})
	}
	for name, q := range map[string]Question{
		"no prompts":            {},
		"too many prompts":      tooMany,
		"duplicate id":          {Prompts: []Prompt{{ID: "a", Text: "x"}, {ID: "a", Text: "y"}}},
		"empty text":            {Prompts: []Prompt{{ID: "a"}}},
		"long text":             {Prompts: []Prompt{{ID: "a", Text: strings.Repeat("x", MaxPromptTextChars+1)}}},
		"control character":     {Prompts: []Prompt{{ID: "a", Text: "x\x07"}}},
		"invalid utf-8":         {Prompts: []Prompt{{ID: "a", Text: "x\xff"}}},
		"other without options": {Prompts: []Prompt{{ID: "a", Text: "x", Other: true}}},
		"duplicate label":       {Prompts: []Prompt{{ID: "a", Text: "x", Options: []PromptOption{{Label: "l"}, {Label: "l"}}}}},
		"empty label":           {Prompts: []Prompt{{ID: "a", Text: "x", Options: []PromptOption{{}}}}},
		"too large in total": {Prompts: func() []Prompt {
			var ps []Prompt
			for i := range MaxPrompts {
				ps = append(ps, Prompt{ID: string(rune('a' + i)), Text: strings.Repeat("x", MaxPromptTextChars)})
			}
			return ps
		}()},
	} {
		if err := q.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAnswerCheckAgainst(t *testing.T) {
	q := testQuestion()
	if err := testAnswer().CheckAgainst(q); err != nil {
		t.Fatalf("CheckAgainst() error = %v", err)
	}
	open := Question{Prompts: []Prompt{{ID: "scope", Text: "x", Options: []PromptOption{{Label: "unit"}}, Other: true}}}
	if err := (Answer{"scope": {"something else"}}).CheckAgainst(open); err != nil {
		t.Errorf("other answer: CheckAgainst() error = %v", err)
	}
	for name, a := range map[string]Answer{
		"missing prompt":   {"scope": {"unit"}},
		"unknown prompt":   {"scope": {"unit"}, "why": {"x"}, "extra": {"y"}},
		"not an option":    {"scope": {"none"}, "why": {"x"}},
		"two strings":      {"scope": {"unit", "all"}, "why": {"x"}},
		"empty answer":     {},
		"control char":     {"scope": {"unit"}, "why": {"x\x00"}},
		"too long a reply": {"scope": {"unit"}, "why": {strings.Repeat("x", MaxAnswerTextChars+1)}},
	} {
		if err := a.CheckAgainst(q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CheckAgainst() error = %v, want ErrInvalid", name, err)
		}
	}
}

// TestAnswerDigestIsRFC8785 pins the canonical form with vectors a browser
// client reproduces with JSON.stringify over sorted keys: keys ordered by UTF-16
// code units, no whitespace, minimal escaping, non-ASCII text unescaped.
func TestAnswerDigestIsRFC8785(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer Answer
		want   string
	}{
		{"sorted keys", Answer{"b": {"2"}, "a": {"1"}}, `{"a":["1"],"b":["2"]}`},
		{"escapes", Answer{"a": {"q\"\\\n\r\t"}}, `{"a":["q\"\\\n\r\t"]}`},
		{"no html escaping", Answer{"a": {"<b>&"}}, `{"a":["<b>&"]}`},
		{"non-ascii", Answer{"a": {"héllo  "}}, "{\"a\":[\"héllo  \"]}"},
		// U+FF5E sorts after U+10000 by UTF-16 code units (0xFF5E > 0xD800), but
		// before it by code point; RFC 8785 uses code units.
		{"utf-16 order", Answer{"～": {"x"}, "\U00010000": {"y"}}, "{\"\U00010000\":[\"y\"],\"～\":[\"x\"]}"},
	} {
		got, err := tc.answer.Digest()
		if err != nil {
			t.Fatalf("%s: Digest() error = %v", tc.name, err)
		}
		if want := identity.DigestOf([]byte(tc.want)); got != want {
			t.Errorf("%s: digest = %s, want digest of %s", tc.name, got, tc.want)
		}
	}
}

func TestAnswerVerifyDigest(t *testing.T) {
	a := testAnswer()
	digest, err := a.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	if err := a.VerifyDigest(digest); err != nil {
		t.Errorf("VerifyDigest() error = %v", err)
	}
	if err := (Answer{"scope": {"all"}, "why": {"Faster feedback"}}).VerifyDigest(digest); !errors.Is(err, ErrAnswerDigest) {
		t.Errorf("VerifyDigest() of another answer error = %v, want ErrAnswerDigest", err)
	}
}

// TestSchemaAndGoCountTextTheSame pins that a limit means characters in both:
// 1025 and 2048 two-byte characters are within the 2048-character bound in the
// schema and in Go alike, and one more is outside it in both.
func TestSchemaAndGoCountTextTheSame(t *testing.T) {
	question := func(n int) []byte {
		return schematest.Marshal(t, Question{Prompts: []Prompt{{ID: "a", Text: strings.Repeat("\u00e9", n)}}})
	}
	answer := func(n int) []byte {
		return schematest.Marshal(t, Answer{"a": {strings.Repeat("\u00e9", n)}})
	}
	for _, tc := range []struct {
		name     string
		contract schematest.Contract
		data     []byte
		valid    bool
	}{
		{"question at the limit", questionContract, question(MaxPromptTextChars), true},
		{"question over the limit", questionContract, question(MaxPromptTextChars + 1), false},
		{"answer at the limit", answerContract, answer(MaxAnswerTextChars), true},
		{"answer over the limit", answerContract, answer(MaxAnswerTextChars + 1), false},
	} {
		schemaErr := schematest.Accepts(t, schematest.Compile(t, tc.contract.Schema), tc.data)
		goErr := tc.contract.GoAccept(tc.data)
		if (schemaErr == nil) != tc.valid || (goErr == nil) != tc.valid {
			t.Errorf("%s: schema error = %v, Go error = %v, want valid = %v", tc.name, schemaErr, goErr, tc.valid)
		}
	}
}

func TestAnswerRejectsNull(t *testing.T) {
	for _, data := range []string{`{"a":[null]}`, `null`, `{"a":null}`} {
		if err := answerContract.GoAccept([]byte(data)); err == nil {
			t.Errorf("Go accepted %s", data)
		}
		if schematest.Accepts(t, schematest.Compile(t, answerContract.Schema), []byte(data)) == nil {
			t.Errorf("schema accepted %s", data)
		}
	}
}
