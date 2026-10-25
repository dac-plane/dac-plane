package authn

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

const configSchema = "../../contracts/jsonschema/config/identity-config.schema.json"

var configContract = schematest.GoContract[Config](configSchema)

const minimalConfig = `{"issuers":[{"issuer":"https://issuer.example","audience":"https://api.example","algorithms":["RS256"],"max_token_lifetime_seconds":900}]}`

const fullConfig = `{"issuers":[
 {"issuer":"https://issuer.example/realms/a","audience":"dac-plane","algorithms":["ES256","RS256"],"max_token_lifetime_seconds":3600,"clock_skew_seconds":0,"require_access_token_type":false},
 {"issuer":"https://other.example","audience":"dac-plane","algorithms":["EdDSA"],"max_token_lifetime_seconds":60,"clock_skew_seconds":300,"require_access_token_type":true}],
 "adapter_issuers":[
 {"issuer":"https://issuer.example/realms/a","audience":"dac-plane-adapters","algorithms":["ES256"],"max_token_lifetime_seconds":900,"development":true}]}`

func TestConfigSchemasAcceptValidGoDocuments(t *testing.T) {
	schema := schematest.Compile(t, configSchema)
	for name, document := range map[string]string{"minimal": minimalConfig, "full": fullConfig} {
		t.Run(name, func(t *testing.T) {
			if err := configContract.GoAccept([]byte(document)); err != nil {
				t.Fatalf("Go rejected %s: %v", name, err)
			}
			if err := schematest.Accepts(t, schema, []byte(document)); err != nil {
				t.Fatalf("schema rejected %s: %v", name, err)
			}
		})
	}
}

func TestConfigSchemaAndGoRejectTheSameInvalidDocuments(t *testing.T) {
	issuer := func(change func(map[string]any)) func(map[string]any) {
		return func(d map[string]any) { change(d["issuers"].([]any)[0].(map[string]any)) }
	}
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing issuers", func(d map[string]any) { delete(d, "issuers") }},
		{"empty issuers", func(d map[string]any) { d["issuers"] = []any{} }},
		{"unknown top-level field", func(d map[string]any) { d["debug"] = true }},
		{"http issuer", issuer(func(i map[string]any) { i["issuer"] = "http://issuer.example" })},
		{"issuer with query", issuer(func(i map[string]any) { i["issuer"] = "https://issuer.example?x=1" })},
		{"missing audience", issuer(func(i map[string]any) { delete(i, "audience") })},
		{"empty audience", issuer(func(i map[string]any) { i["audience"] = "" })},
		{"audience with space", issuer(func(i map[string]any) { i["audience"] = "dac plane" })},
		{"missing algorithms", issuer(func(i map[string]any) { delete(i, "algorithms") })},
		{"empty algorithms", issuer(func(i map[string]any) { i["algorithms"] = []any{} })},
		{"symmetric algorithm", issuer(func(i map[string]any) { i["algorithms"] = []any{"HS256"} })},
		{"none algorithm", issuer(func(i map[string]any) { i["algorithms"] = []any{"none"} })},
		{"lowercase algorithm", issuer(func(i map[string]any) { i["algorithms"] = []any{"rs256"} })},
		{"repeated algorithm", issuer(func(i map[string]any) { i["algorithms"] = []any{"RS256", "RS256"} })},
		{"missing lifetime", issuer(func(i map[string]any) { delete(i, "max_token_lifetime_seconds") })},
		{"zero lifetime", issuer(func(i map[string]any) { i["max_token_lifetime_seconds"] = 0 })},
		{"lifetime above a day", issuer(func(i map[string]any) { i["max_token_lifetime_seconds"] = 86401 })},
		{"negative skew", issuer(func(i map[string]any) { i["clock_skew_seconds"] = -1 })},
		{"skew above five minutes", issuer(func(i map[string]any) { i["clock_skew_seconds"] = 301 })},
		{"string typing flag", issuer(func(i map[string]any) { i["require_access_token_type"] = "no" })},
		{"string development flag", issuer(func(i map[string]any) { i["development"] = "yes" })},
		{"unknown issuer field", issuer(func(i map[string]any) { i["jwks_uri"] = "https://issuer.example/keys" })},
		{"adapter issuers not a list", func(d map[string]any) { d["adapter_issuers"] = map[string]any{} }},
		{"adapter issuer without audience", func(d map[string]any) {
			d["adapter_issuers"] = []any{map[string]any{"issuer": "https://adapters.example", "algorithms": []any{"ES256"}, "max_token_lifetime_seconds": 60}}
		}},
	}
	schema := schematest.Compile(t, configSchema)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := schematest.Mutate(t, []byte(minimalConfig), tc.change)
			if err := schematest.Accepts(t, schema, data); err == nil {
				t.Errorf("schema accepted %s", data)
			}
			if err := configContract.GoAccept(data); err == nil {
				t.Errorf("Go accepted %s", data)
			}
		})
	}
}

func TestConfigSchemaEnumsMatchGo(t *testing.T) {
	got := schematest.Enum(t, configSchema, "$defs", "issuer", "properties", "algorithms", "items", "enum")
	if !slices.Equal(got, SigningAlgorithms()) {
		t.Fatalf("schema algorithms = %v, Go = %v", got, SigningAlgorithms())
	}
}

func TestParseConfigRejectsDuplicateIssuersAndTrailingDocuments(t *testing.T) {
	duplicate := `{"issuers":[` +
		`{"issuer":"https://issuer.example","audience":"a","algorithms":["RS256"],"max_token_lifetime_seconds":60},` +
		`{"issuer":"https://issuer.example","audience":"b","algorithms":["RS256"],"max_token_lifetime_seconds":60}]}`
	if _, err := ParseConfig([]byte(duplicate)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ParseConfig(duplicate) error = %v", err)
	}
	duplicateAdapter := `{"issuers":[{"issuer":"https://issuer.example","audience":"a","algorithms":["RS256"],"max_token_lifetime_seconds":60}],"adapter_issuers":[` +
		`{"issuer":"https://adapters.example","audience":"b","algorithms":["RS256"],"max_token_lifetime_seconds":60},` +
		`{"issuer":"https://adapters.example","audience":"c","algorithms":["RS256"],"max_token_lifetime_seconds":60}]}`
	if _, err := ParseConfig([]byte(duplicateAdapter)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ParseConfig(duplicate adapter issuer) error = %v", err)
	}
	// One issuer and one audience for both would let a Surface API token
	// open an adapter connection.
	sharedAudience := `{"issuers":[{"issuer":"https://issuer.example","audience":"a","algorithms":["RS256"],"max_token_lifetime_seconds":60}],` +
		`"adapter_issuers":[{"issuer":"https://issuer.example","audience":"a","algorithms":["RS256"],"max_token_lifetime_seconds":60}]}`
	if _, err := ParseConfig([]byte(sharedAudience)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ParseConfig(shared audience) error = %v", err)
	}
	for name, document := range map[string]string{
		"two documents": minimalConfig + minimalConfig, "trailing bracket": minimalConfig + "]",
		"trailing brace": minimalConfig + "}", "null": "null", "array": "[" + minimalConfig + "]", "empty": "",
	} {
		if _, err := ParseConfig([]byte(document)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("ParseConfig(%s) error = %v", name, err)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	config, err := ParseConfig([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	issuer := config.Issuers[0]
	if !issuer.requiresAccessTokenType() || issuer.clockSkew().Seconds() != defaultClockSkewSecs || issuer.maxLifetime().Seconds() != 900 {
		t.Fatalf("defaults = type %v, skew %v, lifetime %v", issuer.requiresAccessTokenType(), issuer.clockSkew(), issuer.maxLifetime())
	}
	if config.Development() {
		t.Fatal("a configuration without a development issuer reports one")
	}
	full, err := ParseConfig([]byte(fullConfig))
	if err != nil || !full.Development() {
		t.Fatalf("development adapter issuer not reported: %v", err)
	}
}

func TestLoadConfigNeverEchoesFileContents(t *testing.T) {
	dir := t.TempDir()
	secretish := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(secretish, []byte(`{"issuers":"canary-value-do-not-echo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "large.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat(" ", maxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"invalid": secretish, "oversized": oversized, "missing": filepath.Join(dir, "absent.json")} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(path)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatalf("error echoes file contents: %v", err)
			}
		})
	}
	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(fullConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if config, err := LoadConfig(valid); err != nil || len(config.Issuers) != 2 {
		t.Fatalf("LoadConfig(valid) = %+v, %v", config, err)
	}
}
