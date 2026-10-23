package schematest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// contractPackages hold the Go types that JSON Schema contracts mirror.
var contractPackages = []string{"../../identity", "../../lifecycle", "../../contracts", "../../authn"}

// nestedRecords are validated structs that never travel alone: each is a
// nested object inside a record schema, covered by that record's agreement
// test. Listing one here names the schema definition that covers it.
var nestedRecords = map[string]string{
	"Scope":       "identity/common.schema.json#/$defs/scope",
	"DenyPolicy":  "identity/policy.schema.json#/properties/denies",
	"Actor":       "lifecycle/audit-event.schema.json#/$defs/actor",
	"TurnFailure": "lifecycle/turn-end.schema.json#/properties/failure",
	"SpendTerms":  "lifecycle/common.schema.json#/$defs/spendTerms",
	"Failure":     "adapter/common.schema.json#/$defs/failure",
	"Lifecycle":   "adapter/frame.schema.json#/$defs/lifecycle",
	"Request":     "adapter/frame.schema.json#/$defs/request",
	"Cancel":      "adapter/frame.schema.json#/$defs/cancel",
	"Reply":       "adapter/frame.schema.json#/$defs/reply",
	"Entry":       "adapter/stream-frame.schema.json#/$defs/entry",
	"Tombstone":   "adapter/stream-frame.schema.json#/$defs/tombstone",
}

var goContractPattern = regexp.MustCompile(`GoContract\[(\w+)\]`)

// TestEveryValidatedRecordHasASchema is the reverse of the schema coverage
// test: every exported struct with a Validate method in a contract package is
// either agreement-tested through GoContract or a listed nested record. A new
// record cannot ship without a language-neutral contract.
func TestEveryValidatedRecordHasASchema(t *testing.T) {
	tested := map[string]bool{}
	for _, match := range goContractPattern.FindAllStringSubmatch(readTestSources(t), -1) {
		tested[match[1]] = true
	}
	var records []string
	for _, dir := range contractPackages {
		records = append(records, validatedStructs(t, dir)...)
	}
	if len(records) == 0 {
		t.Fatal("no validated records found; the package list is stale")
	}
	for _, record := range records {
		if !tested[record] && nestedRecords[record] == "" {
			t.Errorf("%s has Validate() but no GoContract agreement test or nested-record entry", record)
		}
	}
	for record := range nestedRecords {
		if !slices.Contains(records, record) {
			t.Errorf("nested record %s no longer exists; remove it from the list", record)
		}
	}
}

// validatedStructs returns exported struct types in dir's non-test files that
// have a Validate method.
func validatedStructs(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	structs := map[string]bool{}
	validated := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
						if _, isStruct := ts.Type.(*ast.StructType); isStruct {
							structs[ts.Name.Name] = true
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name.Name == "Validate" && d.Recv != nil && len(d.Recv.List) == 1 {
					if ident, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
						validated[ident.Name] = true
					}
				}
			}
		}
	}
	var result []string
	for name := range structs {
		if validated[name] {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	return result
}
