package authn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
)

const (
	maxConfigBytes       = 1 << 20
	maxAudienceLength    = 2048
	maxLifetimeSeconds   = 86400
	maxClockSkewSeconds  = 300
	defaultClockSkewSecs = 30
)

var audiencePattern = regexp.MustCompile(`^[\x21-\x7e]+$`)

// ErrInvalidConfig reports an identity configuration that fails validation.
var ErrInvalidConfig = errors.New("invalid identity configuration")

// SigningAlgorithms lists the asymmetric JWS algorithms an issuer may be
// pinned to. Symmetric algorithms are never accepted: a resource server must
// verify with the issuer's published keys, not a shared value.
func SigningAlgorithms() []string {
	return []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
}

// Config lists the identity providers whose access tokens the control plane
// accepts. It is loaded from the file DAC_IDENTITY_CONFIG_FILE names and
// mirrors contracts/jsonschema/config/identity-config.schema.json.
type Config struct {
	Issuers []IssuerConfig `json:"issuers"`
}

// IssuerConfig is how one trusted issuer's access tokens are verified.
// RequireAccessTokenType defaults to true when absent: tokens must be typed
// at+jwt (RFC 9068). Turning it off is for an issuer that cannot type its
// access tokens; that issuer's audience must then be issued only for this API.
type IssuerConfig struct {
	Issuer                  string   `json:"issuer"`
	Audience                string   `json:"audience"`
	Algorithms              []string `json:"algorithms"`
	MaxTokenLifetimeSeconds int      `json:"max_token_lifetime_seconds"`
	ClockSkewSeconds        *int     `json:"clock_skew_seconds,omitempty"`
	RequireAccessTokenType  *bool    `json:"require_access_token_type,omitempty"`
}

// Validate reports whether the configuration is complete and safe to use.
func (c Config) Validate() error {
	if len(c.Issuers) == 0 {
		return fmt.Errorf("%w: at least one issuer is required", ErrInvalidConfig)
	}
	seen := map[string]bool{}
	for i, issuer := range c.Issuers {
		if err := issuer.validate(); err != nil {
			return fmt.Errorf("issuer %d: %w", i, err)
		}
		if seen[issuer.Issuer] {
			return fmt.Errorf("%w: issuer %d is configured twice", ErrInvalidConfig, i)
		}
		seen[issuer.Issuer] = true
	}
	return nil
}

func (c IssuerConfig) validate() error {
	if !identity.ValidIssuer(c.Issuer) {
		return fmt.Errorf("%w: issuer must be an https URL without query or fragment", ErrInvalidConfig)
	}
	if len(c.Audience) > maxAudienceLength || !audiencePattern.MatchString(c.Audience) {
		return fmt.Errorf("%w: audience must be 1 to %d visible ASCII characters", ErrInvalidConfig, maxAudienceLength)
	}
	if len(c.Algorithms) == 0 {
		return fmt.Errorf("%w: at least one signing algorithm is required", ErrInvalidConfig)
	}
	for i, algorithm := range c.Algorithms {
		if !slices.Contains(SigningAlgorithms(), algorithm) || slices.Contains(c.Algorithms[:i], algorithm) {
			return fmt.Errorf("%w: algorithms must be distinct asymmetric JWS algorithms", ErrInvalidConfig)
		}
	}
	if c.MaxTokenLifetimeSeconds < 1 || c.MaxTokenLifetimeSeconds > maxLifetimeSeconds {
		return fmt.Errorf("%w: max_token_lifetime_seconds must be 1 to %d", ErrInvalidConfig, maxLifetimeSeconds)
	}
	if c.ClockSkewSeconds != nil && (*c.ClockSkewSeconds < 0 || *c.ClockSkewSeconds > maxClockSkewSeconds) {
		return fmt.Errorf("%w: clock_skew_seconds must be 0 to %d", ErrInvalidConfig, maxClockSkewSeconds)
	}
	return nil
}

func (c IssuerConfig) maxLifetime() time.Duration {
	return time.Duration(c.MaxTokenLifetimeSeconds) * time.Second
}

func (c IssuerConfig) clockSkew() time.Duration {
	if c.ClockSkewSeconds == nil {
		return defaultClockSkewSecs * time.Second
	}
	return time.Duration(*c.ClockSkewSeconds) * time.Second
}

func (c IssuerConfig) requiresAccessTokenType() bool {
	return c.RequireAccessTokenType == nil || *c.RequireAccessTokenType
}

// LoadConfig reads and validates the identity configuration at path. Errors
// name the problem but never echo file contents.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("%w: cannot open the identity configuration file", ErrInvalidConfig)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("%w: cannot read the identity configuration file", ErrInvalidConfig)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: the identity configuration file exceeds %d bytes", ErrInvalidConfig, maxConfigBytes)
	}
	return ParseConfig(data)
}

// ParseConfig decodes a configuration document strictly, rejecting unknown
// fields, and validates it.
func ParseConfig(data []byte) (Config, error) {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, fmt.Errorf("%w: the identity configuration must be one JSON object", ErrInvalidConfig)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%w: the identity configuration is not a valid document", ErrInvalidConfig)
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%w: the identity configuration holds more than one document", ErrInvalidConfig)
	}
	return config, config.Validate()
}
