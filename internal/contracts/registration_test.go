package contracts

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

var (
	registrationContract   = schematest.GoContract[ProviderRegistration](filepath.Join(schemaDir, "provider-registration.schema.json"))
	adapterBindingContract = schematest.GoContract[AdapterBinding](filepath.Join(schemaDir, "adapter-binding.schema.json"))
)

func registrationFixtures(t *testing.T) map[string]fixture {
	t.Helper()
	tenant, store := newID[ids.Tenant](t), newID[ids.Provider](t)
	runtime := ProviderRegistration{Tenant: tenant, Provider: newID[ids.Provider](t), Role: RoleRuntime, ArtifactStore: &store, UpdatedAt: testNow}
	environment := ProviderRegistration{Tenant: tenant, Provider: newID[ids.Provider](t), Role: RoleEnvironment, UpdatedAt: testNow}
	binding := AdapterBinding{Issuer: "https://issuer.example.com/realms/adapters", Subject: "adapter-1", Tenant: tenant, Provider: runtime.Provider, Enabled: true, UpdatedAt: testNow}
	disabled := binding
	disabled.Enabled = false
	marshal := func(v any) []byte { return schematest.Marshal(t, v) }
	return map[string]fixture{
		"runtime with store":       {registrationContract, marshal(runtime)},
		"environment":              {registrationContract, marshal(environment)},
		"adapter binding":          {adapterBindingContract, marshal(binding)},
		"disabled adapter binding": {adapterBindingContract, marshal(disabled)},
	}
}

func TestRegistrationGoFixturesMatchSchemas(t *testing.T) {
	for name, f := range registrationFixtures(t) {
		t.Run(name, func(t *testing.T) {
			if err := f.contract.GoAccept(f.data); err != nil {
				t.Fatalf("Go rejected its own fixture %s: %v", f.data, err)
			}
			if err := schematest.Accepts(t, schematest.Compile(t, f.contract.Schema), f.data); err != nil {
				t.Fatalf("schema rejected Go fixture %s: %v", f.data, err)
			}
		})
	}
}

func TestRegistrationInvalidDocuments(t *testing.T) {
	fixtures := registrationFixtures(t)
	other := newID[ids.Provider](t).String()
	cases := []struct {
		name   string
		base   string
		change func(map[string]any)
	}{
		{"registration unknown role", "environment", func(d map[string]any) { d["role"] = "scheduler" }},
		{"registration without provider", "environment", func(d map[string]any) { delete(d, "provider_id") }},
		{"registration without update time", "environment", func(d map[string]any) { delete(d, "updated_at") }},
		{"store on a non-runtime", "environment", func(d map[string]any) { d["artifact_store_id"] = other }},
		{"registration extra field", "environment", func(d map[string]any) { d["issuer"] = "https://issuer.example.com" }},
		{"binding without subject", "adapter binding", func(d map[string]any) { delete(d, "subject") }},
		{"binding http issuer", "adapter binding", func(d map[string]any) { d["issuer"] = "http://issuer.example.com" }},
		{"binding issuer with query", "adapter binding", func(d map[string]any) { d["issuer"] = "https://issuer.example.com/?a=b" }},
		{"binding subject with space", "adapter binding", func(d map[string]any) { d["subject"] = "adapter 1" }},
		{"binding without tenant", "adapter binding", func(d map[string]any) { delete(d, "tenant_id") }},
		{"binding with principal", "adapter binding", func(d map[string]any) { d["principal_id"] = d["provider_id"] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := fixtures[tc.base]
			data := schematest.Mutate(t, base.data, tc.change)
			if err := schematest.Accepts(t, schematest.Compile(t, base.contract.Schema), data); err == nil {
				t.Errorf("schema accepted %s", data)
			}
			if err := base.contract.GoAccept(data); err == nil {
				t.Errorf("Go accepted %s", data)
			}
		})
	}
}

func TestRegistrationRuntimeCannotExportToItself(t *testing.T) {
	provider := newID[ids.Provider](t)
	registration := ProviderRegistration{Tenant: newID[ids.Tenant](t), Provider: provider, Role: RoleRuntime, ArtifactStore: &provider, UpdatedAt: testNow}
	if err := registration.Validate(); err == nil {
		t.Fatal("Validate() accepted a runtime that names itself as its artifact store")
	}
}

func TestRegistrationRoleEnumCannotDrift(t *testing.T) {
	got := schematest.Enum(t, filepath.Join(schemaDir, "provider-registration.schema.json"), "properties", "role", "enum")
	if want := schematest.Strings(Roles()); !slices.Equal(got, want) {
		t.Fatalf("schema roles = %v, Go roles = %v", got, want)
	}
}
