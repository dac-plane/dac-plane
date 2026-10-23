// Package oidctest serves an OpenID Connect issuer over TLS from the test
// process: discovery metadata and a key set the test can rotate, fail or
// stall, plus helpers that sign access tokens for it. It is test support only.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer is a test OpenID Connect issuer.
type Issuer struct {
	server   *httptest.Server
	mu       sync.Mutex
	keys     map[string]crypto.Signer
	fetches  atomic.Int32
	failKeys atomic.Bool
	hold     chan struct{}
	held     chan struct{}

	// DiscoveredIssuer overrides the issuer discovery reports; KeysURL
	// overrides the published jwks_uri. Set them before discovery runs.
	DiscoveredIssuer string
	KeysURL          string
	// Audience, Subject and Now shape the default claims of Token.
	Audience string
	Subject  string
	Now      func() time.Time
}

// New starts an issuer; it stops when the test ends.
func New(t testing.TB) *Issuer {
	t.Helper()
	issuer := &Issuer{keys: map[string]crypto.Signer{}, Subject: "user-1", Audience: "https://dac-plane.example/api", Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		discovered := issuer.DiscoveredIssuer
		if discovered == "" {
			discovered = issuer.server.URL
		}
		keysURL := issuer.KeysURL
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

// URL is the issuer identifier.
func (i *Issuer) URL() string { return i.server.URL }

// Client trusts the issuer's test certificate.
func (i *Issuer) Client() *http.Client { return i.server.Client() }

// Publish adds a signing key to the key set under kid.
func (i *Issuer) Publish(kid string, signer crypto.Signer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys[kid] = signer
}

// Withdraw removes kid from the key set.
func (i *Issuer) Withdraw(kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.keys, kid)
}

// Fetches counts requests to the key endpoint.
func (i *Issuer) Fetches() int32 { return i.fetches.Load() }

// FailKeys makes the key endpoint answer 500 while failing is true.
func (i *Issuer) FailKeys(failing bool) { i.failKeys.Store(failing) }

// HoldKeys blocks the key endpoint until Release(hold); each blocked request
// is announced on held. Cleanup releases it, so a failing test cannot hang
// the server shutdown.
func (i *Issuer) HoldKeys(t testing.TB) (hold, held chan struct{}) {
	t.Helper()
	i.mu.Lock()
	i.hold, i.held = make(chan struct{}), make(chan struct{}, 8)
	hold, held = i.hold, i.held
	i.mu.Unlock()
	t.Cleanup(func() { Release(hold) })
	return hold, held
}

// Release unblocks a held key endpoint; releasing twice is harmless.
func Release(hold chan struct{}) {
	select {
	case <-hold:
	default:
		close(hold)
	}
}

// NewRSAKey generates a 2048-bit RSA key.
func NewRSAKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// NewECKey generates a P-256 key.
func NewECKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Spec describes one signed token. Zero values take the defaults of a valid
// access token: key ID "k1", RS256, typ at+jwt.
type Spec struct {
	KeyID     string
	Key       any
	Algorithm jose.SignatureAlgorithm
	Type      string
	NoType    bool
	EmbedJWK  bool
	Claims    map[string]any
	Drop      []string
}

// DefaultClaims are the claims of a valid access token issued at Now.
func (i *Issuer) DefaultClaims() map[string]any {
	now := i.Now()
	return map[string]any{
		"iss": i.URL(), "sub": i.Subject, "aud": i.Audience,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"jti": "id-1", "client_id": "surface-client",
	}
}

// Token signs DefaultClaims, changed by spec, as a compact JWS.
func (i *Issuer) Token(t testing.TB, spec Spec) string {
	t.Helper()
	claims := i.DefaultClaims()
	for name, value := range spec.Claims {
		claims[name] = value
	}
	for _, name := range spec.Drop {
		delete(claims, name)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return Sign(t, spec, payload).Compact
}

// Signed is one signed payload in both serializations.
type Signed struct {
	Compact, Full string
}

// Sign signs payload as spec describes.
func Sign(t testing.TB, spec Spec, payload []byte) Signed {
	t.Helper()
	if spec.KeyID == "" {
		spec.KeyID = "k1"
	}
	if spec.Algorithm == "" {
		spec.Algorithm = jose.RS256
	}
	options := &jose.SignerOptions{EmbedJWK: spec.EmbedJWK}
	if !spec.NoType {
		typ := spec.Type
		if typ == "" {
			typ = "at+jwt"
		}
		options = options.WithType(jose.ContentType(typ))
	}
	key := jose.SigningKey{Algorithm: spec.Algorithm, Key: jose.JSONWebKey{Key: spec.Key, KeyID: spec.KeyID}}
	if spec.EmbedJWK {
		key.Key = spec.Key
	}
	if spec.Algorithm == jose.HS256 {
		key.Key = spec.Key
		options = options.WithHeader(jose.HeaderKey("kid"), spec.KeyID)
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
	return Signed{Compact: compact, Full: object.FullSerialize()}
}
