//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
)

func connectorReviewFixture(t *testing.T) (*surface, httpapi.SubmissionBody, lifecycle.Connection, application.ConnectorPorts) {
	t.Helper()
	s := newSurface(t)
	submission := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "task", body: s.submitBody()}), 201)
	s.dispatch(t, submission.Operation.ID)
	s.seed(t, func(u *store.UnitOfWork) error {
		a, err := store.GetAttempt(s.ctx, u, s.tenant, submission.Attempt.ID)
		if err != nil {
			return err
		}
		_, _, err = store.ApplyAttempt(s.ctx, u, s.tenant, a.ID, lifecycle.AttemptRuntimeStarted, "", store.Transition{ExpectedVersion: a.Version, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorSystem}, AuditID: mustID[ids.Audit](t)})
		return err
	})
	provider := mustID[ids.Provider](t)
	connector, err := simulator.NewConnector(simulator.Config{Tenant: s.tenant, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	wire := wiretest.For(t, s.pool)
	wire.Serve(t, s.tenant, provider, connector)
	c, err := lifecycle.NewConnection(mustID[ids.Connection](t), s.tenant, provider, s.owner, "Review fixture", s.at)
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error {
		if err := store.InsertConnection(s.ctx, u, c); err != nil {
			return err
		}
		var err error
		c, err = store.CompleteConnection(s.ctx, u, s.tenant, c.ID, lifecycle.ConnectionAccount{Account: "fixture", Permissions: []string{"issues:write"}}, store.Transition{ExpectedVersion: 1, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: mustID[ids.Audit](t)})
		return err
	})
	connector.Consent(c.ID, "private-review-credential")
	return s, submission, c, wire.Registry
}

func reviewGrant(t *testing.T, s *surface, task ids.TaskID, c lifecycle.Connection, permission identity.Permission) lifecycle.ConnectionGrant {
	t.Helper()
	g, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, lifecycle.ConnectionGrantTerms{Task: task, Tools: []string{"get_issue"}, Permission: permission, CallLimit: 10, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, g) })
	return g
}

func TestConnectorGatewayDenialBindsWaitingRequest(t *testing.T) {
	s, submission, c, ports := connectorReviewFixture(t)
	reviewGrant(t, s, submission.Task.ID, c, identity.PermissionAsk)
	tokens, err := connectorgateway.NewTokens(bytes.Repeat([]byte("r"), 32))
	if err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan struct{}, 1)
	gateway, err := connectorgateway.New(s.pool, ports, tokens, func() {
		select {
		case dispatched <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gateway)
	defer server.Close()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + c.ID.String() + `__get_issue","arguments":{"number":7}}}`
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tokens.Issue(s.tenant, submission.Attempt.ID))
	type outcome struct {
		body []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		done <- outcome{body: raw, err: err}
	}()
	var attention lifecycle.AttentionRequest
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for attention.ID.IsZero() {
		open, err := store.ListOpenAttentionByAttempt(ctx, s.pool, s.tenant, submission.Attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 1 {
			attention = open[0]
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("gateway did not create an approval")
		case <-ticker.C:
		}
	}
	reviewGrant(t, s, submission.Task.ID, c, identity.PermissionAlways)
	s.do(t, call{method: "POST", path: "/attention/" + attention.ID.String() + "/decisions", as: "owner", key: "deny", ifMatch: `"1"`, body: httpapi.DecisionRequest{Kind: lifecycle.DecisionDeny}}).want(t, 201)
	select {
	case <-dispatched:
		t.Fatal("denied waiting request dispatched under a different grant")
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		var reply struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(result.body, &reply); err != nil || !reply.Result.IsError {
			t.Fatalf("denied call response: %s, %v", result.body, err)
		}
	case <-ctx.Done():
		t.Fatal("denied request did not finish")
	}
	var count int
	if err := s.pool.QueryRow(s.ctx, `select count(*) from connector_call where tenant_id=$1 and state='authorized'`, s.tenant.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("denied request authorized %d calls", count)
	}
	stored, err := store.GetConnectorCall(s.ctx, s.pool, s.tenant, attention.Operation)
	if err != nil || stored.State != store.CallDenied {
		t.Fatalf("denied operation: %+v %v", stored, err)
	}
}
