package authn

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

const testAudience = "https://dac-plane.example/api"

// testIssuer is an OpenID Connect issuer served over TLS from the test
// process: discovery metadata plus a key set the test can rotate.
type testIssuer struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	keys    map[string]crypto.Signer
	fetches atomic.Int32
	// failKeys makes the key endpoint answer 500.
	failKeys atomic.Bool
	// hold, when set, blocks the key endpoint until it is closed; each
	// blocked request is announced on held.
	hold chan struct{}
	held chan struct{}
	// discoveredIssuer overrides the issuer discovery reports.
	discoveredIssuer string
	keysURL          string
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	issuer := &testIssuer{t: t, keys: map[string]crypto.Signer{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		discovered := issuer.discoveredIssuer
		if discovered == "" {
			discovered = issuer.server.URL
		}
		keysURL := issuer.keysURL
		if keysURL == "" {
			keysURL = issuer.server.URL + "/keys"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                discovered,
			"jwks_uri":                              keysURL,
			"authorization_endpoint":                issuer.server.URL + "/authorize",
			"token_endpoint":                        issuer.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		issuer.fetches.Add(1)
		issuer.mu.Lock()
		hold, held := issuer.hold, issuer.held
		issuer.mu.Unlock()
		if hold != nil {
			held <- struct{}{}
			<-hold
		}
		if issuer.failKeys.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		issuer.mu.Lock()
		defer issuer.mu.Unlock()
		var set jose.JSONWebKeySet
		for kid, signer := range issuer.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: signer.Public(), KeyID: kid, Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	issuer.server = httptest.NewTLSServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (i *testIssuer) url() string { return i.server.URL }

func (i *testIssuer) publish(kid string, signer crypto.Signer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys[kid] = signer
}

func (i *testIssuer) withdraw(kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.keys, kid)
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// tokenSpec describes one signed token. Zero values take the defaults of a
// valid access token from issuer with kid "k1".
type tokenSpec struct {
	kid       string
	key       any
	algorithm jose.SignatureAlgorithm
	typ       string
	noType    bool
	embedJWK  bool
	claims    map[string]any
	drop      []string
}

func (i *testIssuer) claims() map[string]any {
	return map[string]any{
		"iss": i.url(), "sub": "user-248289761001", "aud": testAudience,
		"iat": testNow.Add(-time.Minute).Unix(), "exp": testNow.Add(5 * time.Minute).Unix(),
		"jti": "id-1", "client_id": "surface-client",
	}
}

func (i *testIssuer) token(t *testing.T, spec tokenSpec) string {
	t.Helper()
	claims := i.claims()
	for name, value := range spec.claims {
		claims[name] = value
	}
	for _, name := range spec.drop {
		delete(claims, name)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return sign(t, spec, payload).compact
}

type signedToken struct {
	compact, full string
}

func sign(t *testing.T, spec tokenSpec, payload []byte) signedToken {
	t.Helper()
	if spec.kid == "" {
		spec.kid = "k1"
	}
	if spec.algorithm == "" {
		spec.algorithm = jose.RS256
	}
	options := &jose.SignerOptions{EmbedJWK: spec.embedJWK}
	if !spec.noType {
		typ := spec.typ
		if typ == "" {
			typ = "at+jwt"
		}
		options = options.WithType(jose.ContentType(typ))
	}
	key := jose.SigningKey{Algorithm: spec.algorithm, Key: jose.JSONWebKey{Key: spec.key, KeyID: spec.kid}}
	if spec.embedJWK {
		key.Key = spec.key
	}
	if spec.algorithm == jose.HS256 {
		key.Key = spec.key
		options = options.WithHeader(jose.HeaderKey("kid"), spec.kid)
	}
	signer, err := jose.NewSigner(key, options)
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return signedToken{compact: compact, full: object.FullSerialize()}
}

type binding struct {
	issuer, subject string
	tenant          ids.TenantID
}

type harness struct {
	issuer   *testIssuer
	key      *rsa.PrivateKey
	tenant   ids.TenantID
	auth     *Authenticator
	logs     *bytes.Buffer
	clock    *atomic.Pointer[time.Time]
	bindings map[binding]identity.Principal
	failure  error
}

func newHarness(t *testing.T, change func(*IssuerConfig), opts ...Option) *harness {
	t.Helper()
	issuer := newTestIssuer(t)
	key := newRSAKey(t)
	issuer.publish("k1", key)
	tenant := mustID[ids.Tenant](t)
	h := &harness{issuer: issuer, key: key, tenant: tenant, logs: &bytes.Buffer{}, bindings: map[binding]identity.Principal{}}
	now := testNow
	h.clock = &atomic.Pointer[time.Time]{}
	h.clock.Store(&now)
	principal := identity.Principal{Tenant: tenant, ID: mustID[ids.Principal](t), Kind: identity.PrincipalHuman}
	h.bindings[binding{issuer.url(), "user-248289761001", tenant}] = principal
	config := IssuerConfig{Issuer: issuer.url(), Audience: testAudience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 3600}
	if change != nil {
		change(&config)
	}
	resolve := func(_ context.Context, iss, sub string, tenant ids.TenantID) (identity.Principal, bool, error) {
		if h.failure != nil {
			return identity.Principal{}, false, h.failure
		}
		principal, ok := h.bindings[binding{iss, sub, tenant}]
		return principal, ok, nil
	}
	logger := slog.New(slog.NewJSONHandler(h.logs, nil))
	all := append([]Option{
		WithHTTPClient(issuer.server.Client()),
		WithClock(func() time.Time { return *h.clock.Load() }),
		WithKeyRefresh(time.Minute, 15*time.Minute),
	}, opts...)
	auth, err := New(context.Background(), Config{Issuers: []IssuerConfig{config}}, resolve, logger, all...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	h.auth = auth
	return h
}

func (h *harness) advance(d time.Duration) {
	next := h.clock.Load().Add(d)
	h.clock.Store(&next)
}

func (h *harness) principal() identity.Principal {
	return h.bindings[binding{h.issuer.url(), "user-248289761001", h.tenant}]
}

func mustID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// lastReason returns the reason of the last logged refusal.
func (h *harness) lastReason(t *testing.T) Reason {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	var entry struct {
		Msg    string `json:"msg"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil || entry.Msg != "authentication refused" {
		t.Fatalf("last log line is not a refusal: %q", lines[len(lines)-1])
	}
	return Reason(entry.Reason)
}

func TestAuthenticateAcceptsBoundAccessTokens(t *testing.T) {
	h := newHarness(t, func(c *IssuerConfig) { c.Algorithms = []string{"RS256", "ES256"} })
	ecKey := newECKey(t)
	h.issuer.publish("k2", ecKey)
	for name, spec := range map[string]tokenSpec{
		"RS256 at+jwt":                {key: h.key},
		"ES256 at+jwt":                {kid: "k2", key: ecKey, algorithm: jose.ES256},
		"application/at+jwt":          {key: h.key, typ: "application/at+jwt"},
		"type compares ignoring case": {key: h.key, typ: "AT+JWT"},
		"expired within clock skew":   {key: h.key, claims: map[string]any{"exp": testNow.Add(-10 * time.Second).Unix()}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, spec), h.tenant)
			if err != nil {
				t.Fatalf("Authenticate() error = %v; log %s", err, h.logs)
			}
			if ctx.Principal() != h.principal() {
				t.Fatalf("principal = %+v, want %+v", ctx.Principal(), h.principal())
			}
		})
	}
}

func encodeSegment(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func TestAuthenticateRefusesWithTheRightReason(t *testing.T) {
	h := newHarness(t, nil)
	other := newRSAKey(t)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPublic(t, &h.key.PublicKey)})
	valid := h.issuer.token(t, tokenSpec{key: h.key})
	parts := strings.Split(valid, ".")
	unsigned := encodeSegment(`{"alg":"none","typ":"at+jwt"}`) + "." + parts[1] + "."
	lowercase := encodeSegment(`{"alg":"rs256","typ":"at+jwt","kid":"k1"}`) + "." + parts[1] + "." + parts[2]
	payload, _ := json.Marshal(h.issuer.claims())

	cases := []struct {
		name   string
		token  string
		reason Reason
	}{
		{"missing", "", ReasonMissing},
		{"not a JWT", "not-a-token", ReasonMalformed},
		{"oversized", strings.Repeat("a", maxTokenBytes+1), ReasonMalformed},
		{"encrypted serialization", "a.b.c.d.e", ReasonMalformed},
		{"JSON serialization", sign(t, tokenSpec{key: h.key}, payload).full, ReasonMalformed},
		{"alg none", unsigned, ReasonMalformed},
		{"lowercase alg", lowercase, ReasonMalformed},
		{"HS256 keyed with the public key", h.issuer.token(t, tokenSpec{key: publicPEM, algorithm: jose.HS256}), ReasonAlgorithmNotAllowed},
		{"algorithm not pinned for issuer", h.issuer.token(t, tokenSpec{kid: "k1", key: newECKey(t), algorithm: jose.ES256}), ReasonAlgorithmNotAllowed},
		{"untrusted issuer", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"iss": "https://evil.example"}}), ReasonUntrustedIssuer},
		{"missing type", h.issuer.token(t, tokenSpec{key: h.key, noType: true}), ReasonWrongType},
		{"ID token type", h.issuer.token(t, tokenSpec{key: h.key, typ: "JWT"}), ReasonWrongType},
		{"wrong audience", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"aud": "https://other.example"}}), ReasonWrongAudience},
		{"missing audience", h.issuer.token(t, tokenSpec{key: h.key, drop: []string{"aud"}}), ReasonWrongAudience},
		{"unpublished key under a published key ID", h.issuer.token(t, tokenSpec{key: other}), ReasonBadSignature},
		{"key embedded in the header", h.issuer.token(t, tokenSpec{key: other, embedJWK: true}), ReasonBadSignature},
		{"expired", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"exp": testNow.Add(-time.Minute).Unix()}}), ReasonExpired},
		{"not yet valid", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"nbf": testNow.Add(time.Minute).Unix()}}), ReasonNotYetValid},
		{"issued in the future", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"iat": testNow.Add(time.Minute).Unix()}}), ReasonNotYetValid},
		{"missing issued at", h.issuer.token(t, tokenSpec{key: h.key, drop: []string{"iat"}}), ReasonInvalidClaims},
		{"missing expiry", h.issuer.token(t, tokenSpec{key: h.key, drop: []string{"exp"}}), ReasonInvalidClaims},
		{"lifetime above the maximum", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"iat": testNow.Add(-2 * time.Hour).Unix()}}), ReasonLifetimeExceeded},
		{"missing subject", h.issuer.token(t, tokenSpec{key: h.key, drop: []string{"sub"}}), ReasonInvalidClaims},
		{"subject with a space", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"sub": "user 1"}}), ReasonInvalidClaims},
		{"ID token claims in an access token", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"nonce": "n-1"}}), ReasonInvalidClaims},
		{"unbound subject", h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"sub": "someone-else"}}), ReasonUnbound},
	}
	var tokens []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.auth.Authenticate(context.Background(), tc.token, h.tenant)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
			}
			if got := h.lastReason(t); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
		tokens = append(tokens, tc.token)
	}

	t.Run("logs carry no token content", func(t *testing.T) {
		logs := h.logs.String()
		for _, token := range tokens {
			for _, segment := range strings.Split(token, ".") {
				if len(segment) >= 8 && strings.Contains(logs, segment) {
					t.Fatalf("log contains a token segment %q", segment)
				}
			}
		}
		for _, leak := range []string{"eyJ", "user-248289761001", "evil.example", "BEGIN PUBLIC KEY"} {
			if strings.Contains(logs, leak) {
				t.Fatalf("log contains %q:\n%s", leak, logs)
			}
		}
	})
}

func mustMarshalPublic(t *testing.T, key crypto.PublicKey) []byte {
	t.Helper()
	data, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAuthenticateRefusesOtherTenantsWithoutDistinction(t *testing.T) {
	h := newHarness(t, nil)
	token := h.issuer.token(t, tokenSpec{key: h.key})
	otherTenant := mustID[ids.Tenant](t)
	h.bindings[binding{h.issuer.url(), "user-248289761001", otherTenant}] = identity.Principal{Tenant: h.tenant, ID: mustID[ids.Principal](t), Kind: identity.PrincipalHuman}

	_, unboundErr := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{"sub": "nobody"}}), h.tenant)
	_, foreignErr := h.auth.Authenticate(context.Background(), token, mustID[ids.Tenant](t))
	_, mismatchErr := h.auth.Authenticate(context.Background(), token, otherTenant)
	for name, err := range map[string]error{"unbound": unboundErr, "unknown tenant": foreignErr, "principal of another tenant": mismatchErr} {
		if !errors.Is(err, ErrUnauthenticated) || err.Error() != ErrUnauthenticated.Error() {
			t.Fatalf("%s: error = %v, want exactly ErrUnauthenticated", name, err)
		}
	}
}

func TestAuthenticateSurfacesResolverFailures(t *testing.T) {
	h := newHarness(t, nil)
	h.failure = errors.New("database unavailable")
	_, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{key: h.key}), h.tenant)
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v, want an operational failure", err)
	}
}

func TestUntypedIssuerStillRefusesIDTokens(t *testing.T) {
	disabled := false
	h := newHarness(t, func(c *IssuerConfig) { c.RequireAccessTokenType = &disabled })
	if !strings.Contains(h.logs.String(), "access token typing is not required") {
		t.Fatalf("opt-out was not logged at startup: %s", h.logs)
	}
	if _, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{key: h.key, noType: true}), h.tenant); err != nil {
		t.Fatalf("untyped access token refused: %v", err)
	}
	for name, claim := range map[string]string{"nonce": "nonce", "at_hash": "at_hash"} {
		t.Run(name, func(t *testing.T) {
			token := h.issuer.token(t, tokenSpec{key: h.key, typ: "JWT", claims: map[string]any{claim: "x"}})
			if _, err := h.auth.Authenticate(context.Background(), token, h.tenant); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("ID token with %s accepted: %v", claim, err)
			}
		})
	}
}

func TestUnknownKeyIDFloodFetchesKeysAtMostOncePerInterval(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{key: h.key}), h.tenant); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	before := h.issuer.fetches.Load()
	attacker := newRSAKey(t)
	var wait sync.WaitGroup
	for i := range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token := h.issuer.token(t, tokenSpec{kid: fmt.Sprintf("invented-%d", i), key: attacker})
			_, _ = h.auth.Authenticate(context.Background(), token, h.tenant)
		}()
	}
	wait.Wait()
	if extra := h.issuer.fetches.Load() - before; extra > 1 {
		t.Fatalf("64 unknown key IDs caused %d key-set fetches, want at most 1", extra)
	}
}

func TestKeyRotationAndWithdrawal(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{key: h.key}), h.tenant); err != nil {
		t.Fatalf("initial key: %v", err)
	}
	rotated := newRSAKey(t)
	h.issuer.publish("k2", rotated)
	h.advance(2 * time.Minute)
	if _, err := h.auth.Authenticate(context.Background(), h.issuer.token(t, tokenSpec{kid: "k2", key: rotated}), h.tenant); err != nil {
		t.Fatalf("rotated key refused after the refresh interval: %v", err)
	}
	h.issuer.withdraw("k1")
	h.advance(16 * time.Minute)
	old := h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{
		"iat": h.clock.Load().Add(-time.Minute).Unix(), "exp": h.clock.Load().Add(time.Minute).Unix(),
	}})
	if _, err := h.auth.Authenticate(context.Background(), old, h.tenant); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("withdrawn key still verifies after the maximum key age: %v", err)
	}
}

func TestNewRefusesUnsafeDiscovery(t *testing.T) {
	for name, change := range map[string]func(*testIssuer){
		"issuer mismatch":   func(i *testIssuer) { i.discoveredIssuer = "https://elsewhere.example" },
		"plaintext key URL": func(i *testIssuer) { i.keysURL = "http://keys.example/keys" },
	} {
		t.Run(name, func(t *testing.T) {
			issuer := newTestIssuer(t)
			change(issuer)
			config := Config{Issuers: []IssuerConfig{{Issuer: issuer.url(), Audience: testAudience, Algorithms: []string{"RS256"}, MaxTokenLifetimeSeconds: 60}}}
			resolve := func(context.Context, string, string, ids.TenantID) (identity.Principal, bool, error) {
				return identity.Principal{}, false, nil
			}
			if _, err := New(context.Background(), config, resolve, slog.New(slog.DiscardHandler), WithHTTPClient(issuer.server.Client())); err == nil {
				t.Fatal("New() accepted unsafe discovery")
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc.def.ghi":  "abc.def.ghi",
		"bearer abc.def-_~+/": "abc.def-_~+/",
		"BEARER abc==":        "abc==",
	} {
		if got, ok := BearerToken(header); !ok || got != want {
			t.Errorf("BearerToken(%q) = %q, %v", header, got, ok)
		}
	}
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic abc", "Bearer a b", "Bearer abc\n", "Bearer ===", "Bearerabc"} {
		if _, ok := BearerToken(header); ok {
			t.Errorf("BearerToken(%q) accepted", header)
		}
	}
}

// freshToken is a valid token from key k1 issued at the harness's current time.
func (h *harness) freshToken(t *testing.T) string {
	t.Helper()
	now := *h.clock.Load()
	return h.issuer.token(t, tokenSpec{key: h.key, claims: map[string]any{
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(),
	}})
}

// TestWithdrawnKeyStopsVerifyingWhileKeyEndpointIsDown proves cached keys are
// never used past the maximum key age, whether the refresh failed or was
// rate-limited, while still bridging a short outage inside that bound.
func TestWithdrawnKeyStopsVerifyingWhileKeyEndpointIsDown(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.auth.Authenticate(context.Background(), h.freshToken(t), h.tenant); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.issuer.withdraw("k1")
	h.issuer.failKeys.Store(true)

	h.advance(10 * time.Minute)
	if _, err := h.auth.Authenticate(context.Background(), h.freshToken(t), h.tenant); err != nil {
		t.Fatalf("cached keys inside the maximum age stopped working during an outage: %v", err)
	}
	for _, step := range []struct {
		name    string
		advance time.Duration
	}{
		{"at the maximum age, refresh failed", 5 * time.Minute},
		{"immediately again, refresh rate-limited", 0},
		{"a day later", 24 * time.Hour},
	} {
		h.advance(step.advance)
		if _, err := h.auth.Authenticate(context.Background(), h.freshToken(t), h.tenant); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s: withdrawn cached key still verifies: %v", step.name, err)
		}
	}

	h.issuer.failKeys.Store(false)
	rotated := newRSAKey(t)
	h.issuer.publish("k2", rotated)
	h.advance(2 * time.Minute)
	now := *h.clock.Load()
	token := h.issuer.token(t, tokenSpec{kid: "k2", key: rotated, claims: map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix()}})
	if _, err := h.auth.Authenticate(context.Background(), token, h.tenant); err != nil {
		t.Fatalf("recovered endpoint not used again: %v", err)
	}
}

// TestSlowKeyRefreshDoesNotBlockCallersWithUsableKeys proves the fetch runs
// outside the lock: while one caller's refresh is stuck, others verify with
// the cached keys.
func TestSlowKeyRefreshDoesNotBlockCallersWithUsableKeys(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.auth.Authenticate(context.Background(), h.freshToken(t), h.tenant); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.issuer.mu.Lock()
	h.issuer.hold, h.issuer.held = make(chan struct{}), make(chan struct{}, 1)
	hold, held := h.issuer.hold, h.issuer.held
	h.issuer.mu.Unlock()
	h.advance(8 * time.Minute)

	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	refreshed := make(chan error, 1)
	go func() {
		_, err := h.auth.Authenticate(refreshCtx, h.freshToken(t), h.tenant)
		refreshed <- err
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the key endpoint")
	}
	// The caller that started the refresh disconnects; the refresh must go on.
	cancelRefresh()

	done := make(chan error, 1)
	go func() {
		_, err := h.auth.Authenticate(context.Background(), h.freshToken(t), h.tenant)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("caller with usable keys refused during a slow refresh: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller with usable keys waited for another caller's refresh")
	}
	close(hold)
	if err := <-refreshed; err != nil {
		t.Fatalf("refreshing caller: %v", err)
	}
}
