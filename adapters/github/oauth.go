package githubadapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
	bolt "go.etcd.io/bbolt"
)

func (a *Adapter) begin(r wire.Request, id string, expiry time.Time) (wire.Reply, error) {
	now := time.Now().UTC()
	if !expiry.After(now) || expiry.After(now.Add(11*time.Minute)) {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	var current connection
	if err := a.cfg.Vault.load(connectionKey(r.Tenant, id), &current); err != nil && !errors.Is(err, errAbsent) {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	if current.State == "removing" || current.State == "removed" {
		return failed(r, false, wire.FailureUnauthorized), nil
	}
	if len(current.Previous) >= 8 {
		return failed(r, false, wire.FailureResourceBusy), nil
	}
	state, err := random()
	if err != nil {
		return wire.Reply{}, err
	}
	verifier, err := random()
	if err != nil {
		return wire.Reply{}, err
	}
	stateKey := "state:" + digest([]byte(state))
	pending := consent{Tenant: r.Tenant, Connection: id, Verifier: verifier, ExpiresAt: expiry}
	err = a.cfg.Vault.db.Update(func(tx *bolt.Tx) error {
		if current.PendingState != "" {
			if err := tx.Bucket(bucket).Delete([]byte(current.PendingState)); err != nil {
				return err
			}
		}
		current.PendingState = stateKey
		if current.State == "" {
			current.State = "authorizing"
		}
		if err := a.cfg.Vault.write(tx, stateKey, pending); err != nil {
			return err
		}
		return a.cfg.Vault.write(tx, connectionKey(r.Tenant, id), current)
	})
	if err != nil {
		return wire.Reply{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {a.cfg.ClientID}, "redirect_uri": {a.cfg.CallbackURL}, "scope": {"offline_access"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	return accepted(r, map[string]any{"authorization_url": a.cfg.OAuthBase + "/login/oauth/authorize?" + query.Encode(), "expires_at": expiry}), nil
}

// Callback is the fixed GitHub App OAuth callback. Consent state is random,
// bound to its pending connection, short-lived and consumed before exchange.
func (a *Adapter) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if len(state) > 128 || len(state) < 32 || code == "" || len(code) > 1024 {
		http.Error(w, "invalid authorization callback", 400)
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	key := "state:" + digest([]byte(state))
	var pending consent
	var current connection
	err := a.cfg.Vault.db.Update(func(tx *bolt.Tx) error {
		if err := a.cfg.Vault.read(tx, key, &pending); err != nil {
			return err
		}
		if err := a.cfg.Vault.read(tx, connectionKey(pending.Tenant, pending.Connection), &current); err != nil {
			return err
		}
		if current.PendingState != key || current.State == "removed" || current.State == "removing" || !time.Now().Before(pending.ExpiresAt) {
			return errors.New("consent is unavailable")
		}
		return tx.Bucket(bucket).Delete([]byte(key))
	})
	if err != nil {
		http.Error(w, "consent expired or already used", 400)
		return
	}
	credential, err := a.exchange(r.Context(), url.Values{"code": {code}, "redirect_uri": {a.cfg.CallbackURL}, "code_verifier": {pending.Verifier}})
	if err != nil {
		http.Error(w, "authorization could not be completed; start consent again", 502)
		return
	}
	// Persist every issued credential before another network request. Renewal
	// cannot discard an older still-live token, including after a process crash.
	credential.Previous = append(credential.Previous, current.Previous...)
	if current.AccessToken != "" {
		credential.Previous = append(credential.Previous, oldCredential{AccessToken: current.AccessToken, RefreshToken: current.RefreshToken})
	}
	credential.State = "authorizing"
	if err := a.cfg.Vault.save(connectionKey(pending.Tenant, pending.Connection), credential); err != nil {
		http.Error(w, "cannot store authorization", 500)
		return
	}
	// User and installation reports are safe projections. Neither token ever
	// enters a callback response or a control-plane protocol value.
	var user struct {
		Login string `json:"login"`
	}
	if _, err := a.api(r.Context(), credential.AccessToken, "GET", "/user", nil, &user); err != nil || user.Login == "" {
		// Retain the obtained credential so an owner can still revoke it, even
		// when account discovery failed after a successful code exchange.
		credential.State = "authorizing"
		_ = a.cfg.Vault.save(connectionKey(pending.Tenant, pending.Connection), credential)
		http.Error(w, "account could not be read; retry consent or remove connection", 502)
		return
	}
	var installations struct {
		Installations []struct {
			Permissions map[string]string `json:"permissions"`
		} `json:"installations"`
	}
	permissions := map[string]bool{}
	if _, err := a.api(r.Context(), credential.AccessToken, "GET", "/user/installations?per_page=100", nil, &installations); err == nil {
		for _, installation := range installations.Installations {
			for name, level := range installation.Permissions {
				permissions[name+":"+level] = true
			}
		}
	}
	credential.Account = user.Login
	credential.Permissions = []string{}
	for permission := range permissions {
		credential.Permissions = append(credential.Permissions, permission)
	}
	sort.Strings(credential.Permissions)
	confirmation, err := random()
	if err != nil {
		http.Error(w, "cannot prepare confirmation", 500)
		return
	}
	credential.State = "awaiting_confirmation"
	credential.ConfirmationDigest = digest([]byte(confirmation))
	credential.ConfirmationExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	if err := a.cfg.Vault.save(connectionKey(pending.Tenant, pending.Connection), credential); err != nil {
		http.Error(w, "cannot store authorization", 500)
		return
	}
	if a.cfg.ReturnURL != "" {
		destination, _ := url.Parse(a.cfg.ReturnURL)
		destination.Fragment = url.Values{"connection_id": {pending.Connection}, "confirmation": {confirmation}}.Encode()
		http.Redirect(w, r, destination.String(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"connection_id": pending.Connection, "confirmation": confirmation, "message": "Complete this connection in your authenticated surface session"})

}
func (a *Adapter) exchange(ctx context.Context, form url.Values) (connection, error) {
	form.Set("client_id", a.cfg.ClientID)
	form.Set("client_secret", a.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, "POST", a.cfg.OAuthBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return connection{}, errors.New("cannot prepare token exchange")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return connection{}, errors.New("token exchange unavailable")
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Type             string `json:"token_type"`
		Error            string `json:"error"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&token) != nil || token.Error != "" || token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 || token.RefreshExpiresIn <= 0 || token.Type != "bearer" {
		return connection{}, errors.New("token exchange refused")
	}
	now := time.Now().UTC()
	return connection{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: now.Add(time.Duration(token.ExpiresIn) * time.Second), RefreshExpiresAt: now.Add(time.Duration(token.RefreshExpiresIn) * time.Second)}, nil
}
func (a *Adapter) credential(ctx context.Context, tenant, id string) (connection, error) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	var c connection
	if err := a.cfg.Vault.load(connectionKey(tenant, id), &c); err != nil {
		return c, err
	}
	if c.State != "active" {
		return c, errors.New("connection is not active")
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return c, nil
	}
	if !time.Now().Before(c.RefreshExpiresAt) {
		return c, errors.New("consent must be renewed")
	}
	next, err := a.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}})
	if err != nil {
		return c, err
	}
	next.State, next.Account, next.Permissions, next.PendingState = c.State, c.Account, c.Permissions, c.PendingState
	next.Previous = c.Previous
	next.ConfirmationDigest, next.ConfirmationExpiresAt = c.ConfirmationDigest, c.ConfirmationExpiresAt
	return next, a.cfg.Vault.save(connectionKey(tenant, id), next)
}
func (a *Adapter) revoke(ctx context.Context, r wire.Request, id string) (wire.Reply, error) {
	key := connectionKey(r.Tenant, id)
	var c connection
	if err := a.cfg.Vault.load(key, &c); err != nil && !errors.Is(err, errAbsent) {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	if c.State == "removed" {
		return accepted(r, struct{}{}), nil
	}
	c.State = "removing"
	if err := a.cfg.Vault.db.Update(func(tx *bolt.Tx) error {
		if c.PendingState != "" {
			if err := tx.Bucket(bucket).Delete([]byte(c.PendingState)); err != nil {
				return err
			}
		}
		return a.cfg.Vault.write(tx, key, c)
	}); err != nil {
		return wire.Reply{}, err
	}
	for _, access := range append([]oldCredential{{AccessToken: c.AccessToken, RefreshToken: c.RefreshToken}}, c.Previous...) {
		if access.AccessToken == "" {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"access_token": access.AccessToken})
		req, err := http.NewRequestWithContext(ctx, "DELETE", a.cfg.APIBase+"/applications/"+url.PathEscape(a.cfg.ClientID)+"/token", strings.NewReader(string(raw)))
		if err != nil {
			return failed(r, true, wire.FailureProviderFailed), nil
		}
		req.SetBasicAuth(a.cfg.ClientID, a.cfg.ClientSecret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := a.cfg.HTTP.Do(req)
		if err != nil {
			return failed(r, true, wire.FailureTransportUnavailable), nil
		}
		resp.Body.Close()
		if resp.StatusCode != 204 && resp.StatusCode != 404 {
			return failed(r, true, wire.FailureProviderFailed), nil
		}
	}
	// Delete the refresh token and pending state only after GitHub confirms
	// revocation. Repeated removal sees this durable credential-free tombstone.
	if err := a.cfg.Vault.save(key, connection{State: "removed", Permissions: []string{}}); err != nil {
		return wire.Reply{}, err
	}
	return accepted(r, struct{}{}), nil
}
