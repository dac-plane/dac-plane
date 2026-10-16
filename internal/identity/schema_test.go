package identity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaDir = "../../contracts/jsonschema/identity"

// contract pairs a JSON Schema with Go decoding and validation of the same
// record, so both can be asked whether a document is valid.
type contract struct {
	schema   string
	goAccept func(data []byte) error
}

func goContract[T interface{ Validate() error }](schema string) contract {
	return contract{schema: schema, goAccept: func(data []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var value T
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		return value.Validate()
	}}
}

var (
	principalContract = goContract[Principal]("principal.schema.json")
	requestContract   = goContract[ActionRequest]("action-request.schema.json")
	policyContract    = goContract[Policy]("policy.schema.json")
	grantContract     = goContract[AuthorityGrant]("authority-grant.schema.json")
	useContract       = goContract[AuthorityUse]("authority-use.schema.json")
)

func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	schema, err := compiler.Compile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return schema
}

func schemaAccepts(t *testing.T, schema *jsonschema.Schema, data []byte) error {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	return schema.Validate(instance)
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%T) error = %v", value, err)
	}
	return data
}

// mutate decodes a document, applies change and re-encodes it.
func mutate(t *testing.T, data []byte, change func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	change(document)
	return marshal(t, document)
}

func validRecords(t *testing.T) map[string]struct {
	contract contract
	data     []byte
} {
	f := newFixture(t)
	f.policy.Denies = []DenyPolicy{{Action: "fs.delete", Scope: Scope{Class: ScopeGlobal}}}
	allowed, retired, err := Authorize(f.request, &f.allowOnce, f.policy, testNow)
	if err != nil || retired == nil {
		t.Fatalf("Authorize() = %v, %v", retired, err)
	}
	denied, _, err := Authorize(f.request, nil, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	revoked := f.standing
	revoked.RevokedAt = testNow

	return map[string]struct {
		contract contract
		data     []byte
	}{
		"principal":        {principalContract, marshal(t, testPrincipal(t))},
		"action request":   {requestContract, marshal(t, f.request)},
		"policy":           {policyContract, marshal(t, f.policy)},
		"empty policy":     {policyContract, marshal(t, Policy{Tenant: f.policy.Tenant, Generation: 1})},
		"standing grant":   {grantContract, marshal(t, f.standing)},
		"revoked grant":    {grantContract, marshal(t, revoked)},
		"allow-once grant": {grantContract, marshal(t, f.allowOnce)},
		"retired grant":    {grantContract, marshal(t, *retired)},
		"allowed use":      {useContract, marshal(t, allowed)},
		"denied use":       {useContract, marshal(t, denied)},
		"denied grant use": {useContract, marshal(t, deniedWithGrant(t, f))},
	}
}

func deniedWithGrant(t *testing.T, f fixture) AuthorityUse {
	t.Helper()
	f.request.Subject = "repo/other.go"
	use, _, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	return use
}

func TestSchemasAcceptValidGoRecords(t *testing.T) {
	for name, record := range validRecords(t) {
		t.Run(name, func(t *testing.T) {
			if err := record.contract.goAccept(record.data); err != nil {
				t.Fatalf("Go rejected its own record: %v", err)
			}
			schema := compileSchema(t, record.contract.schema)
			if err := schemaAccepts(t, schema, record.data); err != nil {
				t.Fatalf("schema rejected Go record %s: %v", record.data, err)
			}
		})
	}
}

func TestSchemaAndGoRejectTheSameInvalidDocuments(t *testing.T) {
	valid := validRecords(t)
	var allowOnce map[string]any
	if err := json.Unmarshal(valid["allow-once grant"].data, &allowOnce); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	cases := []struct {
		name   string
		base   string
		change func(map[string]any)
	}{
		{"principal missing tenant", "principal", func(d map[string]any) { delete(d, "tenant_id") }},
		{"principal unknown kind", "principal", func(d map[string]any) { d["kind"] = "provider" }},
		{"principal non-v7 id", "principal", func(d map[string]any) { d["principal_id"] = "01928f6e-7c3a-4b1e-9f00-0123456789ab" }},
		{"principal uppercase id", "principal", func(d map[string]any) { d["principal_id"] = strings.ToUpper(d["principal_id"].(string)) }},
		{"principal unknown field", "principal", func(d map[string]any) { d["password"] = "x" }},
		{"request missing operation", "action request", func(d map[string]any) { delete(d, "operation_id") }},
		{"request malformed action", "action request", func(d map[string]any) { d["action"] = "FS.Write" }},
		{"policy zero generation", "policy", func(d map[string]any) { d["generation"] = 0 }},
		{"policy unknown ceiling class", "policy", func(d map[string]any) { d["ceiling"] = map[string]any{"fs.write": "tenant"} }},
		{"policy malformed ceiling action", "policy", func(d map[string]any) { d["ceiling"] = map[string]any{"Fs": "global"} }},
		{"policy call-scoped deny", "policy", func(d map[string]any) {
			d["denies"] = []any{map[string]any{"action": "fs.write", "scope": map[string]any{"class": "call"}}}
		}},
		{"grant unknown mode", "standing grant", func(d map[string]any) { d["mode"] = "always" }},
		{"grant missing granting principal", "standing grant", func(d map[string]any) { delete(d, "granted_by") }},
		{"grant subject scope without value", "standing grant", func(d map[string]any) { d["scope"] = map[string]any{"class": "subject"} }},
		{"grant global scope with value", "standing grant", func(d map[string]any) { d["scope"] = map[string]any{"class": "global", "value": "x"} }},
		{"standing grant with operation", "standing grant", func(d map[string]any) { d["operation_id"] = allowOnce["operation_id"] }},
		{"standing grant retired", "standing grant", func(d map[string]any) { d["retired_at"] = testNow.Format(time.RFC3339) }},
		{"standing grant call scoped", "allow-once grant", func(d map[string]any) { d["mode"] = "standing" }},
		{"allow-once grant global scoped", "allow-once grant", func(d map[string]any) {
			d["scope"] = map[string]any{"class": "global"}
			delete(d, "operation_id")
		}},
		{"call grant without operation", "allow-once grant", func(d map[string]any) { delete(d, "operation_id") }},
		{"grant malformed timestamp", "standing grant", func(d map[string]any) { d["issued_at"] = "yesterday" }},
		{"allowed use with reason", "allowed use", func(d map[string]any) { d["reason"] = "expired" }},
		{"allowed use without grant", "allowed use", func(d map[string]any) {
			delete(d, "grant_id")
			delete(d, "source")
			delete(d, "granted_by")
		}},
		{"denied use without reason", "denied use", func(d map[string]any) { delete(d, "reason") }},
		{"denied use unknown reason", "denied use", func(d map[string]any) { d["reason"] = "nope" }},
		{"use grant without source", "denied grant use", func(d map[string]any) { delete(d, "source") }},
		{"use unknown outcome", "allowed use", func(d map[string]any) { d["outcome"] = "maybe" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := valid[tc.base]
			if !ok {
				t.Fatalf("unknown base record %q", tc.base)
			}
			data := mutate(t, base.data, tc.change)
			schema := compileSchema(t, base.contract.schema)
			if err := schemaAccepts(t, schema, data); err == nil {
				t.Errorf("schema accepted %s", data)
			}
			if err := base.contract.goAccept(data); err == nil {
				t.Errorf("Go accepted %s", data)
			}
		})
	}
}

func TestSchemaEnumsMatchGoConstants(t *testing.T) {
	for _, tc := range []struct {
		file string
		path []string
		want []string
	}{
		{"common.schema.json", []string{"$defs", "scopeClass", "enum"}, strs(ScopeClasses())},
		{"principal.schema.json", []string{"properties", "kind", "enum"}, strs(PrincipalKinds())},
		{"authority-grant.schema.json", []string{"properties", "mode", "enum"}, strs(GrantModes())},
		{"authority-use.schema.json", []string{"properties", "source", "enum"}, strs(AuthoritySources())},
		{"authority-use.schema.json", []string{"properties", "outcome", "enum"}, strs(UseOutcomes())},
		{"authority-use.schema.json", []string{"properties", "reason", "enum"}, strs(DenialReasons())},
	} {
		t.Run(tc.file+"#"+strings.Join(tc.path, "/"), func(t *testing.T) {
			got := schemaEnum(t, tc.file, tc.path)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("schema enum = %v, Go constants = %v", got, tc.want)
			}
		})
	}
}

func TestRecordsCarryNoCredentialFields(t *testing.T) {
	forbidden := []string{"secret", "password", "token", "credential", "api_key", "private_key"}
	for name, record := range validRecords(t) {
		lower := strings.ToLower(string(record.data))
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("%s record contains %q: %s", name, word, record.data)
			}
		}
	}
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(schemaDir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		lower := strings.ToLower(string(data))
		for _, word := range forbidden {
			if strings.Contains(lower, `"`+word) {
				t.Errorf("schema %s declares a %q property", entry.Name(), word)
			}
		}
	}
}

func schemaEnum(t *testing.T, file string, path []string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(schemaDir, file))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var node any
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", file, err)
	}
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object", file, key)
		}
		node = object[key]
	}
	values, ok := node.([]any)
	if !ok {
		t.Fatalf("%s: %v is not an enum array", file, path)
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i], _ = value.(string)
	}
	return result
}

func strs[S ~string](values []S) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
