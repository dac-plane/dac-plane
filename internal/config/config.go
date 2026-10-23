// Package config loads and validates process configuration.
package config

import (
	"errors"
	"strings"
	"time"
)

const (
	databaseURLEnv            = "DAC_DATABASE_URL"
	databaseConnectTimeoutEnv = "DAC_DATABASE_CONNECT_TIMEOUT"
	identityConfigFileEnv     = "DAC_IDENTITY_CONFIG_FILE"
	defaultConnectTimeout     = 10 * time.Second
)

// Config contains the configuration needed to bootstrap the control-plane process.
type Config struct {
	DatabaseURL            string
	DatabaseConnectTimeout time.Duration
	// IdentityConfigFile names the trusted-issuer configuration the Surface
	// API authenticates against (ADR-0007). It is optional until a process
	// serves the Surface API.
	IdentityConfigFile string
}

// Load reads configuration through lookup. Error messages intentionally omit
// values because environment variables may contain credentials.
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("configuration lookup is required")
	}

	databaseURL, ok := lookup(databaseURLEnv)
	if !ok || strings.TrimSpace(databaseURL) == "" {
		return Config{}, errors.New("DAC_DATABASE_URL is required")
	}

	connectTimeout := defaultConnectTimeout
	if rawTimeout, exists := lookup(databaseConnectTimeoutEnv); exists {
		parsedTimeout, err := time.ParseDuration(strings.TrimSpace(rawTimeout))
		if err != nil || parsedTimeout <= 0 {
			return Config{}, errors.New("DAC_DATABASE_CONNECT_TIMEOUT must be a positive duration")
		}
		connectTimeout = parsedTimeout
	}

	identityConfigFile, _ := lookup(identityConfigFileEnv)

	return Config{
		DatabaseURL:            databaseURL,
		DatabaseConnectTimeout: connectTimeout,
		IdentityConfigFile:     strings.TrimSpace(identityConfigFile),
	}, nil
}
