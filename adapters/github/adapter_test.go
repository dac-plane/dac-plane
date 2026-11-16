package githubadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit"
	"github.com/dac-plane/dac-plane/adapterkit/conformance"
	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

const tenant = "01900000-0000-7000-8000-000000000001"
const provider = "01900000-0000-7000-8000-000000000002"
const attempt = "01900000-0000-7000-8000-000000000003"
const operation = "01900000-0000-7000-8000-000000000004"
const connectionID = "01900000-0000-7000-8000-000000000005"
const delivery = "01900000-0000-7000-8000-000000000006"
const externalSecret = "ghu_canary_external_credential_never_return"
const refreshSecret = "ghr_canary_refresh_credential_never_return"

type fixture struct {
	adapter                   *Adapter
	vault                     *Vault
	server                    *httptest.Server
	path                      string
	mu                        sync.Mutex
	calls, refreshes, revokes int
	verifier                  string
	failComment               bool
	secretTitle               bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{path: filepath.Join(t.TempDir(), "vault.db")}
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_secret") != "client-secret" {
				t.Error("missing client secret")
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != refreshSecret {
					t.Error("wrong refresh token")
				}
				f.refreshes++
			} else {
				f.verifier = r.Form.Get("code_verifier")
			}
			io.WriteString(w, `{"access_token":"`+externalSecret+`","refresh_token":"`+refreshSecret+`","expires_in":28800,"refresh_token_expires_in":15897600,"token_type":"bearer"}`)
		case r.URL.Path == "/applications/app/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "app" || pass != "client-secret" {
				t.Error("wrong revoke auth")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["access_token"] != externalSecret {
				t.Error("wrong revoke token")
			}
			f.revokes++
			w.WriteHeader(204)
		case r.URL.Path == "/user":
			if r.Header.Get("Authorization") != "Bearer "+externalSecret {
				t.Error("wrong API authorization")
			}
			io.WriteString(w, `{"login":"octocat","token":"`+externalSecret+`"}`)
		case r.URL.Path == "/user/installations":
			io.WriteString(w, `{"installations":[{"permissions":{"issues":"write","metadata":"read"}}]}`)
		case r.URL.Path == "/repos/acme/project/issues/1/comments":
			f.calls++
			if f.failComment {
				w.WriteHeader(500)
				io.WriteString(w, `{"message":"`+externalSecret+`"}`)
				return
			}
			io.WriteString(w, `{"id":42,"html_url":"https://github.com/acme/project/issues/1#issuecomment-42","token":"`+externalSecret+`"}`)
		case r.URL.Path == "/repos/acme/project/issues/1":
			f.calls++
			if f.secretTitle {
				io.WriteString(w, `{"number":1,"title":"`+externalSecret+`","state":"open"}`)
				return
			}
			io.WriteString(w, `{"number":1,"title":"<safe> issue","state":"open","html_url":"https://github.com/acme/project/issues/1","access_token":"`+externalSecret+`"}`)
		case r.URL.Path == "/user/repos":
			f.calls++
			io.WriteString(w, `[{"full_name":"acme/project","private":true,"html_url":"https://github.com/acme/project","token":"`+externalSecret+`"}]`)
		default:
			w.WriteHeader(404)
		}
	})
	f.server = httptest.NewServer(fake)
	t.Cleanup(f.server.Close)
	vault, err := OpenVault(f.path, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	f.vault = vault
	t.Cleanup(func() { _ = f.vault.Close() })
	f.adapter = f.newAdapter(t)
	return f
}
func (f *fixture) newAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New(Config{Vault: f.vault, TenantID: tenant, ProviderID: provider, ClientID: "app", ClientSecret: "client-secret", CallbackURL: "https://connector.example/oauth/callback", APIBase: f.server.URL, OAuthBase: f.server.URL, HTTP: f.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func request(method string, value any) wire.Request {
	var raw json.RawMessage
	if value != nil {
		raw, _ = json.Marshal(value)
	}
	r := wire.Request{Operation: operation, Delivery: delivery, Tenant: tenant, Role: wire.RoleConnector, Method: method, Arguments: raw, Demand: "person", Deadline: time.Now().UTC().Add(time.Minute)}
	if method == "call_tool" || method == "read_call" {
		r.Attempt = attempt
	}
	return r
}
func (f *fixture) authorize(t *testing.T) {
	t.Helper()
	r := request("begin_authorization", map[string]any{"connection_id": connectionID, "expires_at": time.Now().UTC().Add(time.Minute)})
	reply, err := f.adapter.Send(context.Background(), r)
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatalf("begin: %+v %v", reply, err)
	}
	var started struct {
		URL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(reply.Result, &started); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(started.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("scope") != "offline_access" {
		t.Fatal("missing PKCE / refresh consent")
	}
	callback := "https://connector.example/oauth/callback?state=" + url.QueryEscape(parsed.Query().Get("state")) + "&code=fixture"
	w := httptest.NewRecorder()
	f.adapter.Callback(w, httptest.NewRequest("GET", callback, nil))
	if w.Code != 200 {
		t.Fatalf("callback = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), externalSecret) || strings.Contains(w.Body.String(), refreshSecret) {
		t.Fatal("callback leaked credential")
	}
	sum := sha256.Sum256([]byte(f.verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != parsed.Query().Get("code_challenge") {
		t.Fatal("PKCE verifier mismatch")
	}
	replay := httptest.NewRecorder()
	f.adapter.Callback(replay, httptest.NewRequest("GET", callback, nil))
	if replay.Code != 400 {
		t.Fatal("consent state replay accepted")
	}
	var confirmed struct {
		Confirmation string `json:"confirmation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &confirmed); err != nil {
		t.Fatal(err)
	}
	// Polling the attacker's original transaction reveals no completion proof.
	wrong, err := f.adapter.Send(context.Background(), request("complete_authorization", map[string]string{"connection_id": connectionID, "confirmation": strings.Repeat("a", 32)}))
	if err != nil || bytes.Contains(wrong.Result, []byte(`"status":"authorized"`)) {
		t.Fatal("forwarded-URL consent activated without browser proof")
	}
	complete, err := f.adapter.Send(context.Background(), request("complete_authorization", map[string]string{"connection_id": connectionID, "confirmation": confirmed.Confirmation}))
	if err != nil || !bytes.Contains(complete.Result, []byte(`"status":"authorized"`)) {
		t.Fatalf("complete = %+v %v", complete, err)
	}
	if bytes.Contains(complete.Result, []byte(externalSecret)) {
		t.Fatal("complete leaked credential")
	}
}
func toolRequest(name string) wire.Request {
	input := json.RawMessage(`{"owner":"acme","repo":"project","number":1}`)
	if name == "create_comment" {
		input = json.RawMessage(`{"owner":"acme","repo":"project","number":1,"body":"Safe comment"}`)
	}
	if name == "list_repositories" {
		input = json.RawMessage(`{}`)
	}
	e := execution{Request: actionRequest{Tenant: tenant, Attempt: attempt, Provider: provider, Operation: operation, Action: "connector." + name, InputDigest: digest(input)}, Grant: delivery, Source: "standing_grant", ConnectionGrant: delivery, Connection: connectionID, Tool: name, Input: input}
	e.Request.Scope.Class = "subject"
	e.Request.Scope.Target = "connection:" + connectionID
	return request("call_tool", e)
}
func TestConsentSealsCredentialsAndProjectsTools(t *testing.T) {
	for _, tool := range []string{"get_issue", "create_comment", "list_repositories"} {
		t.Run(tool, func(t *testing.T) {
			f := setup(t)
			f.authorize(t)
			r := toolRequest(tool)
			first, err := f.adapter.Send(context.Background(), r)
			if err != nil || first.Outcome != wire.OutcomeOK {
				t.Fatalf("call = %+v %v", first, err)
			}
			second, err := f.adapter.Send(context.Background(), r)
			if err != nil || !bytes.Equal(first.Result, second.Result) {
				t.Fatal("redelivery changed output")
			}
			f.mu.Lock()
			calls := f.calls
			f.mu.Unlock()
			if calls != 1 {
				t.Fatalf("effect count = %d", calls)
			}
			raw, _ := json.Marshal(first)
			if bytes.Contains(raw, []byte(externalSecret)) || bytes.Contains(raw, []byte(refreshSecret)) {
				t.Fatal("credential in reply")
			}
			var result callResult
			if err := json.Unmarshal(first.Result, &result); err != nil {
				t.Fatal(err)
			}
			if digest(result.Output) != result.Digest {
				t.Fatal("wire changed output digest")
			}
			// A process restart replays the completed call without another GitHub request.
			if err := f.vault.Close(); err != nil {
				t.Fatal(err)
			}
			f.vault, err = OpenVault(f.path, bytes.Repeat([]byte("k"), 32))
			if err != nil {
				t.Fatal(err)
			}
			f.adapter = f.newAdapter(t)
			resumed, err := f.adapter.Send(context.Background(), r)
			if err != nil || !bytes.Equal(resumed.Result, first.Result) {
				t.Fatal("restart repeated or lost completed call")
			}
			read, err := f.adapter.Send(context.Background(), request("read_call", nil))
			if err != nil || !bytes.Equal(read.Result, first.Result) {
				t.Fatal("result lookup failed")
			}
			disk, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{externalSecret, refreshSecret, "client-secret"} {
				if bytes.Contains(disk, []byte(secret)) {
					t.Fatal("plaintext secret in vault")
				}
			}
			info, err := os.Stat(f.path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("vault permissions")
			}
		})
	}
}
func TestUnknownEffectIsNeverRepeated(t *testing.T) {
	f := setup(t)
	f.authorize(t)
	f.failComment = true
	r := toolRequest("create_comment")
	for range 2 {
		reply, err := f.adapter.Send(context.Background(), r)
		if err != nil || reply.Durability != wire.DurabilityUnknown {
			t.Fatalf("uncertain = %+v %v", reply, err)
		}
	}
	read, err := f.adapter.Send(context.Background(), request("read_call", nil))
	if err != nil || read.Durability != wire.DurabilityUnknown {
		t.Fatal("unknown outcome guessed")
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatal("uncertain effect was repeated")
	}
}
func TestRefreshCancellationAndRevocation(t *testing.T) {
	f := setup(t)
	f.authorize(t)
	var c connection
	if err := f.vault.load(connectionKey(tenant, connectionID), &c); err != nil {
		t.Fatal(err)
	}
	c.ExpiresAt = time.Now().Add(-time.Minute)
	if err := f.vault.save(connectionKey(tenant, connectionID), c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.adapter.credential(context.Background(), tenant, connectionID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	refreshes := f.refreshes
	f.mu.Unlock()
	if refreshes != 1 {
		t.Fatal("refresh did not run")
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: operation, Delivery: delivery}); err != nil {
		t.Fatal(err)
	}
	reply, err := f.adapter.Send(context.Background(), toolRequest("create_comment"))
	if err != nil || !bytes.Contains(reply.Result, []byte(`"cancelled":true`)) {
		t.Fatalf("cancelled call = %+v %v", reply, err)
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 0 {
		t.Fatal("cancelled call had an effect")
	}
	for range 2 {
		reply, err := f.adapter.Send(context.Background(), request("revoke_connection", map[string]string{"connection_id": connectionID}))
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatal("revocation failed")
		}
	}
	if err := f.vault.load(connectionKey(tenant, connectionID), &c); err != nil || c.AccessToken != "" || c.RefreshToken != "" || c.State != "removed" {
		t.Fatal("credential not destroyed")
	}
	f.mu.Lock()
	revokes := f.revokes
	f.mu.Unlock()
	if revokes != 1 {
		t.Fatal("revocation replay changed remote state")
	}
}
func TestRejectsChangedAuthorityAndForeignTenant(t *testing.T) {
	f := setup(t)
	f.authorize(t)
	r := toolRequest("get_issue")
	r.Tenant = provider
	reply, err := f.adapter.Send(context.Background(), r)
	if err != nil || reply.Durability != wire.DurabilityRejected {
		t.Fatal("foreign tenant accepted")
	}
	r = toolRequest("get_issue")
	var e execution
	if err := json.Unmarshal(r.Arguments, &e); err != nil {
		t.Fatal(err)
	}
	e.Request.InputDigest = digest([]byte("changed"))
	r.Arguments, _ = json.Marshal(e)
	reply, err = f.adapter.Send(context.Background(), r)
	if err != nil || reply.Durability != wire.DurabilityRejected {
		t.Fatal("changed input accepted")
	}
}

func TestRestartCancellationSettlesPreparingCallWithoutEffect(t *testing.T) {
	f := setup(t)
	record := callRecord{Hash: "preparing", Tenant: tenant, Attempt: attempt, State: "preparing"}
	if err := f.vault.save(callKey(tenant, operation), record); err != nil {
		t.Fatal(err)
	}
	if err := f.adapter.Cancel(context.Background(), wire.Cancel{Operation: operation, Delivery: delivery}); err != nil {
		t.Fatal(err)
	}
	reply, err := f.adapter.Send(context.Background(), request("read_call", nil))
	if err != nil || reply.Durability != wire.DurabilityAccepted || !bytes.Contains(reply.Result, []byte(`"external_request_sent":false`)) {
		t.Fatal("known unsent cancellation did not recover")
	}
}

func TestRenewalRetainsBothCredentialsUntilRevocation(t *testing.T) {
	f := setup(t)
	f.authorize(t)
	f.authorize(t)
	var c connection
	if err := f.vault.load(connectionKey(tenant, connectionID), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Previous) != 1 || c.Previous[0].AccessToken != externalSecret {
		t.Fatal("renewal discarded previous credential")
	}
	reply, err := f.adapter.Send(context.Background(), request("revoke_connection", map[string]string{"connection_id": connectionID}))
	if err != nil || reply.Outcome != wire.OutcomeOK {
		t.Fatal("renewed connection could not be removed")
	}
	f.mu.Lock()
	revokes := f.revokes
	f.mu.Unlock()
	if revokes != 2 {
		t.Fatalf("revoked %d credential generations, want 2", revokes)
	}
}

func TestCredentialCannotLeakThroughAProjectedField(t *testing.T) {
	f := setup(t)
	f.authorize(t)
	f.mu.Lock()
	f.secretTitle = true
	f.mu.Unlock()
	reply, err := f.adapter.Send(context.Background(), toolRequest("get_issue"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(reply.Result, []byte(externalSecret)) || !bytes.Contains(reply.Result, []byte(`"is_error":true`)) {
		t.Fatal("protected credential escaped a projected field")
	}
}

func TestConnectorConformanceOverThePublishedWire(t *testing.T) {
	f := setup(t)
	conformance.Run(t, conformance.Target{
		Fixture: conformance.Fixture{Provider: provider, Role: wire.RoleConnector, Request: request("list_tools", nil)},
		Start: func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
			ctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = adapterkit.Run(ctx, adapterkit.Config{URL: endpoint.URL, Provider: provider, Role: wire.RoleConnector, Handler: f.adapter, Token: func(context.Context) (adapterkit.Token, error) {
					return adapterkit.Token{Value: "fixture", Expiry: time.Now().Add(time.Hour)}, nil
				}, TLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: endpoint.RootCAs}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), BackoffBase: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond})
			}()
			return func() { cancel(); <-done }, nil
		},
	})
}
