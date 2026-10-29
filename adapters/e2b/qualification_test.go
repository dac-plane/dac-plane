//go:build e2bqualification

package e2badapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

func TestE2BCloudLiveProvider(t *testing.T) {
	base, key, template, provider := os.Getenv("DAC_E2B_BASE_URL"), os.Getenv("DAC_E2B_API_KEY"), os.Getenv("DAC_E2B_TEMPLATE_ID"), os.Getenv("DAC_PROVIDER_ID")
	if base == "" || key == "" || template == "" || provider == "" {
		t.Fatal("set DAC_E2B_BASE_URL, DAC_E2B_API_KEY, DAC_E2B_TEMPLATE_ID and DAC_PROVIDER_ID")
	}
	if base != "https://api.e2b.app" {
		t.Fatal("live qualification requires the E2B Cloud API origin https://api.e2b.app")
	}
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adapter, err := New(Config{BaseURL: base, APIKey: key, TemplateID: template, ProviderID: provider, Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	tenant, source, migration := newQualificationID(), newQualificationID(), newQualificationID()
	operations := []string{newQualificationID(), newQualificationID()}
	targets := make([]string, len(operations))
	refs := make([]string, 0, 2)
	for i, operation := range operations {
		target := newQualificationID()
		targets[i] = target
		args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operation, Source: source, Target: target, Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
		req := wire.Request{Operation: operation, Delivery: newQualificationID(), Tenant: tenant, Attempt: target, Role: wire.RoleEnvironment, Method: "provision", Arguments: args, Demand: "background"}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cleanupCancel()
			if err := adapter.Cancel(cleanupCtx, wire.Cancel{Operation: operation}); err != nil {
				t.Errorf("cleanup sandbox %d failed: %v", i+1, err)
				return
			}
			current, err := adapter.cfg.Journal.Operation(operation)
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			if err != nil {
				t.Errorf("cleanup sandbox %d state check failed", i+1)
				return
			}
			id, ok := sandboxID(current.ResourceRef)
			if !ok {
				return
			}
			if err := waitSandboxAbsent(cleanupCtx, adapter, id); err != nil {
				t.Errorf("cleanup sandbox %d failed: %v", i+1, err)
			}
		})
		reply, sendErr := adapter.Send(ctx, req)
		if sendErr != nil {
			t.Fatalf("provision sandbox %d: %v", i+1, sendErr)
		}
		if reply.Outcome != wire.OutcomeOK {
			t.Fatalf("provision sandbox %d rejected with outcome %q", i+1, reply.Outcome)
		}
		var receipt migrationReceipt
		if err := json.Unmarshal(reply.Result, &receipt); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, receipt.EnvRef)
		id, ok := sandboxID(receipt.EnvRef)
		if !ok {
			t.Fatalf("provider returned invalid sandbox reference for sandbox %d", i+1)
		}
		var providerInfo sandbox
		if err := adapter.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, "", &providerInfo); err != nil {
			t.Fatalf("read provider metadata for sandbox %d: %v", i+1, err)
		}
		if providerInfo.EnvdVersion == "" {
			t.Fatalf("provider metadata omitted runtime version for sandbox %d", i+1)
		}
		t.Logf("provider service metadata: envd_version=%s template=%s", providerInfo.EnvdVersion, providerInfo.Template)
	}
	if refs[0] == refs[1] {
		t.Fatal("provider returned one resource for two operations")
	}
	cli, cliHome := os.Getenv("DAC_E2B_CLI"), os.Getenv("DAC_E2B_CLI_HOME")
	if cli == "" || cliHome == "" {
		t.Fatal("set DAC_E2B_CLI and DAC_E2B_CLI_HOME from the pinned qualification setup")
	}
	leftID, _ := sandboxID(refs[0])
	rightID, _ := sandboxID(refs[1])
	apiKey := key
	marker := "dac-isolation-" + operations[0]
	file := "/tmp/" + marker
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "sh", "-c", fmt.Sprintf("printf '%s' isolated > '%s'", marker, file)); err != nil {
		t.Fatalf("write isolation probe: %v", err)
	}
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, rightID, "sh", "-c", "test ! -e '"+file+"'"); err != nil {
		t.Fatalf("filesystem isolation probe: %v", err)
	}
	pidFile := "/tmp/" + marker + ".pid"
	processMarker := marker + "-process"
	startProcess := "nohup python3 -c \"import time; print('" + processMarker + "'); time.sleep(300)\" >/tmp/" + processMarker + ".log 2>&1 </dev/null & echo $! > '" + pidFile + "'"
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "sh", "-c", startProcess); err != nil {
		t.Fatalf("start process isolation probe: %v", err)
	}
	pid, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "cat", pidFile)
	if err != nil || pid == "" || strings.Trim(pid, "0123456789") != "" {
		t.Fatalf("read process isolation PID: %q err=%v", pid, err)
	}
	pid = strings.TrimSpace(pid)
	processCheck := "kill -0 " + pid + " && tr '\\000' ' ' </proc/" + pid + "/cmdline | grep -Fq '" + processMarker + "'"
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "sh", "-c", processCheck); err != nil {
		t.Fatalf("positive process isolation probe: %v", err)
	}
	processIsolationCheck := "if [ -r /proc/" + pid + "/cmdline ] && tr '\\000' ' ' </proc/" + pid + "/cmdline | grep -Fq '" + processMarker + "'; then exit 1; fi"
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, rightID, "sh", "-c", processIsolationCheck); err != nil {
		t.Fatalf("process isolation probe: %v", err)
	}
	if _, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "python3", "-c", "print('egress probe ready')"); err != nil {
		t.Fatalf("egress probe prerequisite: %v", err)
	}
	egressProbe := "import errno,socket,sys; s=None\ntry:\n s=socket.create_connection(('1.1.1.1',443),timeout=6)\nexcept (TimeoutError,OSError) as e:\n print('EGRESS_BLOCKED' if isinstance(e,TimeoutError) or e.errno in (errno.EACCES,errno.EPERM,errno.ENETUNREACH,errno.EHOSTUNREACH,errno.ETIMEDOUT,errno.ECONNRESET) else 'EGRESS_UNVERIFIED')\n sys.exit(0 if isinstance(e,TimeoutError) or e.errno in (errno.EACCES,errno.EPERM,errno.ENETUNREACH,errno.EHOSTUNREACH,errno.ETIMEDOUT,errno.ECONNRESET) else 2)\nelse:\n s.close(); print('EGRESS_ALLOWED'); sys.exit(3)"
	egressResult, err := runSandboxCommand(ctx, cli, cliHome, base, apiKey, leftID, "python3", "-c", egressProbe)
	if err != nil || egressResult != "EGRESS_BLOCKED" {
		t.Fatalf("deny-egress probe result=%q err=%v", egressResult, err)
	}
	t.Log("filesystem isolation, process isolation, and deny-egress probes passed")
	for i, ref := range refs {
		observeID := newQualificationID()
		args, _ := json.Marshal(observationRequest{EnvironmentRef: ref})
		reply, err := adapter.Send(ctx, wire.Request{Operation: observeID, Delivery: newQualificationID(), Tenant: tenant, Attempt: source, Role: wire.RoleEnvironment, Method: "observe", Arguments: args, Demand: "background"})
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("observe sandbox %d failed: outcome=%q err=%v", i+1, reply.Outcome, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = journal.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err = New(Config{BaseURL: base, APIKey: key, TemplateID: template, ProviderID: provider, Journal: store})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operations[0], Source: source, Target: targets[0], Stage: "target_environment_ready", Epoch: 1, Recovery: "restart_from_artifacts"})
	replay, err := adapter.Send(ctx, wire.Request{Operation: operations[0], Delivery: newQualificationID(), Tenant: tenant, Attempt: targets[0], Role: wire.RoleEnvironment, Method: "provision", Arguments: args, Demand: "background"})
	if err != nil || replay.Outcome != wire.OutcomeOK {
		t.Fatalf("reconcile after adapter restart failed: outcome=%q err=%v", replay.Outcome, err)
	}
	var restored migrationReceipt
	if err := json.Unmarshal(replay.Result, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.EnvRef != refs[0] {
		t.Fatal("restart changed the persisted sandbox mapping")
	}
	for i, ref := range refs {
		operation := newQualificationID()
		stageTarget := ""
		if i == 1 {
			stageTarget = targets[i]
		}
		stage := "source_cleaned"
		attempt := source
		if i == 1 {
			stage = "target_environment_cleaned"
			attempt = stageTarget
		}
		args, _ := json.Marshal(migrationRequest{Migration: migration, Operation: operation, Source: source, Target: stageTarget, Stage: stage, Epoch: 1, EnvRef: ref})
		reply, err := adapter.Send(ctx, wire.Request{Operation: operation, Delivery: newQualificationID(), Tenant: tenant, Attempt: attempt, Role: wire.RoleEnvironment, Method: "stop", Arguments: args, Demand: "background"})
		if err != nil || reply.Outcome != wire.OutcomeOK {
			t.Fatalf("stop sandbox %d failed: outcome=%q err=%v", i+1, reply.Outcome, err)
		}
		observationID := newQualificationID()
		observeArgs, _ := json.Marshal(observationRequest{EnvironmentRef: ref})
		observed, err := adapter.Send(ctx, wire.Request{Operation: observationID, Delivery: newQualificationID(), Tenant: tenant, Attempt: attempt, Role: wire.RoleEnvironment, Method: "observe", Arguments: observeArgs, Demand: "background"})
		if err != nil || observed.Outcome != wire.OutcomeOK {
			t.Fatalf("observe cleaned sandbox %d failed: outcome=%q err=%v", i+1, observed.Outcome, err)
		}
		var state observation
		if err := json.Unmarshal(observed.Result, &state); err != nil || state.State != "absent" {
			t.Fatalf("cleaned sandbox %d state=%q err=%v", i+1, state.State, err)
		}
	}
}

func newQualificationID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic("cannot generate qualification identifier")
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func waitSandboxAbsent(ctx context.Context, adapter *Adapter, id string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var current sandbox
		err := adapter.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, "", &current)
		if err != nil {
			var status apiError
			if errors.As(err, &status) && status.status == http.StatusNotFound {
				return nil
			}
			return errors.New("provider cleanup state could not be verified")
		}
		select {
		case <-ctx.Done():
			return errors.New("sandbox remained present after cleanup")
		case <-ticker.C:
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
