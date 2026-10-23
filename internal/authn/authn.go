// Package authn authenticates surface actors (ADR-0007). It verifies an
// OAuth 2.0 access token from an operator-configured OpenID Connect issuer and
// maps the verified (issuer, subject) pair, through an explicit stored
// binding, to a principal of the tenant the request names.
//
// The package is transport-neutral: the Surface API hands it the bearer token
// and the tenant from the request path. Authentication only establishes the
// principal; authorization stays in internal/application.
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const (
	maxTokenBytes  = 8192
	defaultTimeout = 10 * time.Second
)

// ErrUnauthenticated is the single refusal every caller sees. A malformed
// token, an untrusted issuer, a bad signature, an expired token and an
// identity without an enabled binding in the requested tenant are
// indistinguishable to the caller; the reason is logged, never returned.
var ErrUnauthenticated = errors.New("unauthenticated")

// accessTokenTypes are the JWT typ values RFC 9068 defines for access
// tokens. Media types compare case-insensitively.
var accessTokenTypes = []string{"at+jwt", "application/at+jwt"}

// Reason categorizes a refusal for the log. It never contains token content.
type Reason string

const (
	ReasonMissing             Reason = "missing"
	ReasonMalformed           Reason = "malformed"
	ReasonAlgorithmNotAllowed Reason = "algorithm_not_allowed"
	ReasonUntrustedIssuer     Reason = "untrusted_issuer"
	ReasonWrongType           Reason = "wrong_type"
	ReasonWrongAudience       Reason = "wrong_audience"
	ReasonBadSignature        Reason = "bad_signature"
	ReasonExpired             Reason = "expired"
	ReasonNotYetValid         Reason = "not_yet_valid"
	ReasonLifetimeExceeded    Reason = "lifetime_exceeded"
	ReasonInvalidClaims       Reason = "invalid_claims"
	ReasonUnbound             Reason = "unbound"
)

// Resolver returns the principal an enabled binding of (issuer, subject)
// names in tenant. found is false for an unbound identity, a disabled binding
// and a binding in another tenant alike; err is reserved for operational
// failures.
type Resolver func(ctx context.Context, issuer, subject string, tenant ids.TenantID) (principal identity.Principal, found bool, err error)

// Option adjusts how an Authenticator reaches issuers and reads time.
type Option func(*options)

type options struct {
	client     *http.Client
	now        func() time.Time
	minRefresh time.Duration
	maxKeyAge  time.Duration
}

// WithHTTPClient sets the client used for discovery and key fetching.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) { o.client = client }
}

// WithClock sets the time source for token time checks and key refresh.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithKeyRefresh sets the minimum interval between key-set refreshes and the
// age after which cached keys are refreshed.
func WithKeyRefresh(minInterval, maxAge time.Duration) Option {
	return func(o *options) { o.minRefresh, o.maxKeyAge = minInterval, maxAge }
}

type trustedIssuer struct {
	config   IssuerConfig
	verifier *oidc.IDTokenVerifier
	keys     *keySet
}

// Authenticator verifies access tokens from the configured issuers and maps
// them to principals. It is safe for concurrent use.
type Authenticator struct {
	issuers map[string]*trustedIssuer
	resolve Resolver
	logger  *slog.Logger
	now     func() time.Time
}

// New discovers every configured issuer and prepares its verifier. Discovery
// must confirm the configured issuer exactly and publish an https jwks_uri;
// otherwise New fails and no token from that issuer is ever accepted.
func New(ctx context.Context, config Config, resolve Resolver, logger *slog.Logger, opts ...Option) (*Authenticator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if resolve == nil || logger == nil {
		return nil, errors.New("authn: resolver and logger are required")
	}
	settings := options{
		client:     &http.Client{Timeout: defaultTimeout},
		now:        time.Now,
		minRefresh: defaultMinRefreshInterval,
		maxKeyAge:  defaultMaxKeyAge,
	}
	for _, opt := range opts {
		opt(&settings)
	}
	authenticator := &Authenticator{issuers: map[string]*trustedIssuer{}, resolve: resolve, logger: logger, now: settings.now}
	for _, issuerConfig := range config.Issuers {
		trusted, err := discover(ctx, issuerConfig, settings)
		if err != nil {
			return nil, fmt.Errorf("authn: issuer %s: %w", issuerConfig.Issuer, err)
		}
		authenticator.issuers[issuerConfig.Issuer] = trusted
		if !issuerConfig.requiresAccessTokenType() {
			logger.Warn("access token typing is not required for this issuer; its audience must be issued for this API alone",
				"issuer", issuerConfig.Issuer)
		}
	}
	return authenticator, nil
}

func discover(ctx context.Context, config IssuerConfig, settings options) (*trustedIssuer, error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, settings.client), config.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var metadata struct {
		KeysURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, fmt.Errorf("discovery metadata: %w", err)
	}
	keysURL, err := url.Parse(metadata.KeysURL)
	if err != nil || keysURL.Scheme != "https" || keysURL.Host == "" {
		return nil, errors.New("discovery must publish an https jwks_uri")
	}
	keys := &keySet{
		client:     settings.client,
		url:        metadata.KeysURL,
		algorithms: joseAlgorithms(config.Algorithms),
		now:        settings.now,
		minRefresh: settings.minRefresh,
		maxAge:     settings.maxKeyAge,
	}
	verifier := oidc.NewVerifier(config.Issuer, keys, &oidc.Config{
		ClientID:             config.Audience,
		SupportedSigningAlgs: config.Algorithms,
		// Expiry, not-before and issued-at are checked afterwards with this
		// issuer's configured clock skew and maximum lifetime.
		SkipExpiryCheck: true,
	})
	return &trustedIssuer{config: config, verifier: verifier, keys: keys}, nil
}

// Authenticate verifies token and returns the authorization context of the
// principal its identity is bound to in tenant. Every refusal is
// ErrUnauthenticated; any other error is an operational failure of the
// resolver.
func (a *Authenticator) Authenticate(ctx context.Context, token string, tenant ids.TenantID) (identity.AuthorizationContext, error) {
	issuer, subject, reason := a.verify(ctx, token)
	if reason != "" {
		a.refuse(ctx, reason, issuer)
		return identity.AuthorizationContext{}, ErrUnauthenticated
	}
	principal, found, err := a.resolve(ctx, issuer, subject, tenant)
	if err != nil {
		return identity.AuthorizationContext{}, fmt.Errorf("authn: resolve identity binding: %w", err)
	}
	if !found || principal.Tenant != tenant {
		a.refuse(ctx, ReasonUnbound, issuer)
		return identity.AuthorizationContext{}, ErrUnauthenticated
	}
	return identity.NewAuthorizationContext(principal)
}

// refuse logs a refusal with its reason and, only when it is a configured
// issuer, the issuer. Nothing from the token itself is logged.
func (a *Authenticator) refuse(ctx context.Context, reason Reason, issuer string) {
	attrs := []slog.Attr{slog.String("reason", string(reason))}
	if issuer != "" {
		attrs = append(attrs, slog.String("issuer", issuer))
	}
	a.logger.LogAttrs(ctx, slog.LevelWarn, "authentication refused", attrs...)
}

// verify returns the verified (issuer, subject), or the reason it refused.
// issuer is set on refusal only when it names a configured issuer.
func (a *Authenticator) verify(ctx context.Context, token string) (string, string, Reason) {
	if token == "" {
		return "", "", ReasonMissing
	}
	if len(token) > maxTokenBytes {
		return "", "", ReasonMalformed
	}
	// Parse the compact serialization only; JWE and JSON serializations are
	// refused here, before any key is consulted.
	signed, err := jose.ParseSignedCompact(token, joseAlgorithms(knownAlgorithms))
	if err != nil || len(signed.Signatures) != 1 {
		return "", "", ReasonMalformed
	}
	header := signed.Signatures[0].Header
	if !slices.Contains(SigningAlgorithms(), header.Algorithm) {
		return "", "", ReasonAlgorithmNotAllowed
	}
	var unverified struct {
		Issuer   string       `json:"iss"`
		Audience jwt.Audience `json:"aud"`
	}
	if err := json.Unmarshal(signed.UnsafePayloadWithoutVerification(), &unverified); err != nil {
		return "", "", ReasonMalformed
	}
	trusted, ok := a.issuers[unverified.Issuer]
	if !ok {
		return "", "", ReasonUntrustedIssuer
	}
	issuer := trusted.config.Issuer
	if !slices.Contains(trusted.config.Algorithms, header.Algorithm) {
		return issuer, "", ReasonAlgorithmNotAllowed
	}
	if trusted.config.requiresAccessTokenType() {
		typ, _ := header.ExtraHeaders[jose.HeaderType].(string)
		if !slices.ContainsFunc(accessTokenTypes, func(want string) bool { return strings.EqualFold(typ, want) }) {
			return issuer, "", ReasonWrongType
		}
	}
	verified, err := trusted.verifier.Verify(ctx, token)
	if err != nil {
		if !unverified.Audience.Contains(trusted.config.Audience) {
			return issuer, "", ReasonWrongAudience
		}
		return issuer, "", ReasonBadSignature
	}
	if reason := checkClaims(verified, trusted.config, a.now()); reason != "" {
		return issuer, "", reason
	}
	return issuer, verified.Subject, ""
}

// checkClaims applies the time, lifetime and shape rules go-oidc leaves to
// the caller. An access token never carries the ID-token-only claims nonce or
// at_hash; refusing them keeps an ID token from standing in for an access
// token even where typing is not required (RFC 8725 mutually exclusive
// validation).
func checkClaims(token *oidc.IDToken, config IssuerConfig, now time.Time) Reason {
	var claims struct {
		Expiry    *jwt.NumericDate `json:"exp"`
		NotBefore *jwt.NumericDate `json:"nbf"`
		IssuedAt  *jwt.NumericDate `json:"iat"`
		Subject   string           `json:"sub"`
	}
	var present map[string]json.RawMessage
	if token.Claims(&claims) != nil || token.Claims(&present) != nil {
		return ReasonInvalidClaims
	}
	if claims.Expiry == nil || claims.IssuedAt == nil || !identity.ValidSubject(claims.Subject) {
		return ReasonInvalidClaims
	}
	if _, ok := present["nonce"]; ok {
		return ReasonInvalidClaims
	}
	if _, ok := present["at_hash"]; ok {
		return ReasonInvalidClaims
	}
	skew := config.clockSkew()
	expiry, issuedAt := claims.Expiry.Time(), claims.IssuedAt.Time()
	switch {
	case now.After(expiry.Add(skew)):
		return ReasonExpired
	case claims.NotBefore != nil && claims.NotBefore.Time().After(now.Add(skew)):
		return ReasonNotYetValid
	case issuedAt.After(now.Add(skew)):
		return ReasonNotYetValid
	case expiry.Before(issuedAt):
		return ReasonInvalidClaims
	case expiry.Sub(issuedAt) > config.maxLifetime():
		return ReasonLifetimeExceeded
	}
	return ""
}

// knownAlgorithms are parsed so a disallowed algorithm is reported as such
// rather than as a malformed token; only SigningAlgorithms ever verify.
var knownAlgorithms = append(SigningAlgorithms(), "HS256", "HS384", "HS512")

// BearerToken extracts the token from an Authorization header value using
// the Bearer scheme (RFC 6750). The scheme compares case-insensitively; the
// token must be a single b64token.
func BearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > maxTokenBytes {
		return "", false
	}
	trimmed := strings.TrimRight(token, "=")
	for _, r := range trimmed {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/", r)) {
			return "", false
		}
	}
	return token, trimmed != ""
}
