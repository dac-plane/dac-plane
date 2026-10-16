package schematest

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	contractsDir = "../../../contracts/jsonschema"
	internalDir  = "../.."
)

// TestEveryContractSchemaIsValidAndCovered guards the contract directory as a
// whole: every schema must compile against its declared metaschema, and every
// record schema must be exercised by a Go agreement test. A schema with no
// root type only holds shared definitions and is covered through the records
// that reference it.
func TestEveryContractSchemaIsValidAndCovered(t *testing.T) {
	testSources := readTestSources(t)
	var schemas int
	err := filepath.WalkDir(contractsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return err
		}
		schemas++
		t.Run(strings.TrimPrefix(path, contractsDir+"/"), func(t *testing.T) {
			document := readJSON(t, path)
			if document["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatalf("$schema = %v, want JSON Schema 2020-12", document["$schema"])
			}
			Compile(t, path)
			if _, isRecord := document["type"]; isRecord && !strings.Contains(testSources, entry.Name()) {
				t.Fatalf("no Go agreement test references %s", entry.Name())
			}
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", contractsDir, err)
	}
	if schemas == 0 {
		t.Fatalf("no schemas found under %s", contractsDir)
	}
}

// readTestSources concatenates the Go test files outside this package, where
// agreement tests name the schemas they check.
func readTestSources(t *testing.T) string {
	t.Helper()
	var sources strings.Builder
	err := filepath.WalkDir(internalDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") || filepath.Base(filepath.Dir(path)) == "schematest" {
			return err
		}
		data, err := os.ReadFile(path)
		sources.Write(data)
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", internalDir, err)
	}
	return sources.String()
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("%s is not a JSON object: %v", path, err)
	}
	return document
}
