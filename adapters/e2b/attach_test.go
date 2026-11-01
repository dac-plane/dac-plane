package e2badapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

// fakeSandboxPorts answers envd's process service and the exec-server port.
type fakeSandboxPorts struct {
	mu          sync.Mutex
	commands    []string
	tokens      []string
	refuseStart bool
	notReady    int
}

func (f *fakeSandboxPorts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		if f.notReady > 0 {
			f.notReady--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var start struct {
		Process struct {
			Args []string `json:"args"`
		} `json:"process"`
	}
	if len(raw) < 5 || json.Unmarshal(raw[5:], &start) != nil || len(start.Process.Args) != 3 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.tokens = append(f.tokens, r.Header.Get("X-Access-Token"))
	f.commands = append(f.commands, start.Process.Args[2])
	var flags byte
	if f.refuseStart {
		flags = connectEndStream
	}
	frame := []byte{flags, 0, 0, 0, 0}
	message := []byte(`{"event":{"start":{"pid":7}}}`)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	_, _ = w.Write(append(frame, message...))
}

func attachingAdapter(t *testing.T, fake *fakeE2B, ports *fakeSandboxPorts) *Adapter {
	t.Helper()
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	sandboxes := httptest.NewServer(ports)
	t.Cleanup(sandboxes.Close)
	store, err := journal.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a, err := New(Config{BaseURL: api.URL, APIKey: "e2b-test-secret", TemplateID: "base", ProviderID: "e2b-test", Journal: store, AttachKey: []byte("attach-key"), SandboxURL: func(int, string) string { return sandboxes.URL }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func taskProvision(operation, delivery string) wire.Request {
	return wire.Request{Operation: operation, Delivery: delivery, Tenant: "tenant-1", Attempt: "attempt-1", Role: wire.RoleEnvironment, Method: "provision", Arguments: []byte(`{}`)}
}

func resourceRef(t *testing.T, reply wire.Reply) string {
	t.Helper()
	var result taskProvisionResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result.ResourceRef
}

// The value is pinned because the runtime adapter derives the same token, and
// its test pins the same value.
func TestAttachTokenDerivation(t *testing.T) {
	const want = "6d47dc03aab35e65cca42dbc5267090a7ed4fdf847d79892de91dd07f1a28984"
	if got := attachToken([]byte("attach-key"), "sandbox:abc"); got != want {
		t.Fatalf("attach token = %s", got)
	}
}

func TestProvisionStartsExecServerWithOnlyTheTokenHash(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	ports := &fakeSandboxPorts{notReady: 2}
	a := attachingAdapter(t, fake, ports)
	reply, err := a.Send(context.Background(), taskProvision("operation-1", "delivery-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeOK || !fake.secure {
		t.Fatalf("reply=%+v secure=%t", reply, fake.secure)
	}
	token := attachToken([]byte("attach-key"), resourceRef(t, reply))
	sum := sha256.Sum256([]byte(token))
	if len(ports.commands) != 1 || !strings.Contains(ports.commands[0], hex.EncodeToString(sum[:])) || strings.Contains(ports.commands[0], token) {
		t.Fatalf("exec-server command does not carry only the hash: %q", ports.commands)
	}
	if ports.tokens[0] != "envd-token" {
		t.Fatalf("envd token = %q", ports.tokens[0])
	}
	if ports.notReady != 0 {
		t.Fatalf("provision reported before exec-server listened: %d polls left", ports.notReady)
	}
}

func TestProvisionWithoutAttachKeyStartsNothing(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a, _ := newTestAdapter(t, fake)
	reply, err := a.Send(context.Background(), taskProvision("operation-1", "delivery-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeOK || fake.secure {
		t.Fatalf("reply=%+v secure=%t", reply, fake.secure)
	}
}

func TestSandboxThatCannotServeCommandsIsDeletedAndReplaced(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	ports := &fakeSandboxPorts{refuseStart: true}
	a := attachingAdapter(t, fake, ports)
	if _, err := a.Send(context.Background(), taskProvision("operation-1", "delivery-1")); err == nil {
		t.Fatal("provision succeeded without exec-server")
	}
	if fake.deleted != 1 || len(fake.sandboxes) != 0 {
		t.Fatalf("failed sandbox survived: deleted=%d live=%d", fake.deleted, len(fake.sandboxes))
	}
	ports.refuseStart = false
	reply, err := a.Send(context.Background(), taskProvision("operation-1", "delivery-2"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeOK || fake.created != 2 {
		t.Fatalf("reply=%+v creates=%d", reply, fake.created)
	}
}

func TestLostCreateReplyStillStartsExecServer(t *testing.T) {
	fake := &fakeE2B{sandboxes: map[string]sandbox{}, dropFirstCreateReply: true}
	ports := &fakeSandboxPorts{}
	a := attachingAdapter(t, fake, ports)
	reply, err := a.Send(context.Background(), taskProvision("operation-1", "delivery-1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Outcome != wire.OutcomeOK || fake.created != 1 || fake.connected != 1 {
		t.Fatalf("reply=%+v creates=%d connects=%d", reply, fake.created, fake.connected)
	}
	if len(ports.tokens) != 1 || ports.tokens[0] != "envd-token" {
		t.Fatalf("envd tokens = %q", ports.tokens)
	}
}

func TestExecServerThatNeverListensFailsWithinItsBound(t *testing.T) {
	ports := &fakeSandboxPorts{notReady: 1 << 30}
	fake := &fakeE2B{sandboxes: map[string]sandbox{}}
	a := attachingAdapter(t, fake, ports)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := a.waitForExecServer(ctx, "sandbox-1")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("did not start listening")) {
		t.Fatalf("err = %v", err)
	}
}
