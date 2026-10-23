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
// invented key IDs cannot turn into a flood of requests to the issuer.
//
// Cached keys are refreshed once they reach half of maxAge and are never used
// once they reach maxAge, whether the refresh failed or was rate-limited: a
// key the issuer withdraws stops verifying within maxAge even while the
// issuer's key endpoint is unavailable. Inside that bound the last good keys
// keep authentication working through a failed refresh.
//
// The fetch runs outside the lock with a timeout that does not depend on the
// caller, so a slow or failing endpoint never blocks callers that hold usable
// keys, and one caller disconnecting does not cancel the refresh for others.
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
	// refreshing is closed when the refresh in progress, if any, completes.
	refreshing chan struct{}
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

// current returns usable cached keys, refreshing them when they are missing,
// past half of maxAge, or wanted after a verification failure. At most one
// refresh runs at a time and one starts at most once per minRefresh. A caller
// whose token verified against the cache never waits for a refresh it did not
// start. A caller without usable keys, or one that wants fresh keys because
// its signature did not verify, waits for the refresh in progress and then
// uses its result, so a token signed with a newly published key is not
// refused merely because another request started the refresh.
func (k *keySet) current(ctx context.Context, wanted bool) ([]jose.JSONWebKey, error) {
	for {
		k.mu.Lock()
		now := k.now()
		usable := k.usable(now)
		due := k.fetchedAt.IsZero() || now.Sub(k.fetchedAt) >= k.maxAge/2
		allowed := k.lastAttempt.IsZero() || now.Sub(k.lastAttempt) >= k.minRefresh
		if (wanted || due) && allowed && k.refreshing == nil {
			return k.refresh(ctx, now)
		}
		refreshing := k.refreshing
		if usable && (!wanted || refreshing == nil) {
			keys := k.keys
			k.mu.Unlock()
			return keys, nil
		}
		k.mu.Unlock()
		if refreshing == nil {
			return nil, errKeysUnavailable
		}
		select {
		case <-refreshing:
			// The refresh just completed satisfies a wanted refresh; do not
			// start another one for the same verification failure.
			wanted = false
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

var errKeysUnavailable = errors.New("no issuer keys younger than the maximum key age are available")

// usable reports whether the cached keys may verify at now. Called with mu
// held.
func (k *keySet) usable(now time.Time) bool {
	return !k.fetchedAt.IsZero() && now.Sub(k.fetchedAt) < k.maxAge
}

// refresh fetches the key set with mu released and stores it on success.
// Called with mu held; returns with it released.
func (k *keySet) refresh(ctx context.Context, started time.Time) ([]jose.JSONWebKey, error) {
	done := make(chan struct{})
	k.refreshing, k.lastAttempt = done, started
	k.mu.Unlock()

	fetchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTimeout)
	keys, err := k.fetch(fetchContext)
	cancel()

	k.mu.Lock()
	if err == nil {
		k.keys, k.fetchedAt = keys, started
	}
	k.refreshing = nil
	close(done)
	usable, current := k.usable(k.now()), k.keys
	k.mu.Unlock()
	switch {
	case usable:
		return current, nil
	case err != nil:
		return nil, err
	default:
		return nil, errKeysUnavailable
	}
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
