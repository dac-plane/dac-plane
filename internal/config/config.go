// Package config loads and validates process configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const (
	databaseURLEnv            = "DAC_DATABASE_URL"
	databaseConnectTimeoutEnv = "DAC_DATABASE_CONNECT_TIMEOUT"
	identityConfigFileEnv     = "DAC_IDENTITY_CONFIG_FILE"
	surfaceAddrEnv            = "DAC_SURFACE_ADDR"
	adapterAddrEnv            = "DAC_ADAPTER_ADDR"
	opsAddrEnv                = "DAC_OPS_ADDR"
	tlsCertFileEnv            = "DAC_TLS_CERT_FILE"
	tlsKeyFileEnv             = "DAC_TLS_KEY_FILE"
	deploymentEnv             = "DAC_DEPLOYMENT"
	fileSuffix                = "_FILE"
	defaultConnectTimeout     = 10 * time.Second
	defaultSurfaceAddr        = ":8443"
	defaultAdapterAddr        = ":9443"
	defaultOpsAddr            = "127.0.0.1:8081"
	maxSecretFileBytes        = 64 << 10
)

// Deployment says which configuration rules a process enforces (ADR-0009).
type Deployment string

const (
	// DeploymentProduction refuses development-only trust, such as the
	// development profile's test issuer. It is the default.
	DeploymentProduction Deployment = "production"
	// DeploymentDevelopment is the reference stack's development profile.
	DeploymentDevelopment Deployment = "development"
)

// Config contains the configuration needed to bootstrap the control-plane process.
type Config struct {
	DatabaseURL            string
	DatabaseConnectTimeout time.Duration
	// IdentityConfigFile names the trusted-issuer configuration the Surface
	// API and the adapter endpoint authenticate against (ADR-0007,
	// ADR-0008). Serving requires it.
	IdentityConfigFile string
	// SurfaceAddr, AdapterAddr and OpsAddr are the listener addresses of
	// the Surface API, the adapter endpoint and the operations listener.
	SurfaceAddr, AdapterAddr, OpsAddr string
	// TLSCertFile and TLSKeyFile hold the PEM certificate chain and key
	// every non-loopback listener serves (ADR-0009). Serving requires them.
	TLSCertFile, TLSKeyFile string
	Deployment              Deployment
}

// Load reads configuration through lookup. A secret-bearing setting may
// instead name a file in <NAME>_FILE, so secrets arrive through the
// substrate's secret mechanism rather than the environment. Error messages
// intentionally omit values because settings may contain credentials.
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("configuration lookup is required")
	}

	databaseURL, err := secret(lookup, databaseURLEnv)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(databaseURL) == "" {
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

	cfg := Config{
		DatabaseURL:            databaseURL,
		DatabaseConnectTimeout: connectTimeout,
		IdentityConfigFile:     setting(lookup, identityConfigFileEnv, ""),
		SurfaceAddr:            setting(lookup, surfaceAddrEnv, defaultSurfaceAddr),
		AdapterAddr:            setting(lookup, adapterAddrEnv, defaultAdapterAddr),
		OpsAddr:                setting(lookup, opsAddrEnv, defaultOpsAddr),
		TLSCertFile:            setting(lookup, tlsCertFileEnv, ""),
		TLSKeyFile:             setting(lookup, tlsKeyFileEnv, ""),
		Deployment:             Deployment(setting(lookup, deploymentEnv, string(DeploymentProduction))),
	}
	if cfg.Deployment != DeploymentProduction && cfg.Deployment != DeploymentDevelopment {
		return Config{}, errors.New("DAC_DEPLOYMENT must be production or development")
	}
	for name, addr := range map[string]string{surfaceAddrEnv: cfg.SurfaceAddr, adapterAddrEnv: cfg.AdapterAddr, opsAddrEnv: cfg.OpsAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return Config{}, fmt.Errorf("%s must be a host:port listener address", name)
		}
	}
	return cfg, nil
}

// ValidateServe reports what serving needs beyond Load: the identity
// configuration and the TLS material.
func (c Config) ValidateServe() error {
	if c.IdentityConfigFile == "" {
		return errors.New("DAC_IDENTITY_CONFIG_FILE is required to serve")
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return errors.New("DAC_TLS_CERT_FILE and DAC_TLS_KEY_FILE are required to serve")
	}
	return nil
}

// OpsLoopback reports whether the operations listener is bound to loopback
// only, where it may serve without TLS.
func (c Config) OpsLoopback() bool {
	host, _, err := net.SplitHostPort(c.OpsAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func setting(lookup func(string) (string, bool), name, fallback string) string {
	value, ok := lookup(name)
	if value = strings.TrimSpace(value); !ok || value == "" {
		return fallback
	}
	return value
}

// secret reads name directly or from the file <name>_FILE names, never both.
// A trailing line break in the file is dropped.
func secret(lookup func(string) (string, bool), name string) (string, error) {
	value, direct := lookup(name)
	path, fromFile := lookup(name + fileSuffix)
	if !fromFile {
		return value, nil
	}
	if direct {
		return "", fmt.Errorf("set %s or %s%s, not both", name, name, fileSuffix)
	}
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("cannot open the %s%s file", name, fileSuffix)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil || len(data) > maxSecretFileBytes {
		return "", fmt.Errorf("cannot read the %s%s file", name, fileSuffix)
	}
	return string(bytes.TrimRight(data, "\r\n")), nil
}
