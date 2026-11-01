//go:build codexqualification

package codexqualification

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// envdPort is the sandbox's own process service.
const envdPort = 49983

// connectEndStream marks the Connect frame that ends a stream.
const connectEndStream = 2

// containmentStack observes the sandbox directly, through E2B, so nothing here
// relies on what the adapters under test report.
func newContainmentStack(t *testing.T, ctx context.Context) *cancellationStack {
	t.Helper()
	project := os.Getenv("DAC_QUALIFICATION_PROJECT")
	if project == "" {
		t.Fatal("DAC_QUALIFICATION_PROJECT must name the deployed stack")
	}
	key := readSecretFile(t, "DAC_E2B_API_KEY_FILE")
	client := qualificationClient(t)
	return &cancellationStack{t: t, ctx: ctx, client: client, token: &tokenSource{t: t, ctx: ctx, client: client}, project: project, e2bKey: key, inSandbox: true}
}

func readSecretFile(t *testing.T, variable string) string {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv(variable))
	if err != nil {
		t.Fatalf("cannot read %s", variable)
	}
	return strings.TrimSpace(string(raw))
}

func TestContainmentQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	s := newContainmentStack(t, ctx)

	t.Run("commands run in the sandbox", func(t *testing.T) {
		task := s.startActiveTask(t)
		sandbox := s.sandbox(t, task)
		if !s.sandboxHasCommand(t, sandbox) {
			t.Fatalf("%q is not in the sandbox", s.command)
		}
		if s.adapterHasCommand(t) {
			t.Fatalf("%q runs in the adapter container", s.command)
		}
		t.Logf("sandbox reports: %s", s.sandboxRun(t, sandbox, "hostname; id -un; pgrep -af 'exec-server' | sed 's/[0-9a-f]\\{64\\}/HASH/'"))
		cancelTask(t, ctx, s.client, s.token, task.Task.ID)
		s.requireConverged(t, task)
	})

	t.Run("credentials stay out of the workload", func(t *testing.T) {
		task := s.startActiveTask(t)
		sandbox := s.sandbox(t, task)
		attachKey := readSecretFile(t, "DAC_ENVIRONMENT_ATTACH_KEY_FILE")
		token := attachToken(attachKey, "sandbox:"+sandbox)
		sum := sha256.Sum256([]byte(token))
		hash := hex.EncodeToString(sum[:])
		// The secrets are searched for here rather than sent in, because a search
		// run in the sandbox would have to carry them there. Root sees every
		// process, including the workload's own command.
		dump := s.sandboxDump(t, sandbox)
		if !bytes.Contains(dump, []byte(hash)) {
			t.Fatalf("the dump of %d bytes missed exec-server's command line, so it proves nothing", len(dump))
		}
		if !bytes.Contains(dump, []byte(s.command)) {
			t.Fatalf("the dump of %d bytes missed the workload's command line, so it proves nothing", len(dump))
		}
		for name, secret := range map[string]string{"model key": readSecretFile(t, "DAC_CODEX_API_KEY_FILE"), "attach key": attachKey, "attach token": token, "E2B key": s.e2bKey} {
			if bytes.Contains(dump, []byte(secret)) {
				t.Fatalf("the %s is readable in the sandbox", name)
			}
		}
		t.Logf("searched %d bytes of process and file content", len(dump))
		cancelTask(t, ctx, s.client, s.token, task.Task.ID)
		s.requireConverged(t, task)
	})

	t.Run("egress stays denied", func(t *testing.T) {
		task := s.startActiveTask(t)
		sandbox := s.sandbox(t, task)
		for _, target := range []string{"https://api.ai.camer.digital/v1/models", "https://1.1.1.1"} {
			output := s.sandboxRun(t, sandbox, fmt.Sprintf(`curl -sS -m 6 -o /dev/null -w 'status=%%{http_code}' %s 2>/dev/null; echo " exit=$?"`, target))
			if !strings.Contains(output, "status=000") || strings.Contains(output, "exit=0") {
				t.Fatalf("%s was reachable from the sandbox: %s", target, output)
			}
		}
		cancelTask(t, ctx, s.client, s.token, task.Task.ID)
		s.requireConverged(t, task)
	})

	t.Run("stopping the environment alone ends the work", func(t *testing.T) {
		started := time.Now()
		task := s.startActiveTask(t)
		sandbox := s.sandbox(t, task)
		s.deleteSandbox(t, sandbox)
		if s.sandboxExists(t, sandbox) {
			t.Fatal("the sandbox survived its deletion")
		}
		s.requireSettled(t, task)
		t.Logf("settled in %s without a cancellation or an interrupt\n%s", time.Since(started).Round(time.Millisecond), s.taskEvidence(t, task))
		if got := s.sql(t, fmt.Sprintf("select count(*) from outbox where attempt_id = '%s' and payload->>'method' = 'interrupt'", task.Attempt.ID)); got != "0" {
			t.Fatalf("the control plane sent %s interrupts", got)
		}
	})

	t.Run("runtime restart", func(t *testing.T) {
		task := s.startActiveTask(t)
		sandbox := s.sandbox(t, task)
		s.docker(t, "kill", "codex-adapter")
		s.start(t, "codex-adapter")
		s.requireSettled(t, task)
		if s.sandboxHasCommand(t, sandbox) {
			t.Fatalf("%q outlived the settled task", s.command)
		}
		t.Logf("settled after restart\n%s", s.taskEvidence(t, task))
	})

	t.Run("a result is read from the sandbox", func(t *testing.T) {
		submitted := submit(t, ctx, s.client, s.token,
			"Use exec_command exactly once with sandbox_permissions require_escalated to run: printf 'contained\\n' > result.txt. Do not use another file-writing tool.",
			"result.txt contains exactly contained followed by one newline")
		for approvals := 0; approvals < 4; approvals++ {
			attention, ready := waitAttentionOrReady(t, ctx, s.client, s.token, submitted.Task.ID)
			if ready {
				break
			}
			decide(t, ctx, s.client, s.token, attention, lifecycle.DecisionApproveOnce)
			waitDecisionConsumed(t, ctx, s.client, s.token, submitted.Task.ID, attention.ID)
		}
		ready := waitTask(t, ctx, s.client, s.token, submitted.Task.ID, lifecycle.TaskReadyForReview)
		if len(ready.value.Revisions) != 1 || len(ready.value.Revisions[0].Revision.Artifacts) != 1 {
			t.Fatalf("revisions=%+v", ready.value.Revisions)
		}
		if got := artifactEvidence(t, ctx, s.client, ready.value.Revisions[0].Revision.Artifacts[0]); string(got) != "contained\n" {
			t.Fatalf("artifact bytes=%q", got)
		}
		if _, err := os.Stat("result.txt"); err == nil {
			t.Fatal("the result appeared on the host")
		}
		review(t, ctx, s.client, s.token, ready, lifecycle.DispositionAccept)
		_ = waitTask(t, ctx, s.client, s.token, submitted.Task.ID, lifecycle.TaskSucceeded)
	})
}

// requireSettled proves a task that lost its environment ends rather than
// hanging, and that it gave its budget back.
func (s *cancellationStack) requireSettled(t *testing.T, task httpapi.SubmissionBody) {
	t.Helper()
	deadline, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	view := poll(t, deadline, "settled task", func() (httpapi.TaskBody, bool) {
		view := s.view(t, task)
		return view, view.Task.State == lifecycle.TaskFailed
	})
	if view.Attempt.State != lifecycle.AttemptFailed {
		t.Fatalf("attempt = %+v, want failed", view.Attempt)
	}
	if got := s.sql(t, fmt.Sprintf("select state from reservation where attempt_id = '%s'", task.Attempt.ID)); got != string(lifecycle.ReservationReleased) {
		t.Fatalf("reservation = %q, want released", got)
	}
}

func attachToken(key, ref string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("dac-exec-server-v1\x00" + ref))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *cancellationStack) e2bRequest(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	base := os.Getenv("DAC_E2B_BASE_URL")
	if base == "" {
		base = "https://api.e2b.app"
	}
	req, err := http.NewRequestWithContext(s.ctx, method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", s.e2bKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("E2B request failed")
	}
	return res
}

func (s *cancellationStack) deleteSandbox(t *testing.T, id string) {
	t.Helper()
	res := s.e2bRequest(t, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("E2B delete = %d", res.StatusCode)
	}
}

// sandboxRun runs a command as root through envd. Connecting restarts the
// sandbox's lifetime at the timeout given, which is long enough for any scenario.
func (s *cancellationStack) sandboxRun(t *testing.T, id, command string) string {
	t.Helper()
	output, ok := s.trySandboxRun(t, id, command)
	if !ok {
		t.Fatalf("sandbox %s is gone", id)
	}
	return output
}

func (s *cancellationStack) trySandboxRun(t *testing.T, id, command string) (string, bool) {
	t.Helper()
	res := s.e2bRequest(t, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/connect", strings.NewReader(`{"timeout":900}`))
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return "", false
	}
	if res.StatusCode >= 300 {
		t.Fatalf("E2B connect = %d", res.StatusCode)
	}
	var connected struct {
		Token string `json:"envdAccessToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&connected); err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(map[string]any{"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-c", command}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://%d-%s.e2b.app/process.Process/Start", envdPort, id), bytes.NewReader(append(frame, message...)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("root:")))
	req.Header.Set("X-Access-Token", connected.Token)
	process, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("envd request failed")
	}
	defer process.Body.Close()
	if process.StatusCode != http.StatusOK {
		t.Fatalf("envd = %d", process.StatusCode)
	}
	var output strings.Builder
	for {
		var header [5]byte
		if _, err := io.ReadFull(process.Body, header[:]); err != nil {
			t.Fatal("envd stream ended early")
		}
		body := make([]byte, binary.BigEndian.Uint32(header[1:]))
		if _, err := io.ReadFull(process.Body, body); err != nil {
			t.Fatal("envd stream ended early")
		}
		if header[0]&connectEndStream != 0 {
			return output.String(), true
		}
		var event struct {
			Event struct {
				Data struct {
					Stdout string `json:"stdout"`
				} `json:"data"`
			} `json:"event"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(event.Event.Data.Stdout)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(data)
	}
}

func (s *cancellationStack) sandboxHasCommand(t *testing.T, id string) bool {
	t.Helper()
	output, ok := s.trySandboxRun(t, id, fmt.Sprintf("pgrep -fc '^(/usr/bin/)?%s$' || true", s.command))
	return ok && strings.TrimSpace(output) != "0" && strings.TrimSpace(output) != ""
}

// sandboxDump reads every process's environment and command line, and every
// small file a workload could read.
func (s *cancellationStack) sandboxDump(t *testing.T, id string) []byte {
	t.Helper()
	encoded := s.sandboxRun(t, id, `{ for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do cat "$f" 2>/dev/null; echo; done; find /home /workspace /tmp /etc /run /root -type f -size -1024k -exec cat {} + 2>/dev/null; } | gzip -c | base64 -w0`)
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return gunzip(t, compressed)
}

func gunzip(t *testing.T, compressed []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
