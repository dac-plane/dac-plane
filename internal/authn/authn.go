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

// Verifier verifies access tokens from a set of trusted issuers, each with
// its own audience, and returns the verified identity. It maps nothing to a
// principal or provider; callers resolve the identity through their own
// binding. It is safe for concurrent use.
type Verifier struct {
	issuers map[string]*trustedIssuer
	logger  *slog.Logger
	now     func() time.Time
}

// Verified is a verified access token's identity and expiry. Expiry bounds
// anything the token authenticates, such as an adapter connection.
type Verified struct {
	Issuer  string
	Subject string
	Expiry  time.Time
}

// NewVerifier discovers every issuer and prepares its verifier. Discovery
// must confirm the configured issuer exactly and publish an https jwks_uri;
// otherwise NewVerifier fails and no token from that issuer is ever accepted.
// An empty issuer list yields a verifier that refuses every token.
func NewVerifier(ctx context.Context, issuers []IssuerConfig, logger *slog.Logger, opts ...Option) (*Verifier, error) {
	if logger == nil {
		return nil, errors.New("authn: logger is required")
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
	verifier := &Verifier{issuers: map[string]*trustedIssuer{}, logger: logger, now: settings.now}
	for _, issuerConfig := range issuers {
		if err := issuerConfig.validate(); err != nil {
			return nil, err
		}
		trusted, err := discover(ctx, issuerConfig, settings)
		if err != nil {
			return nil, fmt.Errorf("authn: issuer %s: %w", issuerConfig.Issuer, err)
		}
		verifier.issuers[issuerConfig.Issuer] = trusted
		if !issuerConfig.requiresAccessTokenType() {
			logger.Warn("access token typing is not required for this issuer; its audience must be issued for this API alone",
				"issuer", issuerConfig.Issuer)
		}
	}
	return verifier, nil
}

// Verify verifies token and returns its identity and expiry. Every refusal
// is ErrUnauthenticated; the reason is logged with the configured issuer, if
// any, and never with token content.
func (v *Verifier) Verify(ctx context.Context, token string) (Verified, error) {
	verified, reason := v.verify(ctx, token)
	if reason != "" {
		v.Refuse(ctx, reason, verified.Issuer)
		return Verified{}, ErrUnauthenticated
	}
	return verified, nil
}

// Refuse logs a refusal with its reason and, only when it is a configured
// issuer, the issuer. Nothing from the token itself is logged.
func (v *Verifier) Refuse(ctx context.Context, reason Reason, issuer string) {
	attrs := []slog.Attr{slog.String("reason", string(reason))}
	if issuer != "" {
		attrs = append(attrs, slog.String("issuer", issuer))
	}
	v.logger.LogAttrs(ctx, slog.LevelWarn, "authentication refused", attrs...)
}

// Authenticator verifies Surface API access tokens and maps them to
// principals. It is safe for concurrent use.
type Authenticator struct {
	verifier *Verifier
	resolve  Resolver
}

// New discovers every configured Surface API issuer and prepares its
// verifier; adapter issuers are verified separately (NewVerifier with
// Config.AdapterIssuers).
func New(ctx context.Context, config Config, resolve Resolver, logger *slog.Logger, opts ...Option) (*Authenticator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if resolve == nil || logger == nil {
		return nil, errors.New("authn: resolver and logger are required")
	}
	verifier, err := NewVerifier(ctx, config.Issuers, logger, opts...)
	if err != nil {
		return nil, err
	}
	return &Authenticator{verifier: verifier, resolve: resolve}, nil
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
	verified, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return identity.AuthorizationContext{}, err
	}
	principal, found, err := a.resolve(ctx, verified.Issuer, verified.Subject, tenant)
	if err != nil {
		return identity.AuthorizationContext{}, fmt.Errorf("authn: resolve identity binding: %w", err)
	}
	if !found || principal.Tenant != tenant {
		a.verifier.Refuse(ctx, ReasonUnbound, verified.Issuer)
		return identity.AuthorizationContext{}, ErrUnauthenticated
	}
	return identity.NewAuthorizationContext(principal)
}

// verify returns the verified identity, or the reason it refused. On
// refusal the identity carries the issuer only when it names a configured
// issuer.
func (v *Verifier) verify(ctx context.Context, token string) (Verified, Reason) {
	if token == "" {
		return Verified{}, ReasonMissing
	}
	if len(token) > maxTokenBytes {
		return Verified{}, ReasonMalformed
	}
	// Parse the compact serialization only; JWE and JSON serializations are
	// refused here, before any key is consulted.
	signed, err := jose.ParseSignedCompact(token, joseAlgorithms(knownAlgorithms))
	if err != nil || len(signed.Signatures) != 1 {
		return Verified{}, ReasonMalformed
	}
	header := signed.Signatures[0].Header
	if !slices.Contains(SigningAlgorithms(), header.Algorithm) {
		return Verified{}, ReasonAlgorithmNotAllowed
	}
	var unverified struct {
		Issuer   string       `json:"iss"`
		Audience jwt.Audience `json:"aud"`
	}
	if err := json.Unmarshal(signed.UnsafePayloadWithoutVerification(), &unverified); err != nil {
		return Verified{}, ReasonMalformed
	}
	trusted, ok := v.issuers[unverified.Issuer]
	if !ok {
		return Verified{}, ReasonUntrustedIssuer
	}
	refused := Verified{Issuer: trusted.config.Issuer}
	if !slices.Contains(trusted.config.Algorithms, header.Algorithm) {
		return refused, ReasonAlgorithmNotAllowed
	}
	if trusted.config.requiresAccessTokenType() {
		typ, _ := header.ExtraHeaders[jose.HeaderType].(string)
		if !slices.ContainsFunc(accessTokenTypes, func(want string) bool { return strings.EqualFold(typ, want) }) {
			return refused, ReasonWrongType
		}
	}
	verifiedToken, err := trusted.verifier.Verify(ctx, token)
	if err != nil {
		if !unverified.Audience.Contains(trusted.config.Audience) {
			return refused, ReasonWrongAudience
		}
		return refused, ReasonBadSignature
	}
	expiry, reason := checkClaims(verifiedToken, trusted.config, v.now())
	if reason != "" {
		return refused, reason
	}
	return Verified{Issuer: trusted.config.Issuer, Subject: verifiedToken.Subject, Expiry: expiry}, ""
}

// checkClaims applies the time, lifetime and shape rules go-oidc leaves to
// the caller. An access token never carries the ID-token-only claims nonce or
// at_hash; refusing them keeps an ID token from standing in for an access
// token even where typing is not required (RFC 8725 mutually exclusive
// validation).
func checkClaims(token *oidc.IDToken, config IssuerConfig, now time.Time) (time.Time, Reason) {
	var claims struct {
		Expiry    *jwt.NumericDate `json:"exp"`
		NotBefore *jwt.NumericDate `json:"nbf"`
		IssuedAt  *jwt.NumericDate `json:"iat"`
		Subject   string           `json:"sub"`
	}
	var present map[string]json.RawMessage
	if token.Claims(&claims) != nil || token.Claims(&present) != nil {
		return time.Time{}, ReasonInvalidClaims
	}
	if claims.Expiry == nil || claims.IssuedAt == nil || !identity.ValidSubject(claims.Subject) {
		return time.Time{}, ReasonInvalidClaims
	}
	if _, ok := present["nonce"]; ok {
		return time.Time{}, ReasonInvalidClaims
	}
	if _, ok := present["at_hash"]; ok {
		return time.Time{}, ReasonInvalidClaims
	}
	skew := config.clockSkew()
	expiry, issuedAt := claims.Expiry.Time(), claims.IssuedAt.Time()
	switch {
	case now.After(expiry.Add(skew)):
		return time.Time{}, ReasonExpired
	case claims.NotBefore != nil && claims.NotBefore.Time().After(now.Add(skew)):
		return time.Time{}, ReasonNotYetValid
	case issuedAt.After(now.Add(skew)):
		return time.Time{}, ReasonNotYetValid
	case expiry.Before(issuedAt):
		return time.Time{}, ReasonInvalidClaims
	case expiry.Sub(issuedAt) > config.maxLifetime():
		return time.Time{}, ReasonLifetimeExceeded
	}
	return expiry, ""
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
