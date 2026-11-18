package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// maxSteerArgumentBytes is the adapter protocol's bound on request arguments
// (adapterkit/wire: 64 KiB).
const maxSteerArgumentBytes = 64 << 10

// An answer at the limit made of characters json.Marshal escapes must still fit
// the request's size bound once attached to the steer (ADR-0027).
func TestAnswerAtTheLimitFitsTheSteerBound(t *testing.T) {
	answer := lifecycle.Answer{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		answer[id] = []string{strings.Repeat("<", lifecycle.MaxAnswerTextChars-1)}
	}
	if err := answer.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	fields := map[string]json.RawMessage{"kind": json.RawMessage(`"answer"`), "decision_id": json.RawMessage(`"x"`)}
	body, err := withAnswer(fields, answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxSteerArgumentBytes {
		t.Fatalf("steer arguments are %d bytes, over the %d byte bound", len(body), maxSteerArgumentBytes)
	}
	if strings.Contains(string(body), `\u003c`) {
		t.Fatal("the answer was HTML-escaped")
	}
	var back struct {
		Answer lifecycle.Answer `json:"answer"`
	}
	if err := json.Unmarshal(body, &back); err != nil || back.Answer["a"][0] != answer["a"][0] {
		t.Fatalf("answer did not round-trip: %v", err)
	}
}
