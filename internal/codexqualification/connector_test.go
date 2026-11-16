//go:build codexqualification

package codexqualification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// connectorProviderID is the development stack's simulated connector
// (deploy/compose/compose.connector.yaml).
const connectorProviderID = "01a167fd-51d9-7d55-8a3b-6c5d4e3f2a13"

// TestCodexConnectorQualification has a live model call a connector tool
// through the gateway (ADR-0022): Codex reaches dac-plane's MCP endpoint with
// its attempt token, the call is authorized by a connection grant, the
// simulated connector answers, and the model writes what the tool returned.
// The simulator's answer carries a marker derived from the connection's
// simulated credential and absent from the prompt, so the artifact proves
// the call ran and its output reached the model.
func TestCodexConnectorQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client := qualificationClient(t)
	token := &tokenSource{t: t, ctx: ctx, client: client}
	connection, marker := activeConnection(t, ctx, client, token)

	for _, tc := range []struct {
		name       string
		permission identity.Permission
		issue      int
	}{
		{"always grant", identity.PermissionAlways, 137},
		{"ask grant waits for approval", identity.PermissionAsk, 97},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := createAgent(t, ctx, client, token, "Issue reader "+string(tc.permission), "Reads issues for the team",
				"Read issues only through the get_issue tool of the dac_plane MCP server.")
			grant := request[lifecycle.ConnectionGrant](t, ctx, client, token, http.MethodPost, "/connections/"+connection.ID.String()+"/grants", map[string]string{
				"Idempotency-Key": "codex-grant-" + strconv.FormatInt(time.Now().UnixNano(), 10),
			}, httpapi.ConnectionGrantRequest{Agent: agent.ID, Tools: []string{"get_issue"}, Permission: tc.permission, CallLimit: 3, ExpiresAt: time.Now().UTC().Add(time.Hour)}, http.StatusCreated).value
			issue := strconv.Itoa(tc.issue)
			submitted := submitTo(t, ctx, client, token, agent.ID,
				"Call the get_issue tool of the dac_plane MCP server with number "+issue+". Then use exec_command exactly once with sandbox_permissions require_escalated to write the issue's title, exactly as the tool returned it, followed by one newline, to issue.txt with printf. Do not use another file-writing tool.",
				"issue.txt contains the title get_issue returned for issue "+issue)
			connectorApprovals := 0
			for approvals := 0; approvals < 6; approvals++ {
				attention, done := waitAttentionOrReady(t, ctx, client, token, submitted.Task.ID)
				if done {
					break
				}
				if attention.Provider.String() == connectorProviderID {
					connectorApprovals++
				}
				decide(t, ctx, client, token, attention, lifecycle.DecisionApproveOnce)
				waitDecisionConsumed(t, ctx, client, token, submitted.Task.ID, attention.ID)
			}
			if tc.permission == identity.PermissionAsk && connectorApprovals == 0 {
				t.Fatal("an ask grant's call was not held for approval")
			}
			if tc.permission == identity.PermissionAlways && connectorApprovals != 0 {
				t.Fatalf("an always grant's call asked for approval %d times", connectorApprovals)
			}
			ready := waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskReadyForReview)
			if len(ready.value.Revisions) != 1 || len(ready.value.Revisions[0].Revision.Artifacts) != 1 {
				t.Fatalf("revisions=%+v", ready.value.Revisions)
			}
			// The marker never appears in the prompt: only the connector's
			// answer carries it, so the artifact proves the call ran.
			want := "Simulated issue " + issue + " " + marker + "\n"
			if got := artifactEvidence(t, ctx, client, ready.value.Revisions[0].Revision.Artifacts[0]); string(got) != want {
				t.Fatalf("artifact bytes=%q, want %q, which only the connector's answer names", got, want)
			}
			review(t, ctx, client, token, ready, lifecycle.DispositionAccept)
			_ = waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskSucceeded)
			revoked := request[lifecycle.ConnectionGrant](t, ctx, client, token, http.MethodPost, "/connection-grants/"+grant.ID.String()+"/revoke", map[string]string{
				"Idempotency-Key": "codex-revoke-" + strconv.FormatInt(time.Now().UnixNano(), 10), "If-Match": `"` + strconv.FormatUint(grant.Version, 10) + `"`,
			}, httpapi.EmptyCommandRequest{}, http.StatusOK).value
			if revoked.State != lifecycle.ConnectionGrantRevoked {
				t.Fatalf("revoked grant=%+v", revoked)
			}
		})
	}
}

// activeConnection registers a connection to the simulated connector and
// completes its consent through the simulator's loopback control, standing
// in for the owner's browser.
func activeConnection(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource) (lifecycle.Connection, string) {
	t.Helper()
	provider, err := ids.Parse[ids.Provider](connectorProviderID)
	if err != nil {
		t.Fatal(err)
	}
	created := request[httpapi.ConnectionCreatedBody](t, ctx, client, token, http.MethodPost, "/connections", map[string]string{
		"Idempotency-Key": "codex-connection-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	}, httpapi.ConnectionRequest{Provider: provider, Name: "Qualification connector " + strconv.FormatInt(time.Now().UnixNano(), 10)}, http.StatusCreated).value
	if created.Authorization == nil {
		t.Fatalf("connection created without a consent URL: %+v", created)
	}
	port := os.Getenv("DAC_CONNECTOR_CONTROL_PORT")
	if port == "" {
		port = "18090"
	}
	consent, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/v1/connections/"+created.Connection.ID.String()+"/consent", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(consent)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var proof struct {
		Confirmation string `json:"confirmation"`
		Marker       string `json:"marker"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(data, &proof) != nil || proof.Confirmation == "" || proof.Marker == "" {
		t.Fatalf("simulated consent = %d", res.StatusCode)
	}
	active := request[lifecycle.Connection](t, ctx, client, token, http.MethodPost, "/connections/"+created.Connection.ID.String()+"/complete", map[string]string{
		"Idempotency-Key": "codex-complete-" + strconv.FormatInt(time.Now().UnixNano(), 10), "If-Match": `"` + strconv.FormatUint(created.Connection.Version, 10) + `"`,
	}, httpapi.CompleteConnectionRequest{Confirmation: proof.Confirmation}, http.StatusOK).value
	if active.State != lifecycle.ConnectionActive {
		t.Fatalf("completed connection=%+v", active)
	}
	return active, proof.Marker
}
