package config

import (
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
