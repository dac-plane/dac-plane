//go:build e2bqualification

package e2badapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

func TestE2BEmbedLiveProvider(t *testing.T) {
	base, keyPath, template, provider := os.Getenv("DAC_E2B_BASE_URL"), os.Getenv("DAC_E2B_API_KEY_FILE"), os.Getenv("DAC_E2B_TEMPLATE_ID"), os.Getenv("DAC_PROVIDER_ID")
	if base == "" || keyPath == "" || template == "" || provider == "" {
		t.Fatal("set DAC_E2B_BASE_URL, DAC_E2B_API_KEY_FILE, DAC_E2B_TEMPLATE_ID and DAC_PROVIDER_ID")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal("cannot read E2B credential file")
	}
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{BaseURL: base, APIKey: strings.TrimSpace(string(key)), TemplateID: template, ProviderID: provider, Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	const tenant = "019a0d5b-f74c-7000-8000-000000000101"
	const source = "019a0d5b-f74c-7000-8000-000000000102"
	const migration = "019a0d5b-f74c-7000-8000-000000000103"
	operations := []string{"019a0d5b-f74c-7000-8000-000000000104", "019a0d5b-f74c-7000-8000-000000000105"}
	refs := make([]string, 0, 2)
	for i, operation := range operations {
		target := []string{"019a0d5b-f74c-7000-8000-000000000106", "019a0d5b-f74c-7000-8000-000000000107"}[i]
		args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operation, Source: source, Target: target, Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
		req := wire.Request{Operation: operation, Delivery: "019a0d5b-f74c-7000-8000-000000000108", Tenant: tenant, Attempt: target, Role: wire.RoleEnvironment, Method: "provision", Arguments: args, Demand: "background"}
		reply, sendErr := adapter.Send(ctx, req)
		if sendErr != nil {
			t.Fatalf("provision sandbox %d: %v", i+1, sendErr)
		}
		if reply.Outcome != wire.OutcomeOK {
			t.Fatalf("provision sandbox %d rejected: %+v", i+1, reply)
		}
		var receipt migrationReceipt
		if err := json.Unmarshal(reply.Result, &receipt); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, receipt.EnvRef)
		t.Cleanup(func() { _ = adapter.Cancel(context.Background(), wire.Cancel{Operation: operation}) })
	}
	if refs[0] == refs[1] {
		t.Fatalf("provider returned one resource for two operations: %q", refs[0])
	}
	cli, cliHome := os.Getenv("DAC_E2B_CLI"), os.Getenv("DAC_E2B_CLI_HOME")
	if cli == "" || cliHome == "" {
		t.Fatal("set DAC_E2B_CLI and DAC_E2B_CLI_HOME from the pinned qualification setup")
	}
	leftID, _ := sandboxID(refs[0])
	rightID, _ := sandboxID(refs[1])
	apiKey := strings.TrimSpace(string(key))
	marker := "dac-isolation-" + operations[0]
	file := "/tmp/" + marker
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "sh", "-c", fmt.Sprintf("printf '%s' isolated > '%s'", marker, file)); err != nil {
		t.Fatalf("write isolation probe: %v", err)
	}
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, rightID, "sh", "-c", "test ! -e '"+file+"'"); err != nil {
		t.Fatalf("filesystem isolation probe: %v", err)
	}
	pidFile := "/tmp/" + marker + ".pid"
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "sh", "-c", "sleep 300 >/dev/null 2>&1 </dev/null & echo $! > '"+pidFile+"'"); err != nil {
		t.Fatalf("start process isolation probe: %v", err)
	}
	pid, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "cat", pidFile)
	if err != nil || pid == "" || strings.Trim(pid, "0123456789") != "" {
		t.Fatalf("read process isolation PID: %q err=%v", pid, err)
	}
	pid = strings.TrimSpace(pid)
	processCheck := "if [ -r /proc/" + pid + "/cmdline ] && tr '\\000' ' ' </proc/" + pid + "/cmdline | grep -q '^sleep 300 '; then exit 1; fi"
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, rightID, "sh", "-c", processCheck); err != nil {
		t.Fatalf("process isolation probe: %v", err)
	}
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "python3", "-c", "print('egress probe ready')"); err != nil {
		t.Fatalf("egress probe prerequisite: %v", err)
	}
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "timeout", "8", "python3", "-c", "import urllib.request; urllib.request.urlopen('https://example.com', timeout=5)"); err == nil {
		t.Fatal("internet egress succeeded while allow_internet_access=false")
	}
	t.Log("filesystem isolation, process isolation, and deny-egress probes passed")
	for i, ref := range refs {
		observeID := []string{"019a0d5b-f74c-7000-8000-000000000109", "019a0d5b-f74c-7000-8000-00000000010a"}[i]
		args, _ := json.Marshal(observationRequest{EnvironmentRef: ref})
		reply, err := adapter.Send(ctx, wire.Request{Operation: observeID, Delivery: "019a0d5b-f74c-7000-8000-00000000010b", Tenant: tenant, Attempt: source, Role: wire.RoleEnvironment, Method: "observe", Arguments: args, Demand: "background"})
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("observe %q: reply=%+v err=%v", ref, reply, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	adapter, err = New(Config{BaseURL: base, APIKey: strings.TrimSpace(string(key)), TemplateID: template, ProviderID: provider, Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operations[0], Source: source, Target: "019a0d5b-f74c-7000-8000-000000000106", Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
	replay, err := adapter.Send(ctx, wire.Request{Operation: operations[0], Delivery: "019a0d5b-f74c-7000-8000-00000000010c", Tenant: tenant, Attempt: "019a0d5b-f74c-7000-8000-000000000106", Role: wire.RoleEnvironment, Method: "provision", Arguments: args, Demand: "background"})
	if err != nil || replay.Outcome != wire.OutcomeOK {
		t.Fatalf("reconcile after adapter restart: reply=%+v err=%v", replay, err)
	}
	var restored migrationReceipt
	if err := json.Unmarshal(replay.Result, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.EnvRef != refs[0] {
		t.Fatalf("restart changed resource mapping from %q to %q", refs[0], restored.EnvRef)
	}
	for i, ref := range refs {
		operation := "019a0d5b-f74c-7000-8000-00000000010d"
		stageTarget := ""
		if i == 1 {
			operation = "019a0d5b-f74c-7000-8000-00000000010e"
			stageTarget = "019a0d5b-f74c-7000-8000-000000000107"
		}
		stage := "source_cleaned"
		attempt := source
		if i == 1 {
			stage = "target_environment_cleaned"
			attempt = stageTarget
		}
		args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operation, Source: source, Target: stageTarget, Stage: stage, Epoch: 1, EnvRef: ref})
		reply, err := adapter.Send(ctx, wire.Request{Operation: operation, Delivery: "019a0d5b-f74c-7000-8000-00000000010f", Tenant: tenant, Attempt: attempt, Role: wire.RoleEnvironment, Method: "stop", Arguments: args, Demand: "background"})
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("stop %q: reply=%+v err=%v", ref, reply, err)
		}
		observationID := "019a0d5b-f74c-7000-8000-000000000110"
		if i == 1 {
			observationID = "019a0d5b-f74c-7000-8000-000000000111"
		}
		observeArgs, _ := json.Marshal(observationRequest{EnvironmentRef: ref})
		observed, err := adapter.Send(ctx, wire.Request{Operation: observationID, Delivery: "019a0d5b-f74c-7000-8000-000000000112", Tenant: tenant, Attempt: attempt, Role: wire.RoleEnvironment, Method: "observe", Arguments: observeArgs, Demand: "background"})
		if err != nil || observed.Outcome != wire.OutcomeOK {
			t.Fatalf("observe stopped resource %q: reply=%+v err=%v", ref, observed, err)
		}
		var state observation
		if err := json.Unmarshal(observed.Result, &state); err != nil || state.State != "stopped" {
			t.Fatalf("stopped resource %q state=%+v err=%v", ref, state, err)
		}
	}
}

func runSandboxCommand(ctx context.Context, cli, cliHome, baseURL, apiKey, sandbox string, args ...string) (string, error) {
	commandArgs := append([]string{"sandbox", "exec", sandbox}, args...)
	command := exec.CommandContext(ctx, cli, commandArgs...)
	command.Env = make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "HOME=") || strings.HasPrefix(value, "XDG_CONFIG_HOME=") || strings.HasPrefix(value, "E2B_API_URL=") || strings.HasPrefix(value, "E2B_API_KEY=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, "HOME="+cliHome, "XDG_CONFIG_HOME="+filepath.Join(cliHome, ".config"), "E2B_API_URL="+baseURL, "E2B_API_KEY="+apiKey)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("E2B CLI sandbox command failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}
