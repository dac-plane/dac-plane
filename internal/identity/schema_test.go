package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

const schemaDir = "../../contracts/jsonschema/identity"

var (
	principalContract = schematest.GoContract[Principal](filepath.Join(schemaDir, "principal.schema.json"))
	requestContract   = schematest.GoContract[ActionRequest](filepath.Join(schemaDir, "action-request.schema.json"))
	policyContract    = schematest.GoContract[Policy](filepath.Join(schemaDir, "policy.schema.json"))
	grantContract     = schematest.GoContract[AuthorityGrant](filepath.Join(schemaDir, "authority-grant.schema.json"))
	useContract       = schematest.GoContract[AuthorityUse](filepath.Join(schemaDir, "authority-use.schema.json"))
	bindingContract   = schematest.GoContract[IdentityBinding](filepath.Join(schemaDir, "identity-binding.schema.json"))
	readerContract    = schematest.GoContract[TenantReader](filepath.Join(schemaDir, "tenant-reader.schema.json"))
)

func validRecords(t *testing.T) map[string]struct {
	contract schematest.Contract
	data     []byte
} {
	f := newFixture(t)
	f.policy.Denies = []DenyPolicy{{Action: "message.send", Target: "bob@example.com"}}
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
		contract schematest.Contract
		data     []byte
	}{
		"principal":        {principalContract, schematest.Marshal(t, testPrincipal(t))},
		"action request":   {requestContract, schematest.Marshal(t, f.request)},
		"policy":           {policyContract, schematest.Marshal(t, f.policy)},
		"empty policy":     {policyContract, schematest.Marshal(t, Policy{Tenant: f.policy.Tenant, Generation: 1})},
		"standing grant":   {grantContract, schematest.Marshal(t, f.standing)},
		"revoked grant":    {grantContract, schematest.Marshal(t, revoked)},
		"allow-once grant": {grantContract, schematest.Marshal(t, f.allowOnce)},
		"retired grant":    {grantContract, schematest.Marshal(t, *retired)},
		"allowed use":      {useContract, schematest.Marshal(t, allowed)},
		"denied use":       {useContract, schematest.Marshal(t, denied)},
		"denied grant use": {useContract, schematest.Marshal(t, deniedWithGrant(t, f))},
		"identity binding": {bindingContract, schematest.Marshal(t, testBinding(t))},
		"disabled binding": {bindingContract, schematest.Marshal(t, disabledBinding(t))},
		"tenant reader":    {readerContract, schematest.Marshal(t, testReader(t))},
	}
}

func deniedWithGrant(t *testing.T, f fixture) AuthorityUse {
	t.Helper()
	f.request.Scope.Target = "carol@example.com"
	use, _, err := Authorize(f.request, &f.standing, f.policy, testNow)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	return use
}

func TestSchemasAcceptValidGoRecords(t *testing.T) {
	for name, record := range validRecords(t) {
		t.Run(name, func(t *testing.T) {
			if err := record.contract.GoAccept(record.data); err != nil {
				t.Fatalf("Go rejected its own record: %v", err)
			}
			schema := schematest.Compile(t, record.contract.Schema)
			if err := schematest.Accepts(t, schema, record.data); err != nil {
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
		{"request missing digest", "action request", func(d map[string]any) { delete(d, "input_digest") }},
		{"request malformed digest", "action request", func(d map[string]any) { d["input_digest"] = "md5:abc" }},
		{"request malformed action", "action request", func(d map[string]any) { d["action"] = "Message.Send" }},
		{"request subject scope without target", "action request", func(d map[string]any) { d["scope"] = map[string]any{"class": "subject"} }},
		{"policy zero generation", "policy", func(d map[string]any) { d["generation"] = 0 }},
		{"policy unknown permission", "policy", func(d map[string]any) { d["ceiling"] = map[string]any{"message.send": "sometimes"} }},
		{"policy malformed settings action", "policy", func(d map[string]any) { d["settings"] = map[string]any{"Msg": "ask"} }},
		{"policy deny without target", "policy", func(d map[string]any) {
			d["denies"] = []any{map[string]any{"action": "message.send"}}
		}},
		{"grant unknown mode", "standing grant", func(d map[string]any) { d["mode"] = "always" }},
		{"grant missing granting principal", "standing grant", func(d map[string]any) { delete(d, "granted_by") }},
		{"grant malformed timestamp", "standing grant", func(d map[string]any) { d["issued_at"] = "yesterday" }},
		{"standing grant without scope", "standing grant", func(d map[string]any) { delete(d, "scope") }},
		{"standing grant call scoped", "standing grant", func(d map[string]any) { d["scope"] = map[string]any{"class": "call"} }},
		{"standing grant global scope with target", "standing grant", func(d map[string]any) {
			d["scope"] = map[string]any{"class": "global", "target": "x"}
		}},
		{"standing grant with operation", "standing grant", func(d map[string]any) { d["operation_id"] = allowOnce["operation_id"] }},
		{"standing grant with digest", "standing grant", func(d map[string]any) { d["input_digest"] = allowOnce["input_digest"] }},
		{"standing grant retired", "standing grant", func(d map[string]any) { d["retired_at"] = testNow.Format(time.RFC3339) }},
		{"allow-once grant without operation", "allow-once grant", func(d map[string]any) { delete(d, "operation_id") }},
		{"allow-once grant without digest", "allow-once grant", func(d map[string]any) { delete(d, "input_digest") }},
		{"allow-once grant with scope", "allow-once grant", func(d map[string]any) { d["scope"] = map[string]any{"class": "global"} }},
		{"allowed use with reason", "allowed use", func(d map[string]any) { d["reason"] = "expired" }},
		{"allowed use without grant", "allowed use", func(d map[string]any) {
			delete(d, "grant_id")
			delete(d, "source")
			delete(d, "granted_by")
		}},
		{"use missing digest", "allowed use", func(d map[string]any) { delete(d, "input_digest") }},
		{"use malformed scope", "allowed use", func(d map[string]any) { d["scope"] = map[string]any{"class": "unbounded"} }},
		{"denied use without reason", "denied use", func(d map[string]any) { delete(d, "reason") }},
		{"denied use unknown reason", "denied use", func(d map[string]any) { d["reason"] = "nope" }},
		{"use grant without source", "denied grant use", func(d map[string]any) { delete(d, "source") }},
		{"use unknown outcome", "allowed use", func(d map[string]any) { d["outcome"] = "maybe" }},
		{"binding missing tenant", "identity binding", func(d map[string]any) { delete(d, "tenant_id") }},
		{"binding missing principal", "identity binding", func(d map[string]any) { delete(d, "principal_id") }},
		{"binding missing update time", "identity binding", func(d map[string]any) { delete(d, "updated_at") }},
		{"binding http issuer", "identity binding", func(d map[string]any) { d["issuer"] = "http://issuer.example" }},
		{"binding issuer with query", "identity binding", func(d map[string]any) { d["issuer"] = "https://issuer.example/?tenant=a" }},
		{"binding issuer with fragment", "identity binding", func(d map[string]any) { d["issuer"] = "https://issuer.example/#x" }},
		{"binding issuer with user", "identity binding", func(d map[string]any) { d["issuer"] = "https://user@issuer.example" }},
		{"binding issuer without host", "identity binding", func(d map[string]any) { d["issuer"] = "https:///path" }},
		{"binding issuer with space", "identity binding", func(d map[string]any) { d["issuer"] = "https://issuer.example/a b" }},
		{"binding issuer too long", "identity binding", func(d map[string]any) { d["issuer"] = "https://issuer.example/" + strings.Repeat("a", 2048) }},
		{"binding empty subject", "identity binding", func(d map[string]any) { d["subject"] = "" }},
		{"binding subject with space", "identity binding", func(d map[string]any) { d["subject"] = "user 1" }},
		{"binding non-ASCII subject", "identity binding", func(d map[string]any) { d["subject"] = "usér" }},
		{"binding subject too long", "identity binding", func(d map[string]any) { d["subject"] = strings.Repeat("s", 256) }},
		{"binding unknown field", "identity binding", func(d map[string]any) { d["email"] = "a@example.com" }},
		{"reader missing principal", "tenant reader", func(d map[string]any) { delete(d, "principal_id") }},
		{"reader missing update time", "tenant reader", func(d map[string]any) { delete(d, "updated_at") }},
		{"reader unknown field", "tenant reader", func(d map[string]any) { d["role"] = "admin" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, ok := valid[tc.base]
			if !ok {
				t.Fatalf("unknown base record %q", tc.base)
			}
			data := schematest.Mutate(t, base.data, tc.change)
			schema := schematest.Compile(t, base.contract.Schema)
			if err := schematest.Accepts(t, schema, data); err == nil {
				t.Errorf("schema accepted %s", data)
			}
			if err := base.contract.GoAccept(data); err == nil {
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
		{"common.schema.json", []string{"$defs", "scopeClass", "enum"}, schematest.Strings(ScopeClasses())},
		{"common.schema.json", []string{"$defs", "permission", "enum"}, schematest.Strings(Permissions())},
		{"principal.schema.json", []string{"properties", "kind", "enum"}, schematest.Strings(PrincipalKinds())},
		{"authority-grant.schema.json", []string{"properties", "mode", "enum"}, schematest.Strings(GrantModes())},
		{"authority-use.schema.json", []string{"properties", "source", "enum"}, schematest.Strings(AuthoritySources())},
		{"authority-use.schema.json", []string{"properties", "outcome", "enum"}, schematest.Strings(UseOutcomes())},
		{"authority-use.schema.json", []string{"properties", "reason", "enum"}, schematest.Strings(DenialReasons())},
	} {
		t.Run(tc.file+"#"+strings.Join(tc.path, "/"), func(t *testing.T) {
			got := schematest.Enum(t, filepath.Join(schemaDir, tc.file), tc.path...)
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

func testBinding(t *testing.T) IdentityBinding {
	t.Helper()
	principal := testPrincipal(t)
	return IdentityBinding{
		Tenant: principal.Tenant, Principal: principal.ID,
		Issuer: "https://issuer.example/realms/a", Subject: "248289761001",
		Enabled: true, UpdatedAt: testNow,
	}
}

func disabledBinding(t *testing.T) IdentityBinding {
	t.Helper()
	binding := testBinding(t)
	binding.Enabled = false
	return binding
}

func testReader(t *testing.T) TenantReader {
	t.Helper()
	principal := testPrincipal(t)
	return TenantReader{Tenant: principal.Tenant, Principal: principal.ID, Enabled: true, UpdatedAt: testNow}
}
