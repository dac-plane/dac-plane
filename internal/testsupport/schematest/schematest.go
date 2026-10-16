// Package schematest checks that JSON Schema contracts and the Go types that
// implement them accept and reject the same documents.
package schematest

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Contract pairs a JSON Schema file with Go decoding and validation of the
// same record, so both can be asked whether a document is valid.
type Contract struct {
	Schema   string
	GoAccept func(data []byte) error
}

// GoContract decodes strictly into T, rejecting unknown fields, and validates.
func GoContract[T interface{ Validate() error }](schemaPath string) Contract {
	return Contract{Schema: schemaPath, GoAccept: func(data []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var value T
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		return value.Validate()
	}}
}

// Compile compiles a JSON Schema 2020-12 file with format assertions enabled.
func Compile(t testing.TB, path string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	schema, err := compiler.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	return schema
}

// Accepts validates a JSON document against schema.
func Accepts(t testing.TB, schema *jsonschema.Schema, data []byte) error {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	return schema.Validate(instance)
}

// Marshal encodes value as JSON.
func Marshal(t testing.TB, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%T) error = %v", value, err)
	}
	return data
}

// Mutate decodes a JSON object, applies change and re-encodes it.
func Mutate(t testing.TB, data []byte, change func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	change(document)
	return Marshal(t, document)
}

// Enum returns the string enum at keyPath inside the schema file.
func Enum(t testing.TB, path string, keyPath ...string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var node any
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	for _, key := range keyPath {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object", path, key)
		}
		node = object[key]
	}
	values, ok := node.([]any)
	if !ok {
		t.Fatalf("%s: %v is not an enum array", path, keyPath)
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i], _ = value.(string)
	}
	return result
}

// Strings converts a slice of string-kinded constants.
func Strings[S ~string](values []S) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
