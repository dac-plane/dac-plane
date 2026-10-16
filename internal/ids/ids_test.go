package ids

import (
	"encoding/json"
	"errors"
	"testing"
)

const canonical = "01928f6e-7c3a-7b1e-9f00-0123456789ab"

func TestNewReturnsParseableUUIDv7(t *testing.T) {
	id, err := New[Tenant]()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if id.IsZero() {
		t.Fatal("New() returned the zero identifier")
	}
	parsed, err := Parse[Tenant](id.String())
	if err != nil {
		t.Fatalf("Parse(New()) error = %v", err)
	}
	if parsed != id {
		t.Fatalf("Parse(New()) = %s, want %s", parsed, id)
	}
}

func TestParseRejectsNonCanonicalForms(t *testing.T) {
	for name, text := range map[string]string{
		"empty":     "",
		"nil":       "00000000-0000-0000-0000-000000000000",
		"version 4": "01928f6e-7c3a-4b1e-9f00-0123456789ab",
		"uppercase": "01928F6E-7C3A-7B1E-9F00-0123456789AB",
		"braces":    "{" + canonical + "}",
		"urn":       "urn:uuid:" + canonical,
		"no dashes": "01928f6e7c3a7b1e9f000123456789ab",
		"garbage":   "tenant-1",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse[Tenant](text); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalid", text, err)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	id, err := Parse[Attempt](canonical)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(encoded) != `"`+canonical+`"` {
		t.Fatalf("Marshal() = %s", encoded)
	}
	var decoded AttemptID
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded != id {
		t.Fatalf("Unmarshal() = %s, want %s", decoded, id)
	}
}

func TestZeroIdentifierDoesNotMarshal(t *testing.T) {
	if _, err := json.Marshal(TenantID{}); err == nil {
		t.Fatal("Marshal(zero) error = nil, want error")
	}
}

func TestUnmarshalRejectsInvalidText(t *testing.T) {
	var id TenantID
	if err := json.Unmarshal([]byte(`"not-an-id"`), &id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Unmarshal() error = %v, want ErrInvalid", err)
	}
	if !id.IsZero() {
		t.Fatal("failed Unmarshal() modified the identifier")
	}
}
