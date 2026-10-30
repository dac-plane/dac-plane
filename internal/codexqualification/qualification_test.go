//go:build codexqualification

package codexqualification

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const (
	tenantID   = "01a167fd-51d9-76ba-b563-496e2bd89646"
	providerID = "01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10"
	// environmentProviderID is the development stack's simulated environment.
	environmentProviderID = "01a167fd-51d9-7d44-8a3b-6c5d4e3f2a12"
	apiRoot               = "https://dac-plane:8443/v1/tenants/" + tenantID
	tokenURL              = "https://issuer:8443/default/token"
)

type tokenSource struct {
	t       *testing.T
	ctx     context.Context
	client  *http.Client
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (s *tokenSource) current() string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(30*time.Second).Before(s.expires) {
		return s.token
	}
	token, lifetime := clientCredentialsToken(s.t, s.ctx, s.client)
	s.token = token
	s.expires = time.Now().Add(lifetime)
	return s.token
}

func TestCodexRuntimeQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client := qualificationClient(t)
	token := &tokenSource{t: t, ctx: ctx, client: client}

	t.Run("approval restart and exact artifact", func(t *testing.T) {
		submitted := submit(t, ctx, client, token,
			"Use exec_command exactly once with sandbox_permissions require_escalated to run: printf 'qualified\\n' > result.txt. Do not use another file-writing tool.",
			"result.txt contains exactly qualified followed by one newline")
		attention := waitAttention(t, ctx, client, token, submitted.Task.ID)
		if attention.Kind != lifecycle.AttentionActionApproval || attention.ActionDigest == "" {
			t.Fatalf("attention=%+v", attention)
		}
		seenDigests := map[string]bool{}
		for approvals := 0; approvals < 4; approvals++ {
			if approvals > 0 {
				var ready bool
				attention, ready = waitAttentionOrReady(t, ctx, client, token, submitted.Task.ID)
				if ready {
					break
				}
			}
			digest := string(attention.ActionDigest)
			if seenDigests[digest] {
				t.Fatalf("runtime repeated an already approved action digest %s", attention.ActionDigest)
			}
			seenDigests[digest] = true
			decide(t, ctx, client, token, attention, lifecycle.DecisionApproveOnce)
			waitDecisionConsumed(t, ctx, client, token, submitted.Task.ID, attention.ID)
		}
		ready := waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskReadyForReview)
		if len(ready.value.Revisions) != 1 || len(ready.value.Revisions[0].Revision.Artifacts) != 1 {
			t.Fatalf("revisions=%+v", ready.value.Revisions)
		}
		artifact := ready.value.Revisions[0].Revision.Artifacts[0]
		if got := artifactEvidence(t, ctx, client, artifact); string(got) != "qualified\n" {
			t.Fatalf("artifact bytes=%q", got)
		}
		// Restart after the runtime has reported its result. Restarting the
		// supervised app-server during an unresolved native approval interrupts
		// that approval, which Codex 0.165.0 does not reissue on thread/resume.
		restart(t, ctx, "dac-codex-qualification-codex-adapter-1", "dac-codex-qualification-dac-plane-1")
		waitSurface(t, ctx, client, token, submitted.Task.ID)
		ready = waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskReadyForReview)
		review(t, ctx, client, token, ready, lifecycle.DispositionAccept)
		_ = waitTask(t, ctx, client, token, submitted.Task.ID, lifecycle.TaskSucceeded)
	})

	t.Run("denial has no effect or standing authority", func(t *testing.T) {
		for attempt := 0; attempt < 2; attempt++ {
			name := fmt.Sprintf("denied-%d.txt", attempt+1)
			submitted := submit(t, ctx, client, token,
				"Use exec_command with sandbox_permissions require_escalated to run: printf 'must-not-exist\\n' > "+name+". Stop if the command is denied.",
				name+" exists only when the exact action is approved")
			attention := waitAttention(t, ctx, client, token, submitted.Task.ID)
			decide(t, ctx, client, token, attention, lifecycle.DecisionDeny)
			waitDecisionConsumed(t, ctx, client, token, submitted.Task.ID, attention.ID)
			cancelTask(t, ctx, client, token, submitted.Task.ID)
			waitTerminal(t, ctx, client, token, submitted.Task.ID)
			workspaceAbsent(t, ctx, name)
		}
	})

}

type responseValue[T any] struct {
	value  T
	header http.Header
}

func submit(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, objective, criterion string) httpapi.SubmissionBody {
	t.Helper()
	provider, err := ids.Parse[ids.Provider](providerID)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := ids.Parse[ids.Provider](environmentProviderID)
	if err != nil {
		t.Fatal(err)
	}
	return request[httpapi.SubmissionBody](t, ctx, client, token, http.MethodPost, "/tasks", map[string]string{
		"Idempotency-Key": "codex-submit-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	}, httpapi.SubmitTaskRequest{
		Objective: objective, AcceptanceCriteria: []string{criterion}, Provider: provider, EnvironmentProvider: environment,
		Demand: contracts.DemandPerson, Deadline: time.Now().UTC().Add(10 * time.Minute),
		Unit: "tokens", Ceiling: 100_000, Amount: 10_000,
	}, http.StatusCreated).value
}

func waitAttention(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID) lifecycle.AttentionRequest {
	t.Helper()
	return poll(t, ctx, "action approval", func() (lifecycle.AttentionRequest, bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK).value
		if view.Task.State == lifecycle.TaskFailed || view.Task.State == lifecycle.TaskCancelled || view.Task.State == lifecycle.TaskSucceeded {
			t.Fatalf("task reached terminal state %s before requesting approval", view.Task.State)
		}
		list := request[httpapi.AttentionListBody](t, ctx, client, token, http.MethodGet, "/attention", nil, nil, http.StatusOK).value
		for _, attention := range list.Attention {
			if attention.Task == task && attention.State == lifecycle.AttentionPending {
				return attention, true
			}
		}
		return lifecycle.AttentionRequest{}, false
	})
}

func waitAttentionOrReady(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID) (lifecycle.AttentionRequest, bool) {
	t.Helper()
	type result struct {
		attention lifecycle.AttentionRequest
		ready     bool
	}
	got := poll(t, ctx, "another action approval or task result", func() (result, bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK).value
		switch view.Task.State {
		case lifecycle.TaskReadyForReview:
			return result{ready: true}, true
		case lifecycle.TaskFailed, lifecycle.TaskCancelled, lifecycle.TaskSucceeded:
			t.Fatalf("task reached terminal state %s before result review", view.Task.State)
		}
		list := request[httpapi.AttentionListBody](t, ctx, client, token, http.MethodGet, "/attention", nil, nil, http.StatusOK).value
		for _, attention := range list.Attention {
			if attention.Task == task && attention.State == lifecycle.AttentionPending {
				return result{attention: attention}, true
			}
		}
		return result{}, false
	})
	return got.attention, got.ready
}

func waitDecisionConsumed(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID, target ids.AttentionID) {
	t.Helper()
	poll(t, ctx, "decision consumption", func() (struct{}, bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK).value
		if view.Task.State == lifecycle.TaskFailed || view.Task.State == lifecycle.TaskCancelled || view.Task.State == lifecycle.TaskSucceeded {
			t.Fatalf("task reached terminal state %s before decision consumption", view.Task.State)
		}
		page := request[httpapi.EventPageBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String()+"/events", nil, nil, http.StatusOK).value
		for _, event := range page.Events {
			if event.Record == lifecycle.RecordAttention && event.RecordID.String() == target.String() && event.Event == string(lifecycle.AttentionConsume) {
				return struct{}{}, true
			}
		}
		return struct{}{}, false
	})
}

func decide(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, attention lifecycle.AttentionRequest, kind lifecycle.DecisionKind) {
	t.Helper()
	request[httpapi.DecisionBody](t, ctx, client, token, http.MethodPost, "/attention/"+attention.ID.String()+"/decisions", map[string]string{
		"Idempotency-Key": "codex-decision-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"If-Match":        strconv.Quote(strconv.FormatUint(attention.Version, 10)),
	}, httpapi.DecisionRequest{Kind: kind}, http.StatusCreated)
}

func cancelTask(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID) {
	t.Helper()
	view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK)
	if view.value.Task.State == lifecycle.TaskCancelled || view.value.Task.State == lifecycle.TaskSucceeded || view.value.Task.State == lifecycle.TaskFailed {
		return
	}
	request[httpapi.CancelBody](t, ctx, client, token, http.MethodPost, "/tasks/"+task.String()+"/cancel", map[string]string{
		"Idempotency-Key": "codex-cancel-" + strconv.FormatInt(time.Now().UnixNano(), 10), "If-Match": view.header.Get("ETag"),
	}, httpapi.CancelRequest{}, http.StatusAccepted)
}

func waitTerminal(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID) {
	t.Helper()
	poll(t, ctx, "terminal task", func() (struct{}, bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK).value
		switch view.Task.State {
		case lifecycle.TaskCancelled, lifecycle.TaskFailed, lifecycle.TaskSucceeded:
			return struct{}{}, true
		default:
			return struct{}{}, false
		}
	})
}

func waitTask(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID, state lifecycle.TaskState) responseValue[httpapi.TaskBody] {
	t.Helper()
	return poll(t, ctx, string(state)+" task", func() (responseValue[httpapi.TaskBody], bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK)
		return view, view.value.Task.State == state
	})
}

func review(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, ready responseValue[httpapi.TaskBody], disposition lifecycle.Disposition) {
	t.Helper()
	revision := ready.value.Revisions[0].Revision
	request[httpapi.ReviewBody](t, ctx, client, token, http.MethodPost, "/tasks/"+ready.value.Task.ID.String()+"/reviews", map[string]string{
		"Idempotency-Key": "codex-review-" + strconv.FormatInt(time.Now().UnixNano(), 10), "If-Match": ready.header.Get("ETag"),
	}, httpapi.ReviewRequest{Revision: revision.ID, Disposition: disposition}, http.StatusCreated)
}

func artifactEvidence(t *testing.T, ctx context.Context, client *http.Client, artifact ids.ArtifactID) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://artifact-adapter:8082/v1/evidence/"+artifact.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("artifact evidence status=%d body=%s err=%v", res.StatusCode, data, err)
	}
	return data
}

func workspaceAbsent(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	if strings.ContainsAny(name, "/\\") {
		t.Fatal("unsafe fixture path")
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "dac-codex-qualification-codex-adapter-1", "test", "!", "-e", "/workspace/"+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workspace effect exists or could not be inspected: %v: %s", err, output)
	}
}

func restart(t *testing.T, ctx context.Context, containers ...string) {
	t.Helper()
	args := append([]string{"restart"}, containers...)
	if output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("restart: %v: %s", err, output)
	}
}

func waitSurface(t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, task ids.TaskID) {
	t.Helper()
	poll(t, ctx, "surface restart", func() (struct{}, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiRoot+"/tasks/"+task.String(), nil)
		if err != nil {
			return struct{}{}, false
		}
		req.Header.Set("Authorization", "Bearer "+token.current())
		res, err := client.Do(req)
		if err != nil {
			return struct{}{}, false
		}
		_ = res.Body.Close()
		return struct{}{}, res.StatusCode == http.StatusOK
	})
}

func request[T any](t *testing.T, ctx context.Context, client *http.Client, token *tokenSource, method, path string, headers map[string]string, body any, want int) responseValue[T] {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiRoot+path, input)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token.current())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, want, data)
	}
	var value T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
	}
	return responseValue[T]{value: value, header: res.Header}
}

func poll[T any](t *testing.T, ctx context.Context, description string, load func() (T, bool)) T {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if value, done := load(); done {
			return value
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func clientCredentialsToken(t *testing.T, ctx context.Context, client *http.Client) (string, time.Duration) {
	t.Helper()
	secret, err := os.ReadFile(os.Getenv("DAC_STACK_SURFACE_CLIENT_SECRET_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"dac-plane-surface"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("surface-owner", strings.TrimSpace(string(secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&payload) != nil || payload.AccessToken == "" || payload.ExpiresIn < 1 {
		t.Fatalf("token endpoint status=%d", res.StatusCode)
	}
	return payload.AccessToken, time.Duration(payload.ExpiresIn) * time.Second
}

func qualificationClient(t *testing.T) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(os.Getenv("DAC_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("qualification CA contains no certificate")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		mapped := map[string]string{"issuer:8443": "127.0.0.1:18080", "dac-plane:8443": "127.0.0.1:18443", "artifact-adapter:8082": "127.0.0.1:18082"}[address]
		if mapped == "" {
			return nil, fmt.Errorf("qualification refuses unexpected address %q", address)
		}
		return dialer.DialContext(ctx, network, mapped)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}
