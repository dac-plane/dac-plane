//go:build githubqualification

package githubadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

// TestLiveGitHubConnector uses an already consented dedicated qualification
// connection. It does not run during ordinary tests or use a personal gh token.
func TestLiveGitHubConnector(t *testing.T) {
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	key, err := os.ReadFile(required("DAC_GITHUB_VAULT_KEY_FILE"))
	if err != nil {
		t.Fatal("cannot read vault key")
	}
	vault, err := OpenVault(required("DAC_GITHUB_VAULT_FILE"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	secret, err := os.ReadFile(required("DAC_GITHUB_CLIENT_SECRET_FILE"))
	if err != nil {
		t.Fatal("cannot read app secret")
	}
	a, err := New(Config{Vault: vault, TenantID: required("DAC_TENANT_ID"), ProviderID: required("DAC_PROVIDER_ID"), ClientID: required("DAC_GITHUB_CLIENT_ID"), ClientSecret: strings.TrimSpace(string(secret)), CallbackURL: required("DAC_GITHUB_CALLBACK_URL")})
	if err != nil {
		t.Fatal(err)
	}
	connection := required("DAC_GITHUB_QUALIFICATION_CONNECTION")
	owner, repo := required("DAC_GITHUB_QUALIFICATION_OWNER"), required("DAC_GITHUB_QUALIFICATION_REPO")
	issue, err := strconv.ParseInt(required("DAC_GITHUB_QUALIFICATION_ISSUE"), 10, 64)
	if err != nil || issue <= 0 {
		t.Fatal("invalid qualification issue number")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools := []string{"get_issue", "list_repositories"}
	if os.Getenv("DAC_GITHUB_QUALIFICATION_WRITE") == "true" {
		tools = append(tools, "create_comment")
	}
	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			operation := liveID(t)
			input := map[string]any{}
			if tool != "list_repositories" {
				input = map[string]any{"owner": owner, "repo": repo, "number": issue}
			}
			if tool == "create_comment" {
				input["body"] = "dac-plane connector qualification " + operation
			}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			e := execution{Request: actionRequest{Tenant: a.cfg.TenantID, Attempt: liveID(t), Provider: a.cfg.ProviderID, Operation: operation, Action: "connector." + tool, InputDigest: digest(raw)}, Grant: liveID(t), ConnectionGrant: liveID(t), Connection: connection, Tool: tool, Source: "standing_grant", Input: raw}
			e.Request.Scope.Class = "subject"
			e.Request.Scope.Target = "connection:" + connection
			arguments, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			r := wire.Request{Operation: operation, Delivery: liveID(t), Tenant: a.cfg.TenantID, Attempt: e.Request.Attempt, Role: wire.RoleConnector, Method: "call_tool", Arguments: arguments, Demand: "person", Deadline: time.Now().UTC().Add(time.Minute)}
			first, err := a.Send(ctx, r)
			if err != nil || first.Durability != wire.DurabilityAccepted {
				t.Fatalf("live %s did not complete with known durability", tool)
			}
			var result callResult
			if json.Unmarshal(first.Result, &result) != nil || result.IsError || result.Digest != digest(result.Output) {
				t.Fatalf("live %s result failed", tool)
			}
			r.Delivery = liveID(t)
			second, err := a.Send(ctx, r)
			if err != nil || string(second.Result) != string(first.Result) {
				t.Fatal("stable-operation replay changed result")
			}
		})
	}
}
func liveID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		raw[i] = byte(ms)
		ms >>= 8
	}
	raw[6] = (raw[6] & 15) | 0x70
	raw[8] = (raw[8] & 63) | 0x80
	h := hex.EncodeToString(raw[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
