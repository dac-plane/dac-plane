package worker

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPool is a non-nil pool stand-in: New only nil-checks it, never uses
// it on the validation paths under test.
func testPool() *pgxpool.Pool {
	return new(pgxpool.Pool)
}

func mustOperationID(t *testing.T) ids.OperationID {
	t.Helper()
	id, err := ids.New[ids.Operation]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}

func TestParseDispatchBodyRoundTrip(t *testing.T) {
	operation := mustOperationID(t)
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	payload := []byte(`{"operation":"` + operation.String() + `","role":"runtime","method":"start_turn",` +
		`"arguments":{"turn":1},"deadline":"` + deadline.Format(time.RFC3339) + `",` +
		`"unit":"tokens","amount":7,"cancel_reason":""}`)
	body, err := ParseDispatchBody(payload)
	if err != nil {
		t.Fatalf("ParseDispatchBody() error = %v", err)
	}
	if body.Operation != operation || body.Role != contracts.RoleRuntime ||
		body.Method != contracts.MethodStartTurn || body.Unit != lifecycle.Unit("tokens") ||
		body.Amount != 7 || !body.Deadline.Equal(deadline) || body.CancelReason != "" {
		t.Fatalf("ParseDispatchBody() = %+v, want the envelope back", body)
	}
	if string(body.Arguments) != `{"turn":1}` {
		t.Fatalf("ParseDispatchBody() arguments = %s", body.Arguments)
	}
}

func TestParseDispatchBodyRejects(t *testing.T) {
	operation := mustOperationID(t).String()
	cases := map[string]string{
		"malformed json":    `{"operation":`,
		"missing operation": `{"role":"runtime","method":"start_turn","deadline":"2030-01-01T00:00:00Z"}`,
		"bad operation id":  `{"operation":"nope","role":"runtime","method":"start_turn","deadline":"2030-01-01T00:00:00Z"}`,
		"missing role":      `{"operation":"` + operation + `","method":"start_turn","deadline":"2030-01-01T00:00:00Z"}`,
		"missing method":    `{"operation":"` + operation + `","role":"runtime","deadline":"2030-01-01T00:00:00Z"}`,
		"missing deadline":  `{"operation":"` + operation + `","role":"runtime","method":"start_turn"}`,
	}
	for name, payload := range cases {
		if _, err := ParseDispatchBody([]byte(payload)); err == nil {
			t.Fatalf("ParseDispatchBody(%s) succeeded, want an error", name)
		}
	}
}

func TestParseDispatchBodyCancel(t *testing.T) {
	operation := mustOperationID(t).String()
	body, err := ParseDispatchBody([]byte(`{"operation":"` + operation + `","role":"runtime",` +
		`"method":"interrupt","deadline":"2030-01-01T00:00:00Z","cancel_reason":"user stop"}`))
	if err != nil {
		t.Fatalf("ParseDispatchBody() error = %v", err)
	}
	if body.CancelReason != "user stop" {
		t.Fatalf("ParseDispatchBody() cancel reason = %q", body.CancelReason)
	}
}

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}
	broken := DefaultConfig()
	broken.PollInterval = 0
	if err := broken.Validate(); err == nil {
		t.Fatal("Validate() with zero poll interval succeeded")
	}
	broken = DefaultConfig()
	broken.Policy.MaxAttempts = 0
	if err := broken.Validate(); err == nil {
		t.Fatal("Validate() with zero max attempts succeeded")
	}
}

func TestNewRejectsBadWiring(t *testing.T) {
	if _, err := New(nil, testLogger(), nil, DefaultConfig(), "owner"); err == nil {
		t.Fatal("New() without a pool succeeded")
	}
	if _, err := New(testPool(), nil, nil, DefaultConfig(), "owner"); err == nil {
		t.Fatal("New() without a logger succeeded")
	}
	if _, err := New(testPool(), testLogger(), nil, DefaultConfig(), ""); err == nil {
		t.Fatal("New() without an owner succeeded")
	}
	broken := DefaultConfig()
	broken.BatchLimit = 0
	if _, err := New(testPool(), testLogger(), nil, broken, "owner"); err == nil {
		t.Fatal("New() with a zero batch limit succeeded")
	}
}

func TestLoopbackPortDefaultsAndScripts(t *testing.T) {
	port := NewLoopbackPort(contracts.RoleRuntime)
	if port.Role() != contracts.RoleRuntime {
		t.Fatalf("Role() = %s", port.Role())
	}
	operation := mustOperationID(t)
	tenant, _ := ids.New[ids.Tenant]()
	attempt, _ := ids.New[ids.Attempt]()
	delivery, _ := ids.New[ids.Delivery]()
	request := contracts.Request{
		Operation: operation, Delivery: delivery, Tenant: tenant, Attempt: attempt,
		Role: contracts.RoleRuntime, Method: contracts.MethodStartTurn,
		Demand: contracts.DemandPerson, Deadline: time.Now().UTC().Add(time.Minute),
	}
	reply, err := port.Send(t.Context(), request)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		t.Fatalf("default Send() = %+v, want ok/accepted", reply)
	}
	if reply.Operation != operation || reply.Delivery != delivery {
		t.Fatal("default Send() did not stamp the request identities")
	}
	if len(port.Sent()) != 1 {
		t.Fatalf("Sent() = %d requests, want 1", len(port.Sent()))
	}
	port.ScriptSendError(operation, errors.New("boom"))
	if _, err := port.Send(t.Context(), request); err == nil {
		t.Fatal("Send() with a scripted error succeeded")
	}
	if err := port.Cancel(t.Context(), contracts.Cancel{Operation: operation, Delivery: delivery}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if len(port.Cancels()) != 1 {
		t.Fatalf("Cancels() = %d, want 1", len(port.Cancels()))
	}
}
