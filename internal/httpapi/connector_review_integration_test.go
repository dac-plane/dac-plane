//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
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
	"github.com/dac-plane/dac-plane/internal/contracts"
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
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestConnectorProposalOwnerInspection(t *testing.T) {
	s, submission, c, _ := connectorReviewFixture(t)
	reviewGrant(t, s, submission.Task.ID, c, identity.PermissionAsk)
	input, err := contracts.CanonicalJSON(json.RawMessage(`{"repo":"private/repository", "number":7, "body":"exact <comment>"}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input)
	if !errors.Is(err, application.ErrConnectorApprovalPending) {
		t.Fatal(err)
	}
	open, err := store.ListOpenAttentionByAttempt(s.ctx, s.pool, s.tenant, submission.Attempt.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("approval: %+v %v", open, err)
	}
	path := "/attention/" + open[0].ID.String() + "/connector-call"
	for _, actor := range []string{"other", "reader"} {
		s.do(t, call{method: "GET", path: path, as: actor}).want(t, 404)
	}
	response := s.do(t, call{method: "GET", path: path, as: "owner"})
	if response.header.Get("ETag") != `"1"` || response.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("proposal headers: %v", response.header)
	}
	proposal := decodeInto[struct {
		Attention        ids.AttentionID  `json:"attention_id"`
		AttentionVersion uint64           `json:"attention_version"`
		Operation        ids.OperationID  `json:"operation_id"`
		Connection       ids.ConnectionID `json:"connection_id"`
		Tool             string           `json:"tool"`
		Input            json.RawMessage  `json:"input"`
		InputDigest      identity.Digest  `json:"input_digest"`
	}](t, response, 200)
	if proposal.Attention != open[0].ID || proposal.AttentionVersion != open[0].Version || proposal.Operation != pending.Operation || proposal.Connection != c.ID || proposal.Tool != "get_issue" || !bytes.Equal(proposal.Input, input) || identity.DigestOf(proposal.Input) != open[0].ActionDigest || proposal.InputDigest != pending.InputDigest {
		t.Fatalf("proposal does not describe exact approved input: %+v", proposal)
	}
	s.do(t, call{method: "POST", path: "/attention/" + open[0].ID.String() + "/decisions", as: "owner", key: "approve-proposal", ifMatch: `"1"`, body: httpapi.DecisionRequest{Kind: lifecycle.DecisionApproveOnce}}).want(t, 201)
	if _, err := application.ResumeConnectorCall(s.ctx, s.pool, s.tenant, submission.Attempt.ID, pending.Operation, json.RawMessage(`{"number":8}`)); !errors.Is(err, application.ErrConnectorDenied) {
		t.Fatalf("changed input consumed approval: %v", err)
	}
	authorized, err := application.ResumeConnectorCall(s.ctx, s.pool, s.tenant, submission.Attempt.ID, pending.Operation, input)
	if err != nil || authorized.Execution == nil || !bytes.Equal(authorized.Execution.Input, proposal.Input) {
		t.Fatalf("approved execution differs: %+v %v", authorized, err)
	}
}

type reviewPorts map[ids.ProviderID]contracts.Port

func (p reviewPorts) Port(_ ids.TenantID, provider ids.ProviderID, _ contracts.Role) contracts.Port {
	return p[provider]
}

func TestConnectorRemovalRetriesReachLaterConnections(t *testing.T) {
	s := newSurface(t)
	offline, healthy := mustID[ids.Provider](t), mustID[ids.Provider](t)
	ports := reviewPorts{healthy: &connectionPeer{}}
	s.seed(t, func(u *store.UnitOfWork) error {
		for _, provider := range []ids.ProviderID{offline, healthy} {
			if err := store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: provider, Role: contracts.RoleConnector, UpdatedAt: s.at}); err != nil {
				return err
			}
		}
		return nil
	})
	var removing []lifecycle.Connection
	for i := range 11 {
		provider := offline
		if i == 10 {
			provider = healthy
		}
		c, err := lifecycle.NewConnection(mustID[ids.Connection](t), s.tenant, provider, s.owner, "Removal fixture", s.at)
		if err != nil {
			t.Fatal(err)
		}
		s.seed(t, func(u *store.UnitOfWork) error {
			if err := store.InsertConnection(s.ctx, u, c); err != nil {
				return err
			}
			var err error
			c, err = store.RemoveConnection(s.ctx, u, s.tenant, c.ID, store.Transition{ExpectedVersion: 1, At: s.at.Add(time.Duration(i+1) * time.Microsecond), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: mustID[ids.Audit](t)})
			return err
		})
		removing = append(removing, c)
	}
	for range 2 {
		if err := application.FinishConnectionRemovals(s.ctx, s.pool, ports, s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	later, err := store.GetConnection(s.ctx, s.pool, s.tenant, removing[10].ID)
	if err != nil || later.State != lifecycle.ConnectionRemoved {
		t.Fatalf("healthy eleventh removal starved: %+v %v", later, err)
	}
	for _, before := range removing[:10] {
		after, err := store.GetConnection(s.ctx, s.pool, s.tenant, before.ID)
		if err != nil || after.State != lifecycle.ConnectionRemoving || before.Version != after.Version || !before.UpdatedAt.Equal(after.UpdatedAt) {
			t.Fatalf("retry changed lifecycle state: %+v %v", after, err)
		}
	}
}

func TestConnectorCatalogueKeepsHealthyTools(t *testing.T) {
	s, submission, healthy, ports := connectorReviewFixture(t)
	reviewGrant(t, s, submission.Task.ID, healthy, identity.PermissionAlways)
	provider := mustID[ids.Provider](t)
	offline, err := lifecycle.NewConnection(mustID[ids.Connection](t), s.tenant, provider, s.owner, "Offline catalogue", s.at)
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error {
		if err := store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: provider, Role: contracts.RoleConnector, UpdatedAt: s.at}); err != nil {
			return err
		}
		if err := store.InsertConnection(s.ctx, u, offline); err != nil {
			return err
		}
		var err error
		offline, err = store.CompleteConnection(s.ctx, u, s.tenant, offline.ID, lifecycle.ConnectionAccount{Account: "offline", Permissions: []string{"issues:read"}}, store.Transition{ExpectedVersion: 1, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: mustID[ids.Audit](t)})
		return err
	})
	reviewGrant(t, s, submission.Task.ID, offline, identity.PermissionAlways)
	tokens, err := connectorgateway.NewTokens(bytes.Repeat([]byte("c"), 32))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway, err := connectorgateway.New(s.pool, ports, tokens, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", connectorgateway.Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tokens.Issue(s.tenant, submission.Attempt.ID))
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, req)
	var result struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Error) > 0 || len(result.Result.Tools) != 1 || result.Result.Tools[0].Name != healthy.ID.String()+"__get_issue" {
		t.Fatalf("healthy tools hidden by offline connector: %s", response.Body.String())
	}
	if !strings.Contains(logs.String(), offline.ID.String()) || !strings.Contains(logs.String(), provider.String()) || strings.Contains(logs.String(), "private-review-credential") {
		t.Fatalf("unsafe or missing outage report: %s", logs.String())
	}
}
