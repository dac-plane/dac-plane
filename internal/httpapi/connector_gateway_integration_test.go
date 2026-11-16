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
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/dac-plane/dac-plane/internal/simulator"
	"github.com/dac-plane/dac-plane/internal/testsupport/wiretest"
	"github.com/dac-plane/dac-plane/internal/worker"
)

func TestConnectorGatewayGovernance(t *testing.T) {
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
		audit := mustID[ids.Audit](t)
		var err error
		c, err = store.CompleteConnection(s.ctx, u, s.tenant, c.ID, lifecycle.ConnectionAccount{Account: "fixture", Permissions: []string{"issues:write"}}, store.Transition{ExpectedVersion: 1, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: audit})
		return err
	})
	const canary = "connector-canary-credential-must-stay-private"
	connector.Consent(c.ID, canary)
	input := json.RawMessage(`{"number":1}`)
	if _, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input); !errors.Is(err, application.ErrConnectorDenied) {
		t.Fatalf("without grant: %v", err)
	}
	terms := lifecycle.ConnectionGrantTerms{Task: submission.Task.ID, Tools: []string{"get_issue"}, Permission: identity.PermissionAsk, CallLimit: 1, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	g, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, terms, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, g) })
	pending, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input)
	if !errors.Is(err, application.ErrConnectorApprovalPending) || pending.State != store.CallAwaitingApproval {
		t.Fatalf("ask = %+v %v", pending, err)
	}
	replay, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input)
	if !errors.Is(err, application.ErrConnectorApprovalPending) || replay.Operation != pending.Operation {
		t.Fatalf("pending replay = %+v %v", replay, err)
	}
	open, err := store.ListOpenAttentionByAttempt(s.ctx, s.pool, s.tenant, submission.Attempt.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("attention: %+v %v", open, err)
	}
	s.do(t, call{method: "POST", path: "/attention/" + open[0].ID.String() + "/decisions", as: "owner", key: "approve", ifMatch: `"1"`, body: httpapi.DecisionRequest{Kind: lifecycle.DecisionApproveOnce}}).want(t, 201)
	approved, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input)
	if err != nil || approved.Operation != pending.Operation {
		t.Fatalf("approved: %+v %v", approved, err)
	}
	// Both later dispatch locks must be acquired before the final expiry check.
	// A grant remains immutable in normal operation; shortening this fixture's
	// expiry makes a lock wait observable without waiting ten minutes.
	originalExpiry := open[0].ExpiresAt
	for _, scope := range []string{"budget", "operation"} {
		lock, err := store.Begin(s.ctx, s.pool)
		if err != nil {
			t.Fatal(err)
		}
		table, column, id := "budget", "budget_id", submission.Budget.ID.String()
		if scope == "operation" {
			table, column, id = "provider_operation", "operation_id", approved.Operation.String()
		}
		if _, err := lock.Exec(s.ctx, `select 1 from `+table+` where tenant_id=$1 and `+column+`=$2 for update`, s.tenant.String(), id); err != nil {
			t.Fatal(err)
		}
		expires := time.Now().UTC().Add(2 * time.Second)
		if _, err := s.pool.Exec(s.ctx, `update authority_grant set expires_at=$3 where tenant_id=$1 and grant_id=$2`, s.tenant.String(), approved.Execution.Grant.String(), expires); err != nil {
			t.Fatal(err)
		}
		type checked struct {
			reason identity.DenialReason
			err    error
		}
		done := make(chan checked, 1)
		go func() {
			u, err := store.Begin(s.ctx, s.pool)
			if err != nil {
				done <- checked{err: err}
				return
			}
			reason, err := application.CheckConnectorDispatch(s.ctx, u, s.tenant, approved.Operation)
			_ = u.Rollback(s.ctx)
			done <- checked{reason, err}
		}()
		timer := time.NewTimer(time.Until(expires.Add(100 * time.Millisecond)))
		<-timer.C
		if err := lock.Rollback(s.ctx); err != nil {
			t.Fatal(err)
		}
		result := <-done
		if result.err != nil || result.reason != identity.DenialExpired {
			t.Fatalf("%s lock allowed expired approval: %+v", scope, result)
		}
		if _, err := s.pool.Exec(s.ctx, `update authority_grant set expires_at=$3 where tenant_id=$1 and grant_id=$2`, s.tenant.String(), approved.Execution.Grant.String(), originalExpiry); err != nil {
			t.Fatal(err)
		}
	}
	w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "connector-worker", worker.WithPorts(wire.Registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}
	result, err := application.ConnectorCallResult(s.ctx, s.pool, s.tenant, approved.Operation)
	if err != nil || result.Result == nil {
		t.Fatalf("result: %+v %v", result, err)
	}
	if connector.Ledger().Deliveries(approved.Operation) != 1 {
		t.Fatal("wrong number of sends")
	}
	// The call ceiling applies to further calls, even identical ones.
	if _, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input); !errors.Is(err, application.ErrConnectorDenied) {
		t.Fatalf("ceiling: %v", err)
	}
	// Exercise the actual MCP boundary and prove authentication / origin checks.
	tokens, err := connectorgateway.NewTokens(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := connectorgateway.New(s.pool, wire.Registry, tokens, func() {
		if err := w.Tick(s.ctx); err != nil {
			t.Errorf("gateway dispatch: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gateway)
	defer server.Close()
	rpc := func(token, origin, method string) *http.Response {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"protocolVersion":"2025-06-18"}}`
		if method == "tools/call" {
			body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + c.ID.String() + `__get_issue","arguments":{"number":2}}}`
		}
		req, _ := http.NewRequestWithContext(s.ctx, "POST", server.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	token := tokens.Issue(s.tenant, submission.Attempt.ID)
	for _, tc := range []struct {
		token, origin, method string
		status                int
	}{{"invalid", "", "initialize", 401}, {token, "https://evil.example", "initialize", 403}, {token, "", "initialize", 200}, {token, "", "tools/list", 200}} {
		resp := rpc(tc.token, tc.origin, tc.method)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("RPC %s: %d %s", tc.method, resp.StatusCode, body)
		}
		if bytes.Contains(body, []byte(canary)) {
			t.Fatal("credential reached runtime")
		}
	}
	// An exhausted older grant cannot shadow a fresh covering task grant.
	overlapTerms := terms
	overlapTerms.Permission = identity.PermissionAlways
	overlapTerms.CallLimit = 2
	overlap, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, overlapTerms, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, overlap) })
	extra, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", json.RawMessage(`{"number":99}`))
	if err != nil || extra.ConnectionGrant != overlap.ID {
		t.Fatalf("fresh grant shadowed by exhausted older one: %+v %v", extra, err)
	}
	if err := w.Tick(s.ctx); err != nil {
		t.Fatal(err)
	}
	s.do(t, call{method: "POST", path: "/connection-grants/" + overlap.ID.String() + "/revoke", as: "owner", key: "overlap-revoke", ifMatch: `"1"`, body: struct{}{}}).want(t, 200)
	// A revoked grant disappears immediately during a running attempt.
	s.do(t, call{method: "POST", path: "/connection-grants/" + g.ID.String() + "/revoke", as: "owner", key: "revoke", ifMatch: `"1"`, body: struct{}{}}).want(t, 200)
	if _, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", input); !errors.Is(err, application.ErrConnectorDenied) {
		t.Fatalf("revoked: %v", err)
	}
	// Run a complete MCP tools/call through authorization and the real wire.
	terms.Permission = identity.PermissionAlways
	terms.CallLimit = 5
	g2, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, terms, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, g2) })
	resp := rpc(token, "", "tools/call")
	rawResult, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Contains(rawResult, []byte(`"isError":false`)) {
		t.Fatalf("MCP call: %s %v", rawResult, err)
	}
	var allowed, denied int
	if err := s.pool.QueryRow(s.ctx, `select count(*) filter(where outcome='allowed'),count(*) filter(where outcome='denied') from authority_use where tenant_id=$1`, s.tenant.String()).Scan(&allowed, &denied); err != nil {
		t.Fatal(err)
	}
	if allowed != 3 || denied < 3 {
		t.Fatalf("authority uses allowed=%d denied=%d", allowed, denied)
	}
	var noGrant int
	if err := s.pool.QueryRow(s.ctx, `select count(*) from authority_use where tenant_id=$1 and reason='no_grant'`, s.tenant.String()).Scan(&noGrant); err != nil {
		t.Fatal(err)
	}
	if noGrant == 0 {
		t.Fatal("missing no_grant audit reason")
	}
	// An expired approval is closed and releases the product wait.
	s.do(t, call{method: "POST", path: "/connection-grants/" + g2.ID.String() + "/revoke", as: "owner", key: "revoke-second", ifMatch: `"1"`, body: struct{}{}}).want(t, 200)
	terms.Permission = identity.PermissionAsk
	g3, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, terms, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	s.seed(t, func(u *store.UnitOfWork) error { return store.InsertConnectionGrant(s.ctx, u, g3) })
	waiting, err := application.CallConnector(s.ctx, s.pool, s.tenant, submission.Attempt.ID, c.ID, "get_issue", json.RawMessage(`{"number":3}`))
	if !errors.Is(err, application.ErrConnectorApprovalPending) {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(s.ctx, `update attention_request set expires_at=created_at+interval '1 microsecond' where tenant_id=$1 and operation_id=$2`, s.tenant.String(), waiting.Operation.String()); err != nil {
		t.Fatal(err)
	}
	if err := application.ExpireConnectorApprovals(s.ctx, s.pool, s.tenant); err != nil {
		t.Fatal(err)
	}
	a, err := store.GetAttempt(s.ctx, s.pool, s.tenant, submission.Attempt.ID)
	if err != nil || a.State != lifecycle.AttemptExecuting {
		t.Fatalf("expired approval left attempt %+v %v", a, err)
	}
	expired, err := store.GetConnectorCall(s.ctx, s.pool, s.tenant, waiting.Operation)
	if err != nil || expired.State != store.CallDenied {
		t.Fatal("expired call still pending")
	}
	// Scan every public-schema row, not just selected tables, for credential leakage.
	tables, err := s.pool.Query(s.ctx, `select tablename from pg_tables where schemaname='public'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()
	for _, name := range names {
		rows, err := s.pool.Query(context.Background(), `select row_to_json(t)::text from "`+name+`" t`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(raw, canary) {
				t.Fatalf("credential in table %s", name)
			}
		}
		rows.Close()
	}
	raw, err := json.Marshal(connector.Ledger().Sent())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatal("credential in provider frame")
	}
}
