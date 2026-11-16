// Package githubadapter implements the connector role with a GitHub App user
// authorization. External credentials are held only in its sealed vault.
package githubadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
	bolt "go.etcd.io/bbolt"
)

const maxOutput = 32 << 10

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

type Config struct {
	Vault                                                                *Vault
	TenantID, ProviderID, ClientID, ClientSecret, CallbackURL, ReturnURL string
	APIBase, OAuthBase                                                   string
	HTTP                                                                 *http.Client
}
type Adapter struct {
	cfg Config
	// authMu serializes consent, refresh and removal, so no late callback or
	// refreshed credential can resurrect a removing connection.
	authMu  sync.Mutex
	callsMu sync.Mutex
	active  map[string]context.CancelFunc
}
type oldCredential struct{ AccessToken, RefreshToken string }

type connection struct {
	State, AccessToken, RefreshToken, Account, PendingState string
	ExpiresAt, RefreshExpiresAt                             time.Time
	Permissions                                             []string
	Previous                                                []oldCredential
	ConfirmationDigest                                      string
	ConfirmationExpiresAt                                   time.Time
}
type consent struct {
	Tenant, Connection, Verifier string
	ExpiresAt                    time.Time
}
type callRecord struct {
	Hash, Tenant, Attempt, State string
	Result                       *callResult
}
type actionRequest struct {
	Tenant      string                         `json:"tenant_id"`
	Attempt     string                         `json:"attempt_id"`
	Provider    string                         `json:"provider_id"`
	Operation   string                         `json:"operation_id"`
	Action      string                         `json:"action"`
	Scope       struct{ Class, Target string } `json:"scope"`
	InputDigest string                         `json:"input_digest"`
}
type execution struct {
	Request         actionRequest   `json:"request"`
	Grant           string          `json:"grant_id"`
	Decision        string          `json:"decision_id"`
	Source          string          `json:"source"`
	ConnectionGrant string          `json:"connection_grant_id"`
	Connection      string          `json:"connection_id"`
	Tool            string          `json:"tool"`
	Input           json.RawMessage `json:"input"`
}
type callResult struct {
	Operation   string          `json:"operation_id"`
	Output      json.RawMessage `json:"output"`
	Digest      string          `json:"output_digest"`
	IsError     bool            `json:"is_error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

func New(cfg Config) (*Adapter, error) {
	if cfg.Vault == nil || !uuid.MatchString(cfg.ProviderID) || !uuid.MatchString(cfg.TenantID) || cfg.ClientID == "" || cfg.ClientSecret == "" || !safeURL(cfg.CallbackURL) {
		return nil, errors.New("connector configuration is incomplete")
	}
	callback, _ := url.Parse(cfg.CallbackURL)
	if callback.Path == "" {
		return nil, errors.New("callback URL requires a path")
	}
	if cfg.ReturnURL != "" && !safeURL(cfg.ReturnURL) {
		return nil, errors.New("invalid surface return URL")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	if cfg.OAuthBase == "" {
		cfg.OAuthBase = "https://github.com"
	}
	if !safeURL(cfg.APIBase) || !safeURL(cfg.OAuthBase) {
		return nil, errors.New("invalid GitHub origin")
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	cfg.OAuthBase = strings.TrimRight(cfg.OAuthBase, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg.HTTP != nil {
		*client = *cfg.HTTP
	}
	// Never forward credential-bearing requests through redirects, even to a
	// same-origin URL. A changed upstream endpoint is an explicit deployment edit.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = client
	return &Adapter{cfg: cfg, active: map[string]context.CancelFunc{}}, nil
}
func safeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))
}
func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func random() (string, error) {
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw), err
}
func strict(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid connector arguments")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid connector arguments")
	}
	return nil
}
func connectionKey(tenant, id string) string { return "connection:" + tenant + ":" + id }
func callKey(tenant, id string) string       { return "call:" + tenant + ":" + id }
func accepted(r wire.Request, value any) wire.Reply {
	raw, _ := json.Marshal(value)
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted, Result: raw}
}
func failed(r wire.Request, unknown bool, code string) wire.Reply {
	durability := wire.DurabilityRejected
	if unknown {
		durability = wire.DurabilityUnknown
	}
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeFailed, Durability: durability, Failure: &wire.Failure{Code: code, Message: "connector request could not be completed"}}
}

func (a *Adapter) Send(ctx context.Context, r wire.Request) (wire.Reply, error) {
	if r.Tenant != a.cfg.TenantID {
		return failed(r, false, wire.FailureUnauthorized), nil
	}
	if r.Role != wire.RoleConnector {
		return failed(r, false, wire.FailureUnknownOperation), nil
	}
	if err := ctx.Err(); err != nil {
		return failed(r, true, wire.FailureCancelled), nil
	}
	if r.Method == "list_tools" {
		return accepted(r, catalogue()), nil
	}
	if r.Method == "call_tool" {
		return a.call(ctx, r)
	}
	if r.Method == "read_call" {
		var record callRecord
		if err := a.cfg.Vault.load(callKey(r.Tenant, r.Operation), &record); err != nil || record.Tenant != r.Tenant || record.Attempt != r.Attempt || record.Result == nil {
			return failed(r, true, wire.FailureUnknownOperation), nil
		}
		return accepted(r, record.Result), nil
	}
	var args struct {
		Connection   string    `json:"connection_id"`
		ExpiresAt    time.Time `json:"expires_at,omitempty"`
		Confirmation string    `json:"confirmation,omitempty"`
	}
	if strict(r.Arguments, &args) != nil || !uuid.MatchString(args.Connection) {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	if r.Method == "complete_authorization" && !validConfirmation(args.Confirmation) {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	if r.Method != "complete_authorization" && args.Confirmation != "" {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	switch r.Method {
	case "begin_authorization":
		return a.begin(r, args.Connection, args.ExpiresAt)
	case "complete_authorization":
		var c connection
		if err := a.cfg.Vault.load(connectionKey(r.Tenant, args.Connection), &c); err != nil && !errors.Is(err, errAbsent) {
			return failed(r, true, wire.FailureProviderFailed), nil
		}
		if (c.State == "active" || c.State == "awaiting_confirmation") && sameDigest(c.ConfirmationDigest, digest([]byte(args.Confirmation))) && (c.State == "active" || time.Now().Before(c.ConfirmationExpiresAt)) {
			c.State = "active"
			if err := a.cfg.Vault.save(connectionKey(r.Tenant, args.Connection), c); err != nil {
				return failed(r, true, wire.FailureProviderFailed), nil
			}
			return accepted(r, map[string]any{"status": "authorized", "account": map[string]any{"account": c.Account, "permissions": c.Permissions}}), nil
		}
		status := "pending"
		if c.State == "removed" || c.State == "removing" {
			status = "failed"
		}
		return accepted(r, map[string]string{"status": status}), nil
	case "revoke_connection":
		return a.revoke(ctx, r, args.Connection)
	default:
		return failed(r, false, wire.FailureUnknownOperation), nil
	}
}
func (a *Adapter) Subscribe(ctx context.Context, cursor wire.Cursor) (adapterkit.Stream, error) {
	return emptyStream{}, nil
}

type emptyStream struct{}

func (emptyStream) Next(ctx context.Context) (wire.StreamFrame, error) {
	<-ctx.Done()
	return wire.StreamFrame{}, ctx.Err()
}
func (emptyStream) Close() error { return nil }
func (a *Adapter) Cancel(ctx context.Context, c wire.Cancel) error {
	a.callsMu.Lock()
	defer a.callsMu.Unlock()
	// Cancel frames carry no tenant. Resolve only locally recorded operations;
	// the kit connection's authenticated registration owns this process.
	if stop := a.active[c.Operation]; stop != nil {
		stop()
	}
	return a.cfg.Vault.db.Update(func(tx *bolt.Tx) error {
		// The durable key is operation-only for a cancellation tombstone, while
		// call records retain tenant and attempt binding for every observation.
		if err := a.cfg.Vault.write(tx, "cancel:"+c.Operation, true); err != nil {
			return err
		}
		key := callKey(a.cfg.TenantID, c.Operation)
		var record callRecord
		err := a.cfg.Vault.read(tx, key, &record)
		if errors.Is(err, errAbsent) {
			return nil
		}
		if err != nil {
			return err
		}
		if record.State == "preparing" && a.active[c.Operation] == nil {
			raw := json.RawMessage(`{"cancelled":true,"external_request_sent":false}`)
			record.State = "completed"
			record.Result = &callResult{Operation: c.Operation, Output: raw, Digest: digest(raw), IsError: true, CompletedAt: time.Now().UTC()}
			return a.cfg.Vault.write(tx, key, record)
		}
		return nil
	})
}

func validConfirmation(p string) bool {
	if len(p) < 32 || len(p) > 128 {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func sameDigest(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
