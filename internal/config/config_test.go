package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(mapLookup(nil))
	if err == nil {
		t.Fatal("Load() error = nil, want missing database URL error")
	}
}

func TestLoadUsesDefaultConnectTimeout(t *testing.T) {
	config, err := Load(mapLookup(map[string]string{
		databaseURLEnv: "postgres://dac:test@localhost/dac",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.DatabaseConnectTimeout != 10*time.Second {
		t.Fatalf("DatabaseConnectTimeout = %s, want 10s", config.DatabaseConnectTimeout)
	}
}

func TestLoadAcceptsPositiveConnectTimeout(t *testing.T) {
	config, err := Load(mapLookup(map[string]string{
		databaseURLEnv:            "postgres://dac:test@localhost/dac",
		databaseConnectTimeoutEnv: "3s",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.DatabaseConnectTimeout != 3*time.Second {
		t.Fatalf("DatabaseConnectTimeout = %s, want 3s", config.DatabaseConnectTimeout)
	}
}

func TestLoadRejectsInvalidConnectTimeoutWithoutExposingValue(t *testing.T) {
	const sensitiveValue = "not-a-duration-secret-value"
	_, err := Load(mapLookup(map[string]string{
		databaseURLEnv:            "postgres://dac:test@localhost/dac",
		databaseConnectTimeoutEnv: sensitiveValue,
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want invalid timeout error")
	}
	if strings.Contains(err.Error(), sensitiveValue) {
		t.Fatalf("Load() error exposed configuration value: %v", err)
	}
}

func TestLoadRejectsNonPositiveConnectTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			_, err := Load(mapLookup(map[string]string{
				databaseURLEnv:            "postgres://dac:test@localhost/dac",
				databaseConnectTimeoutEnv: timeout,
			}))
			if err == nil {
				t.Fatalf("Load() error = nil for %q, want error", timeout)
			}
		})
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadReadsOptionalIdentityConfigFile(t *testing.T) {
	config, err := Load(mapLookup(map[string]string{databaseURLEnv: "postgres://dac:test@localhost/dac"}))
	if err != nil || config.IdentityConfigFile != "" {
		t.Fatalf("without identity file: %+v, %v", config, err)
	}
	config, err = Load(mapLookup(map[string]string{
		databaseURLEnv:        "postgres://dac:test@localhost/dac",
		identityConfigFileEnv: " /run/config/identity.json ",
	}))
	if err != nil || config.IdentityConfigFile != "/run/config/identity.json" {
		t.Fatalf("with identity file: %+v, %v", config, err)
	}
}

func TestLoadReadsTheDatabaseURLFromAFile(t *testing.T) {
	path := t.TempDir() + "/database-url"
	if err := os.WriteFile(path, []byte("postgres://dac:file@localhost/dac\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(mapLookup(map[string]string{databaseURLEnv + fileSuffix: path}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.DatabaseURL != "postgres://dac:file@localhost/dac" {
		t.Fatal("DatabaseURL was not read from the file without its line break")
	}
}

func TestLoadRefusesBothASecretAndItsFile(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{
		databaseURLEnv:              "postgres://dac:secret-value@localhost/dac",
		databaseURLEnv + fileSuffix: "/run/secrets/database-url",
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want a refusal of both forms")
	}
	if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "/run/secrets") {
		t.Fatalf("Load() error exposed configuration: %v", err)
	}
}

func TestLoadRefusesAnUnreadableSecretFileWithoutNamingIt(t *testing.T) {
	path := t.TempDir() + "/missing-secret-path"
	_, err := Load(mapLookup(map[string]string{databaseURLEnv + fileSuffix: path}))
	if err == nil || strings.Contains(err.Error(), path) {
		t.Fatalf("Load() error = %v, want a refusal that does not name the path", err)
	}
}

func TestLoadDefaultsListenersAndDeployment(t *testing.T) {
	config, err := Load(mapLookup(map[string]string{databaseURLEnv: "postgres://dac:test@localhost/dac"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.SurfaceAddr != ":8443" || config.AdapterAddr != ":9443" || config.OpsAddr != "127.0.0.1:8081" || config.Deployment != DeploymentProduction {
		t.Fatalf("defaults = %+v", config)
	}
	if !config.OpsLoopback() {
		t.Fatal("default operations listener is not loopback")
	}
	if err := config.ValidateServe(); err == nil {
		t.Fatal("ValidateServe() accepted a configuration without identity or TLS files")
	}
}

func TestLoadRejectsAnUnknownDeploymentAndBadAddresses(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"deployment":   {deploymentEnv: "staging"},
		"surface addr": {surfaceAddrEnv: "8443"},
		"ops addr":     {opsAddrEnv: "localhost"},
	} {
		t.Run(name, func(t *testing.T) {
			values[databaseURLEnv] = "postgres://dac:test@localhost/dac"
			if _, err := Load(mapLookup(values)); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestOpsLoopbackOnlyForLoopbackHosts(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8081": true, "[::1]:8081": true, "localhost:8081": true, ":8081": false, "0.0.0.0:8081": false, "10.0.0.5:8081": false} {
		if got := (Config{OpsAddr: addr}).OpsLoopback(); got != want {
			t.Errorf("OpsLoopback(%s) = %v, want %v", addr, got, want)
		}
	}
}
