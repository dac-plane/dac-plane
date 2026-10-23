package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	maxKeySetBytes            = 1 << 20
	defaultMinRefreshInterval = time.Minute
	defaultMaxKeyAge          = 15 * time.Minute
)

var errNoMatchingKey = errors.New("no published key verifies the signature")

// keySet verifies signatures with the keys an issuer publishes at its
// discovered jwks_uri, and nowhere else: keys or key URLs named inside a
// token (jwk, jku, x5u) are never used.
//
// The key ID in a token is attacker-controlled, so an unknown key ID may
// trigger at most one refresh per minRefresh across all callers; a flood of
// invented key IDs cannot turn into a flood of requests to the issuer. Keys
// are also refreshed once they are older than maxAge, so a key the issuer
// withdraws stops verifying within that bound.
type keySet struct {
	client     *http.Client
	url        string
	algorithms []jose.SignatureAlgorithm
	now        func() time.Time
	minRefresh time.Duration
	maxAge     time.Duration

	mu          sync.Mutex
	keys        []jose.JSONWebKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// VerifySignature implements oidc.KeySet. It is called by the go-oidc
// verifier after the token's algorithm has been checked against the issuer's
// pinned list.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	signed, err := jose.ParseSignedCompact(raw, k.algorithms)
	if err != nil {
		return nil, err
	}
	if len(signed.Signatures) != 1 {
		return nil, errors.New("exactly one signature is required")
	}
	header := signed.Signatures[0].Header
	keys, err := k.current(ctx, false)
	if err != nil {
		return nil, err
	}
	if payload, err := verifyWith(signed, header, keys); err == nil {
		return payload, nil
	}
	keys, err = k.current(ctx, true)
	if err != nil {
		return nil, err
	}
	return verifyWith(signed, header, keys)
}

// current returns the cached keys, refreshing them first when they are
// missing, older than maxAge, or wanted after a verification failure. A
// refresh is attempted at most once per minRefresh; a failed refresh keeps the
// last good keys.
func (k *keySet) current(ctx context.Context, wanted bool) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	stale := k.fetchedAt.IsZero() || now.Sub(k.fetchedAt) >= k.maxAge
	allowed := k.lastAttempt.IsZero() || now.Sub(k.lastAttempt) >= k.minRefresh
	if (wanted || stale) && allowed {
		k.lastAttempt = now
		keys, err := k.fetch(ctx)
		if err == nil {
			k.keys, k.fetchedAt = keys, now
		} else if k.fetchedAt.IsZero() {
			return nil, err
		}
	}
	if k.fetchedAt.IsZero() {
		return nil, errors.New("issuer keys are not available yet")
	}
	return k.keys, nil
}

func (k *keySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := k.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch issuer keys: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch issuer keys: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxKeySetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read issuer keys: %w", err)
	}
	if len(data) > maxKeySetBytes {
		return nil, errors.New("issuer key set is too large")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decode issuer keys: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, key := range set.Keys {
		// Only public signing keys verify; symmetric or private entries and
		// encryption keys are ignored rather than trusted.
		if key.Valid() && key.IsPublic() && (key.Use == "" || key.Use == "sig") {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// verifyWith tries the published keys that match the token's key ID (all
// keys when it names none) and whose declared algorithm, if any, is the
// token's.
func verifyWith(signed *jose.JSONWebSignature, header jose.Header, keys []jose.JSONWebKey) ([]byte, error) {
	for _, key := range keys {
		if header.KeyID != "" && key.KeyID != header.KeyID {
			continue
		}
		if key.Algorithm != "" && key.Algorithm != header.Algorithm {
			continue
		}
		if payload, err := signed.Verify(key); err == nil {
			return payload, nil
		}
	}
	return nil, errNoMatchingKey
}

func joseAlgorithms(names []string) []jose.SignatureAlgorithm {
	algorithms := make([]jose.SignatureAlgorithm, 0, len(names))
	for _, name := range names {
		if !slices.Contains(algorithms, jose.SignatureAlgorithm(name)) {
			algorithms = append(algorithms, jose.SignatureAlgorithm(name))
		}
	}
	return algorithms
}
