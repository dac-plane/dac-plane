//go:build codexqualification

package codexqualification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// commandGrace bounds how long a command may outlive its interrupted turn.
const commandGrace = 60 * time.Second

// cancellationStack is the deployed Codex and E2B stack one scenario drives.
type cancellationStack struct {
	t       *testing.T
	ctx     context.Context
	client  *http.Client
	token   *tokenSource
	project string
	e2bKey  string
	tasks   int
	// command is unique per task, so a process listing matches only that task.
	command string
}

// A cancellation scenario proves the stop rather than the request: each one
// observes the command before the cancellation and its absence afterwards, and
// asks E2B directly whether the sandbox still exists.
func TestTaskCancellationQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	project := os.Getenv("DAC_QUALIFICATION_PROJECT")
	if project == "" {
		t.Fatal("DAC_QUALIFICATION_PROJECT must name the deployed stack")
	}
	key, err := os.ReadFile(os.Getenv("DAC_E2B_API_KEY_FILE"))
	if err != nil {
		t.Fatal("cannot read the E2B credential")
	}
	client := qualificationClient(t)
	s := &cancellationStack{t: t, ctx: ctx, client: client, token: &tokenSource{t: t, ctx: ctx, client: client}, project: project, e2bKey: strings.TrimSpace(string(key))}

	t.Run("active command", func(t *testing.T) {
		started := time.Now()
		task := s.startActiveTask(t)
		cancelTask(t, ctx, client, s.token, task.Task.ID)
		s.requireConverged(t, task)
		s.report(t, "active command", task, started)

		// Nothing that restarts afterwards can reverse the converged result.
		before := s.snapshot(t, task)
		s.restart(t, "codex-adapter", "e2b-adapter", "dac-plane")
		waitSurface(t, ctx, client, s.token, task.Task.ID)
		time.Sleep(30 * time.Second)
		if after := s.snapshot(t, task); after != before {
			t.Fatalf("restart changed the converged result:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("environment adapter down", func(t *testing.T) {
		started := time.Now()
		task := s.startActiveTask(t)
		s.stop(t, "e2b-adapter")
		cancelTask(t, ctx, client, s.token, task.Task.ID)
		s.requireRuntimeStopped(t, task)
		s.requireEnvironmentPending(t, task)
		s.start(t, "e2b-adapter")
		s.requireConverged(t, task)
		s.report(t, "environment adapter down", task, started)
	})

	t.Run("control plane restart", func(t *testing.T) {
		started := time.Now()
		task := s.startActiveTask(t)
		s.stop(t, "e2b-adapter")
		cancelTask(t, ctx, client, s.token, task.Task.ID)
		s.requireRuntimeStopped(t, task)
		s.restart(t, "dac-plane")
		waitSurface(t, ctx, client, s.token, task.Task.ID)
		s.requireEnvironmentPending(t, task)
		s.start(t, "e2b-adapter")
		s.requireConverged(t, task)
		s.report(t, "control plane restart", task, started)
	})

	t.Run("runtime adapter restart", func(t *testing.T) {
		started := time.Now()
		task := s.startActiveTask(t)
		s.stop(t, "codex-adapter")
		cancelTask(t, ctx, client, s.token, task.Task.ID)
		if view := s.view(t, task); view.Attempt.RuntimeStopped {
			t.Fatalf("runtime stop observed while its adapter was down: %+v", view.Attempt)
		}
		s.start(t, "codex-adapter")
		s.requireConverged(t, task)
		s.report(t, "runtime adapter restart", task, started)
	})
}

// startActiveTask approves actions until the command is observably running.
func (s *cancellationStack) startActiveTask(t *testing.T) httpapi.SubmissionBody {
	t.Helper()
	s.tasks++
	s.command = fmt.Sprintf("sleep %d", 3170+s.tasks)
	task := submit(t, s.ctx, s.client, s.token,
		"Use exec_command with sandbox_permissions require_escalated to run: "+s.command+". Wait for the command to finish.",
		"the command finishes unless DAC cancels the task")
	approved := map[ids.AttentionID]bool{}
	poll(t, s.ctx, "active command", func() (struct{}, bool) {
		if s.commandRunning(t) {
			return struct{}{}, true
		}
		view := s.view(t, task)
		for _, attention := range view.OpenAttention {
			if attention.State == lifecycle.AttentionPending && !approved[attention.ID] {
				approved[attention.ID] = true
				decide(t, s.ctx, s.client, s.token, attention, lifecycle.DecisionApproveOnce)
			}
		}
		return struct{}{}, false
	})
	if !s.sandboxExists(t, s.sandbox(t, task)) {
		t.Fatal("the assigned sandbox is not running beside the active command")
	}
	return task
}

func (s *cancellationStack) requireRuntimeStopped(t *testing.T, task httpapi.SubmissionBody) {
	t.Helper()
	poll(t, s.ctx, "runtime stop", func() (struct{}, bool) {
		return struct{}{}, s.view(t, task).Attempt.RuntimeStopped
	})
}

// requireEnvironmentPending proves the task does not end on one stop alone.
func (s *cancellationStack) requireEnvironmentPending(t *testing.T, task httpapi.SubmissionBody) {
	t.Helper()
	view := s.view(t, task)
	if view.Task.State != lifecycle.TaskCancelling || view.Attempt.State != lifecycle.AttemptCancellationRequested || view.Attempt.EnvironmentStopped {
		t.Fatalf("task %s attempt %+v, want cancelling with the environment stop pending", view.Task.State, view.Attempt)
	}
	if !s.sandboxExists(t, s.sandbox(t, task)) {
		t.Fatal("the sandbox ended before its stop was delivered")
	}
}

func (s *cancellationStack) requireConverged(t *testing.T, task httpapi.SubmissionBody) {
	t.Helper()
	deadline, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	view := poll(t, deadline, "cancelled task", func() (httpapi.TaskBody, bool) {
		view := s.view(t, task)
		return view, view.Task.State == lifecycle.TaskCancelled
	})
	if view.Attempt.State != lifecycle.AttemptTerminated || !view.Attempt.RuntimeStopped || !view.Attempt.EnvironmentStopped {
		t.Fatalf("attempt = %+v, want terminated with both stops", view.Attempt)
	}
	if got := s.sql(t, fmt.Sprintf("select state from reservation where attempt_id = '%s'", task.Attempt.ID)); got != string(lifecycle.ReservationReleased) {
		t.Fatalf("reservation = %q, want released", got)
	}
	s.requireCommandGone(t)
	if s.sandboxExists(t, s.sandbox(t, task)) {
		t.Fatal("the sandbox survived its observed stop")
	}
	for _, stop := range []lifecycle.AttemptEvent{lifecycle.AttemptRuntimeStopped, lifecycle.AttemptEnvironmentStopped} {
		if got := s.sql(t, fmt.Sprintf("select count(*) from audit_event where record_id = '%s' and event = '%s' and outcome = 'applied'", task.Attempt.ID, stop)); got != "1" {
			t.Fatalf("applied %s = %s, want exactly one", stop, got)
		}
	}
}

// requireCommandGone proves the work itself ended, not only its lifecycle.
func (s *cancellationStack) requireCommandGone(t *testing.T) {
	t.Helper()
	started := time.Now()
	for time.Since(started) < commandGrace {
		if !s.commandRunning(t) {
			t.Logf("%q gone %s after convergence", s.command, time.Since(started).Round(time.Millisecond))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	output, _ := exec.CommandContext(s.ctx, "docker", "top", s.container("codex-adapter"), "-o", "pid,ppid,stat,etime,args").CombinedOutput()
	t.Fatalf("%q survived the cancellation by %s:\n%s", s.command, commandGrace, output)
}

func (s *cancellationStack) view(t *testing.T, task httpapi.SubmissionBody) httpapi.TaskBody {
	t.Helper()
	return request[httpapi.TaskBody](t, s.ctx, s.client, s.token, http.MethodGet, "/tasks/"+task.Task.ID.String(), nil, nil, http.StatusOK).value
}

// snapshot is everything a late observation could change.
func (s *cancellationStack) snapshot(t *testing.T, task httpapi.SubmissionBody) string {
	t.Helper()
	return s.sql(t, fmt.Sprintf(`select a.state, a.version, a.runtime_stopped, a.environment_stopped, t.state, t.version, r.state, r.version,
		(select count(*) from audit_event e where e.record_id = a.attempt_id and e.outcome = 'applied')
		from attempt a join task t on t.task_id = a.task_id join reservation r on r.attempt_id = a.attempt_id where a.attempt_id = '%s'`, task.Attempt.ID))
}

// report keeps the ordered stop evidence in the test log for the evidence note.
func (s *cancellationStack) report(t *testing.T, scenario string, task httpapi.SubmissionBody, started time.Time) {
	t.Helper()
	evidence := s.sql(t, fmt.Sprintf("select occurred_at, event, from_state, to_state, outcome, reason from audit_event where record_id = '%s' order by occurred_at, version", task.Attempt.ID))
	t.Logf("%s converged in %s\n%s", scenario, time.Since(started).Round(time.Millisecond), evidence)
}

func (s *cancellationStack) sandbox(t *testing.T, task httpapi.SubmissionBody) string {
	t.Helper()
	ref := s.sql(t, fmt.Sprintf("select native_ref from binding where attempt_id = '%s' and kind = 'environment_resource'", task.Attempt.ID))
	id, ok := strings.CutPrefix(ref, "sandbox:")
	if !ok || id == "" {
		t.Fatalf("environment resource %q was not provisioned by E2B", ref)
	}
	return id
}

// sandboxExists asks E2B directly, independently of the adapter under test.
func (s *cancellationStack) sandboxExists(t *testing.T, id string) bool {
	t.Helper()
	base := os.Getenv("DAC_E2B_BASE_URL")
	if base == "" {
		base = "https://api.e2b.app"
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, base+"/sandboxes/"+url.PathEscape(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", s.e2bKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		State string `json:"state"`
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	switch res.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		return body.State == "running"
	case http.StatusNotFound:
		return false
	default:
		t.Fatalf("E2B sandbox lookup = %d", res.StatusCode)
		return false
	}
}

func (s *cancellationStack) commandRunning(t *testing.T) bool {
	t.Helper()
	output, err := exec.CommandContext(s.ctx, "docker", "top", s.container("codex-adapter")).CombinedOutput()
	if err != nil {
		// A stopped container has no process list, and so no running command.
		return false
	}
	return strings.Contains(string(output), s.command)
}

func (s *cancellationStack) sql(t *testing.T, query string) string {
	t.Helper()
	output, err := exec.CommandContext(s.ctx, "docker", "exec", s.container("postgres"), "psql", "-v", "ON_ERROR_STOP=1", "-U", "dac_migrate", "-d", "dac", "-tAc", query).CombinedOutput()
	if err != nil {
		t.Fatalf("sql: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func (s *cancellationStack) stop(t *testing.T, services ...string) { s.docker(t, "stop", services...) }

func (s *cancellationStack) start(t *testing.T, services ...string) {
	s.docker(t, "start", services...)
}

func (s *cancellationStack) restart(t *testing.T, services ...string) {
	s.docker(t, "restart", services...)
}

func (s *cancellationStack) docker(t *testing.T, command string, services ...string) {
	t.Helper()
	args := []string{command}
	for _, service := range services {
		args = append(args, s.container(service))
	}
	if output, err := exec.CommandContext(s.ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v: %s", command, err, output)
	}
}

func (s *cancellationStack) container(service string) string {
	return s.project + "-" + service + "-1"
}
