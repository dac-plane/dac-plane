//go:build operationstest

package operationstest

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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const (
	tenantID       = "01a167fd-51d9-76ba-b563-496e2bd89646"
	providerID     = "01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10"
	currentImage   = "dac-plane:operations-current"
	nextImage      = "dac-plane:operations-next"
	oldImage       = "dac-plane:operations-old"
	simulatorImage = "dac-sim-adapter:operations"
	controlRoot    = "http://127.0.0.1:19090"
)

type harness struct {
	t        *testing.T
	project  string
	replicaA string
	replicaB string
	client   *http.Client
	token    string
}

func TestMultiReplicaFencing(t *testing.T) {
	h := newHarness(t, currentImage, currentImage)
	h.start()
	h.startSimulator()
	submitted := h.submit()
	h.registerPause(submitted.Operation.ID, "after_effect")
	h.waitPause(submitted.Operation.ID, "after_effect")

	first := h.waitClaim(submitted.Operation.ID, 1)
	claimant := h.replicaForOwner(first.owner)
	h.compose("pause", claimant)
	h.expireClaim(submitted.Operation.ID, submitted.Attempt.ID)
	winning := h.waitClaim(submitted.Operation.ID, first.epoch+1)
	t.Logf("claim takeover: %s epoch %d -> %s epoch %d", first.owner, first.epoch, winning.owner, winning.epoch)
	if winning.owner == first.owner {
		t.Fatalf("claim owner stayed %q after forced takeover", first.owner)
	}
	h.releasePause(submitted.Operation.ID, "after_effect")
	t.Logf("released after-effect pause; operation=%s outbox=%s", h.operationState(submitted.Operation.ID), h.outboxState(submitted.Operation.ID))
	h.waitOperationState(submitted.Operation.ID, lifecycle.OperationAccepted)
	h.compose("unpause", claimant)
	time.Sleep(500 * time.Millisecond)

	h.assertLedger(submitted.Operation.ID, 1)
	if evidence := h.ledgerEvidence(submitted.Operation.ID); evidence.Deliveries < 2 {
		t.Fatalf("delivery evidence = %+v, want the winning replay after takeover", evidence)
	}
	if state := h.operationState(submitted.Operation.ID); state != string(lifecycle.OperationAccepted) {
		t.Fatalf("operation state = %s, want accepted after winning replay and fenced stale settlement", state)
	}
}

func TestProcessKillAndRestart(t *testing.T) {
	h := newHarness(t, currentImage, currentImage)
	h.start()
	h.startSimulator()
	submitted := h.submit()
	h.registerPause(submitted.Operation.ID, "before_effect")
	h.waitPause(submitted.Operation.ID, "before_effect")

	first := h.waitClaim(submitted.Operation.ID, 1)
	claimant := h.replicaForOwner(first.owner)
	h.compose("stop", "-t", "0", claimant)
	h.expireClaim(submitted.Operation.ID, submitted.Attempt.ID)
	winning := h.waitClaim(submitted.Operation.ID, first.epoch+1)
	t.Logf("restart takeover: %s epoch %d -> %s epoch %d", first.owner, first.epoch, winning.owner, winning.epoch)
	h.compose("start", claimant)
	h.health(claimant)
	time.Sleep(time.Second)
	if state := h.operationState(submitted.Operation.ID); state != string(lifecycle.OperationAccepted) {
		t.Fatalf("replacement claim %s epoch %d left operation %s with ledger %+v; see #88", winning.owner, winning.epoch, state, h.ledgerEvidence(submitted.Operation.ID))
	}
	h.waitAttention()
	h.assertLedger(submitted.Operation.ID, 1)
}

func TestBackupAndRestore(t *testing.T) {
	h := newHarness(t, currentImage, currentImage)
	h.start()
	h.startSimulator()
	submitted := h.submit()
	attention := h.waitAttention()
	h.decide(attention)
	before := h.waitTask(submitted.Task.ID, lifecycle.TaskReadyForReview)
	artifact := before.body.Revisions[0].Revision.Artifacts[0]
	metadataBefore := h.artifactMetadata(artifact)
	externalBefore := h.artifactEvidence(artifact)
	t.Logf("backup snapshot: artifact=%s metadata=%q digest=%s size=%d", artifact, metadataBefore, externalBefore.Digest, externalBefore.Size)

	h.compose("stop", "-t", "0", "replica-a", "replica-b")
	dump := h.composeBytes(nil, "exec", "-T", "postgres", "pg_dump", "-U", "dac_migrate", "-Fc", "dac")
	dumpPath := filepath.Join("..", "..", ".work", "operations", h.project+".dump")
	t.Cleanup(func() { _ = os.Remove(dumpPath) })
	if err := os.MkdirAll(filepath.Dir(dumpPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	h.compose("exec", "-T", "postgres", "dropdb", "-U", "dac_migrate", "--force", "dac")
	h.compose("exec", "-T", "postgres", "createdb", "-U", "dac_migrate", "-T", "template0", "dac")
	h.composeBytes(dump, "exec", "-T", "postgres", "pg_restore", "-U", "dac_migrate", "-d", "dac")
	h.compose("start", "replica-a", "replica-b")
	h.health("replica-a")
	h.health("replica-b")

	after := h.task(submitted.Task.ID)
	if !bytes.Equal(mustJSON(t, before.body), mustJSON(t, after.body)) {
		t.Fatalf("restored task differs\nbefore: %s\nafter:  %s", mustJSON(t, before.body), mustJSON(t, after.body))
	}
	if metadataAfter := h.artifactMetadata(artifact); metadataAfter != metadataBefore {
		t.Fatalf("artifact metadata after restore = %q, want %q", metadataAfter, metadataBefore)
	}
	if externalAfter := h.artifactEvidence(artifact); externalAfter != externalBefore {
		t.Fatalf("external artifact after restore = %+v, want %+v", externalAfter, externalBefore)
	}
	h.review(submitted.Task.ID, after)
	h.waitTask(submitted.Task.ID, lifecycle.TaskSucceeded)
}

func TestRollingUpgradeAcrossMigration(t *testing.T) {
	h := newHarness(t, oldImage, oldImage)
	h.start()
	h.startSimulator()
	submitted := h.submit()
	attention := h.waitAttention()

	h.replicaA = nextImage
	h.recreate("replica-a")
	h.task(submitted.Task.ID)
	h.replicaB = nextImage
	h.recreate("replica-b")
	h.task(submitted.Task.ID)
	h.compose("run", "--rm", "--no-deps", "migrate-next")
	h.health("replica-a")
	h.health("replica-b")
	if version := h.sql("select max(version_id) from goose_db_version where is_applied"); version != "14" {
		t.Fatalf("schema version = %q, want 14", version)
	}
	output, err := h.composeOutput("run", "--rm", "--no-deps", "old-schema-check")
	if err == nil || !strings.Contains(output, "schema version is outside the range") {
		t.Fatalf("old image at schema 14 = %v, output %q", err, output)
	}

	h.decide(attention)
	ready := h.waitTask(submitted.Task.ID, lifecycle.TaskReadyForReview)
	h.review(submitted.Task.ID, ready)
	h.waitTask(submitted.Task.ID, lifecycle.TaskSucceeded)
	h.assertEffectKind(submitted.Operation.ID, "turn_started", 1)
	h.waitAdapterReconnect("replica-a")
	h.waitAdapterReconnect("replica-b")
	t.Logf("upgrade evidence: schema=14 images=%s,%s ledger=%+v", h.replicaA, h.replicaB, h.ledgerEvidence(submitted.Operation.ID))
}

func newHarness(t *testing.T, replicaA, replicaB string) *harness {
	t.Helper()
	name := strings.ToLower(strings.TrimPrefix(t.Name(), "Test"))
	h := &harness{t: t, project: "dac-ops-" + name, replicaA: replicaA, replicaB: replicaB}
	h.client = stackClient(t)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := h.composeOutput("logs", "--no-color", "--tail=300")
			path := filepath.Join("..", "..", ".work", "operations", h.project+".log")
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
			_ = os.WriteFile(path, []byte(logs), 0o600)
			t.Logf("failure logs: %s", path)
		}
		_, _ = h.composeOutput("down", "-v", "--remove-orphans")
	})
	return h
}

func (h *harness) start() {
	h.compose("down", "-v", "--remove-orphans")
	h.compose("up", "-d", "replica-a", "replica-b")
	h.health("replica-a")
	h.health("replica-b")
	h.token = h.clientCredentialsToken()
}

func (h *harness) startSimulator() {
	h.compose("up", "-d", "--no-deps", "sim-adapter-operations")
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := http.Get(controlRoot + "/v1/ledger/runtime/01a167fd-51d9-7000-8000-000000000001")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatal("simulator control did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *harness) recreate(service string) {
	h.compose("up", "-d", "--no-deps", "--force-recreate", service)
	h.health(service)
}

func (h *harness) health(service string) {
	deadline := time.Now().Add(45 * time.Second)
	for {
		if _, err := h.composeOutput("exec", "-T", service, "/dac-plane", "health", "ready"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s did not become ready", service)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (h *harness) registerPause(operation ids.OperationID, boundary string) {
	body := mustJSON(h.t, map[string]string{"role": "runtime", "operation_id": operation.String(), "boundary": boundary})
	response := plainRequest(h.t, http.MethodPost, controlRoot+"/v1/pauses", body)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		h.t.Fatalf("register pause = %d: %s", response.StatusCode, data)
	}
}

func (h *harness) waitPause(operation ids.OperationID, boundary string) {
	path := fmt.Sprintf("%s/v1/pauses/runtime/%s/%s", controlRoot, operation, boundary)
	h.wait("simulator pause", func() bool {
		response := plainRequest(h.t, http.MethodGet, path, nil)
		defer func() { _ = response.Body.Close() }()
		var body struct {
			Reached bool `json:"reached"`
		}
		return response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&body) == nil && body.Reached
	})
}

func (h *harness) releasePause(operation ids.OperationID, boundary string) {
	path := fmt.Sprintf("%s/v1/pauses/runtime/%s/%s/release", controlRoot, operation, boundary)
	response := plainRequest(h.t, http.MethodPost, path, nil)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		h.t.Fatalf("release pause = %d", response.StatusCode)
	}
}

type claim struct {
	owner  string
	epoch  int
	status string
}

func (h *harness) waitClaim(operation ids.OperationID, minimumEpoch int) claim {
	var found claim
	h.wait("outbox claim", func() bool {
		line := h.sql(fmt.Sprintf(`select coalesce(claimed_by,''), lease_epoch, status from outbox where record_kind='provider_operation' and record_id='%s'::uuid`, operation))
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return false
		}
		epoch, err := strconv.Atoi(parts[1])
		if err != nil || epoch < minimumEpoch || parts[0] == "" {
			return false
		}
		found = claim{owner: parts[0], epoch: epoch, status: parts[2]}
		return true
	})
	return found
}

func (h *harness) replicaForOwner(owner string) string {
	for _, service := range []string{"replica-a", "replica-b"} {
		logs, err := h.composeOutput("logs", "--no-color", service)
		if err == nil && strings.Contains(logs, owner) {
			return service
		}
	}
	h.t.Fatalf("worker owner %q was not present in replica logs", owner)
	return ""
}

func (h *harness) expireClaim(operation ids.OperationID, attempt ids.AttemptID) {
	h.sql(fmt.Sprintf(`update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where record_kind='provider_operation' and record_id='%s'::uuid; update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where attempt_id='%s'::uuid; select 'expired'`, operation, attempt))
}

func (h *harness) operationState(operation ids.OperationID) string {
	return h.sql(fmt.Sprintf(`select state from provider_operation where operation_id='%s'::uuid`, operation))
}

func (h *harness) outboxState(operation ids.OperationID) string {
	return h.sql(fmt.Sprintf(`select status||'|'||coalesce(claimed_by,'')||'|'||lease_epoch||'|'||coalesce(last_error,'') from outbox where record_kind='provider_operation' and record_id='%s'::uuid`, operation))
}

func (h *harness) waitOperationState(operation ids.OperationID, state lifecycle.OperationState) {
	h.wait("operation "+string(state), func() bool {
		return h.operationState(operation) == string(state)
	})
}

func (h *harness) assertLedger(operation ids.OperationID, effects int) {
	h.wait("simulator effect", func() bool {
		body := h.ledgerEvidence(operation)
		if body.Effects > effects {
			h.t.Fatalf("operation applied %d effects, want at most %d", body.Effects, effects)
		}
		return body.Effects == effects && body.Deliveries >= 1
	})
}

type ledgerEvidence struct {
	Deliveries int `json:"deliveries"`
	Effects    int `json:"effects"`
	Cancels    int `json:"cancels"`
	Applied    []struct {
		Kind string `json:"kind"`
	} `json:"applied"`
}

func (h *harness) ledgerEvidence(operation ids.OperationID) ledgerEvidence {
	response := plainRequest(h.t, http.MethodGet, fmt.Sprintf("%s/v1/ledger/runtime/%s", controlRoot, operation), nil)
	defer func() { _ = response.Body.Close() }()
	var body ledgerEvidence
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&body) != nil {
		h.t.Fatalf("runtime ledger unavailable: status %d", response.StatusCode)
	}
	return body
}

func (h *harness) assertEffectKind(operation ids.OperationID, kind string, count int) {
	h.wait("simulator "+kind+" effect", func() bool {
		body := h.ledgerEvidence(operation)
		found := 0
		for _, effect := range body.Applied {
			if effect.Kind == kind {
				found++
			}
		}
		if found > count {
			h.t.Fatalf("operation applied %d %s effects, want at most %d", found, kind, count)
		}
		return found == count
	})
}

func (h *harness) waitAdapterReconnect(replica string) {
	h.wait("adapter reconnect to "+replica, func() bool {
		logs, err := h.composeOutput("logs", "--no-color", "sim-adapter-operations")
		return err == nil && strings.Count(logs, "adapter connected") >= 4 && strings.Count(logs, "endpoint=wss://"+replica+":9443") >= 4
	})
}

type artifactEvidence struct {
	Present bool   `json:"present"`
	Digest  string `json:"digest"`
	Size    uint64 `json:"size"`
}

func (h *harness) artifactEvidence(artifact ids.ArtifactID) artifactEvidence {
	response := plainRequest(h.t, http.MethodGet, controlRoot+"/v1/artifacts/"+artifact.String(), nil)
	defer func() { _ = response.Body.Close() }()
	var evidence artifactEvidence
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&evidence) != nil || !evidence.Present {
		h.t.Fatalf("artifact evidence unavailable: status %d", response.StatusCode)
	}
	return evidence
}

func (h *harness) artifactMetadata(artifact ids.ArtifactID) string {
	return h.sql(fmt.Sprintf(`select digest, size_bytes, writer_epoch, media_type, kind from artifact where artifact_id='%s'::uuid`, artifact))
}

func (h *harness) submit() httpapi.SubmissionBody {
	return apiRequest[httpapi.SubmissionBody](h, "replica-a", http.MethodPost, "/tasks", map[string]string{
		"Idempotency-Key": "ops-submit-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	}, httpapi.SubmitTaskRequest{
		Provider: mustProviderID(h.t), Demand: contracts.DemandPerson, Deadline: time.Now().UTC().Add(time.Hour),
		Unit: "tokens", Ceiling: 1000, Amount: 100,
	}, http.StatusCreated).body
}

func (h *harness) waitAttention() lifecycle.AttentionRequest {
	var attention lifecycle.AttentionRequest
	h.wait("attention", func() bool {
		body := apiRequest[httpapi.AttentionListBody](h, "replica-b", http.MethodGet, "/attention", nil, nil, http.StatusOK).body
		if len(body.Attention) == 0 {
			return false
		}
		attention = body.Attention[0]
		return true
	})
	return attention
}

func (h *harness) decide(attention lifecycle.AttentionRequest) {
	apiRequest[httpapi.DecisionBody](h, "replica-a", http.MethodPost, "/attention/"+attention.ID.String()+"/decisions", map[string]string{
		"Idempotency-Key": "ops-decision-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"If-Match":        strconv.Quote(strconv.FormatUint(attention.Version, 10)),
	}, httpapi.DecisionRequest{Kind: lifecycle.DecisionAnswer, AnswerDigest: "sha256:da5f2e58d7277762a4907c15c2cba4a079b259f959e947966b9df6c68b23f267"}, http.StatusCreated)
}

type taskResponse struct {
	body   httpapi.TaskBody
	header http.Header
}

func (h *harness) task(task ids.TaskID) taskResponse {
	response := apiRequest[httpapi.TaskBody](h, "replica-b", http.MethodGet, "/tasks/"+task.String(), nil, nil, http.StatusOK)
	return taskResponse{response.body, response.header}
}

func (h *harness) waitTask(task ids.TaskID, state lifecycle.TaskState) taskResponse {
	var result taskResponse
	h.wait("task "+string(state), func() bool {
		result = h.task(task)
		return result.body.Task.State == state
	})
	return result
}

func (h *harness) review(task ids.TaskID, current taskResponse) {
	revision := current.body.Revisions[0].Revision
	apiRequest[httpapi.ReviewBody](h, "replica-a", http.MethodPost, "/tasks/"+task.String()+"/reviews", map[string]string{
		"Idempotency-Key": "ops-review-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"If-Match":        current.header.Get("ETag"),
	}, httpapi.ReviewRequest{Revision: revision.ID, Disposition: lifecycle.DispositionAccept}, http.StatusCreated)
}

func (h *harness) wait(description string, condition func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *harness) sql(query string) string {
	h.t.Helper()
	output := h.compose("exec", "-T", "postgres", "psql", "-X", "-U", "dac_migrate", "-d", "dac", "-At", "-F", "|", "-c", query)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (h *harness) compose(args ...string) string {
	h.t.Helper()
	output, err := h.composeOutput(args...)
	if err != nil {
		h.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func (h *harness) composeOutput(args ...string) (string, error) {
	h.t.Helper()
	output, err := h.composeBytesResultErr(nil, args...)
	return string(output), err
}

func (h *harness) composeBytes(input []byte, args ...string) []byte {
	h.t.Helper()
	output, err := h.composeBytesResultErr(input, args...)
	if err != nil {
		h.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func (h *harness) composeBytesResultErr(input []byte, args ...string) ([]byte, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		h.t.Fatal(err)
	}
	base := []string{"compose", "-p", h.project, "--env-file", filepath.Join(root, ".work/stack/compose.env"),
		"-f", filepath.Join(root, "deploy/compose/compose.yaml"),
		"-f", filepath.Join(root, "deploy/compose/compose.dev.yaml"),
		"-f", filepath.Join(root, "deploy/compose/compose.operations.yaml"),
		"--profile", "dev", "--profile", "operations"}
	command := exec.Command("docker", append(base, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(),
		"DAC_PLANE_IMAGE="+currentImage,
		"DAC_REPLICA_A_IMAGE="+h.replicaA,
		"DAC_REPLICA_B_IMAGE="+h.replicaB,
		"DAC_NEXT_IMAGE="+nextImage,
		"DAC_OLD_IMAGE="+oldImage,
		"DAC_SIM_ADAPTER_IMAGE="+simulatorImage,
	)
	command.Stdin = bytes.NewReader(input)
	return command.CombinedOutput()
}

type apiResponse[T any] struct {
	body   T
	header http.Header
}

func apiRequest[T any](h *harness, replica, method, path string, headers map[string]string, body any, want int) apiResponse[T] {
	h.t.Helper()
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(mustJSON(h.t, body))
	}
	target := "https://" + replica + ":8443/v1/tenants/" + tenantID + path
	request, err := http.NewRequest(method, target, input)
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		h.t.Fatal(err)
	}
	if response.StatusCode != want {
		h.t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		h.t.Fatal(err)
	}
	return apiResponse[T]{value, response.Header.Clone()}
}

func (h *harness) clientCredentialsToken() string {
	secretPath := os.Getenv("DAC_STACK_SURFACE_CLIENT_SECRET_FILE")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		h.t.Fatal(err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"dac-plane-surface"}}
	request, err := http.NewRequest(http.MethodPost, "https://issuer:8443/default/token", strings.NewReader(form.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	request.SetBasicAuth("surface-owner", strings.TrimSpace(string(secret)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&body) != nil || body.AccessToken == "" {
		h.t.Fatalf("token endpoint = %d", response.StatusCode)
	}
	return body.AccessToken
}

func stackClient(t *testing.T) *http.Client {
	ca, err := os.ReadFile(os.Getenv("DAC_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("development CA contains no certificate")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		mapped := map[string]string{
			"issuer:8443":    "127.0.0.1:18080",
			"replica-a:8443": "127.0.0.1:18443",
			"replica-b:8443": "127.0.0.1:28443",
		}[address]
		if mapped == "" {
			return nil, fmt.Errorf("operations test refuses unexpected address %q", address)
		}
		return dialer.DialContext(ctx, network, mapped)
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func plainRequest(t *testing.T, method, target string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustProviderID(t *testing.T) ids.ProviderID {
	id, err := ids.Parse[ids.Provider](providerID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
