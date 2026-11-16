//go:build integration

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/connectorgateway"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/dac-plane/dac-plane/internal/simulator"
	"github.com/dac-plane/dac-plane/internal/testsupport/wiretest"
	"github.com/dac-plane/dac-plane/internal/worker"
)

// TestConnectorGatewayServesAStartingAttempt covers how Codex 0.165.0 uses the
// gateway: it initializes its MCP servers and lists their tools when the
// thread starts, before the runtime reports it started. A gateway that
// refused the starting attempt left the thread with no connector tools for
// its whole life, which the live qualification observed. Listing is allowed;
// a call still waits for the attempt to execute.
func TestConnectorGatewayServesAStartingAttempt(t *testing.T) {
	s := newSurface(t)
	submission := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "task", body: s.submitBody()}), 201)
	s.dispatch(t, submission.Operation.ID)
	if a, err := store.GetAttempt(s.ctx, s.pool, s.tenant, submission.Attempt.ID); err != nil || a.State != lifecycle.AttemptStarting {
		t.Fatalf("attempt after dispatch = %+v, %v; want starting", a, err)
	}

	connectorID := mustID[ids.Provider](t)
	connector, err := simulator.NewConnector(simulator.Config{Tenant: s.tenant, Provider: connectorID})
	if err != nil {
		t.Fatal(err)
	}
	wire := wiretest.For(t, s.pool)
	wire.Serve(t, s.tenant, connectorID, connector)
	c, err := lifecycle.NewConnection(mustID[ids.Connection](t), s.tenant, connectorID, s.owner, "Fixture", s.at)
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error {
		if err := store.InsertConnection(s.ctx, u, c); err != nil {
			return err
		}
		var err error
		c, err = store.CompleteConnection(s.ctx, u, s.tenant, c.ID, lifecycle.ConnectionAccount{Account: "fixture", Permissions: []string{"issues:read"}}, store.Transition{ExpectedVersion: 1, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: mustID[ids.Audit](t)})
		return err
	})
	connector.Consent(c.ID, "starting-attempt-credential")
	g, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, lifecycle.ConnectionGrantTerms{
		Task: submission.Task.ID, Tools: []string{"get_issue"}, Permission: identity.PermissionAlways, CallLimit: 2,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, g) })

	tokens, err := connectorgateway.NewTokens(bytes.Repeat([]byte("s"), 32))
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "starting-worker", worker.WithPorts(wire.Registry))
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := connectorgateway.New(s.pool, wire.Registry, tokens, func() {
		if err := w.Tick(s.ctx); err != nil {
			t.Errorf("gateway dispatch: %v", err)
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gateway)
	defer server.Close()
	token := tokens.Issue(s.tenant, submission.Attempt.ID)
	for _, method := range []string{"initialize", "tools/list"} {
		req, _ := http.NewRequestWithContext(s.ctx, "POST", server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{"protocolVersion":"2025-06-18"}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s for a starting attempt = %d %s", method, resp.StatusCode, body)
		}
		if method == "tools/list" {
			var listed struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal(body, &listed) != nil || len(listed.Result.Tools) != 1 || !strings.HasSuffix(listed.Result.Tools[0].Name, "get_issue") {
				t.Fatalf("tools/list for a starting attempt = %s", body)
			}
		}
	}
	_, err = application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", json.RawMessage(`{"number":1}`))
	if err == nil || errors.Is(err, application.ErrConnectorApprovalPending) {
		t.Fatalf("a call from a starting attempt = %v, want it refused", err)
	}

	// A runtime may call before the worker applies its start report. The
	// gateway holds the call until the attempt executes instead of refusing it.
	answered := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(s.ctx, "POST", server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+c.ID.String()+`__get_issue","arguments":{"number":2}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := server.Client().Do(req)
		if err != nil {
			answered <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		answered <- string(body)
	}()
	select {
	case body := <-answered:
		t.Fatalf("a call from a starting attempt answered before the attempt executed: %s", body)
	case <-time.After(time.Second):
	}
	s.seed(t, func(u *store.UnitOfWork) error {
		a, err := store.GetAttempt(s.ctx, u, s.tenant, submission.Attempt.ID)
		if err != nil {
			return err
		}
		_, _, err = store.ApplyAttempt(s.ctx, u, s.tenant, a.ID, lifecycle.AttemptRuntimeStarted, "", store.Transition{ExpectedVersion: a.Version, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}, AuditID: mustID[ids.Audit](t)})
		return err
	})
	select {
	case body := <-answered:
		if !strings.Contains(body, "Simulated issue 2") || strings.Contains(body, `"isError":true`) {
			t.Fatalf("held call answered %s", body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("held call never completed after the attempt executed")
	}
}
