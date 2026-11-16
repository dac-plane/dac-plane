//go:build integration

package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/platform/migrations"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

// connectionPeer models an external credential holder; only safe projections
// are returned to the surface. Its mutable fields are guarded for HTTP tests.
type connectionPeer struct {
	contracts.Port
	mu      sync.Mutex
	offline bool
	status  contracts.AuthorizationStatus
	starts  int
}

func (p *connectionPeer) PortForTest()         {}
func (p *connectionPeer) Role() contracts.Role { return contracts.RoleConnector }
func (p *connectionPeer) Send(_ context.Context, r contracts.Request) (contracts.Reply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offline {
		return contracts.Reply{}, errors.New("offline")
	}
	var value any
	switch r.Method {
	case contracts.MethodListTools:
		value = contracts.ToolCatalogue{Tools: []contracts.ConnectorTool{{Name: "read_issue", Description: "Read an issue", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true}}}
	case contracts.MethodBeginAuthorization:
		p.starts++
		value = contracts.AuthorizationStarted{URL: "https://connector.example/consent", ExpiresAt: time.Now().UTC().Add(time.Minute)}
	case contracts.MethodCompleteAuthorization:
		result := contracts.AuthorizationResult{Status: p.status}
		if p.status == contracts.AuthorizationAuthorized {
			result.Account = &lifecycle.ConnectionAccount{Account: "octocat", Permissions: []string{"issues:read"}}
		}
		value = result
	case contracts.MethodRevokeConnection:
		value = struct{}{}
	default:
		return contracts.Reply{}, errors.New("unexpected method")
	}
	raw, err := json.Marshal(value)
	return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: raw}, err
}

type connectionPorts struct{ peer *connectionPeer }

func (p connectionPorts) Port(_ ids.TenantID, _ ids.ProviderID, _ contracts.Role) contracts.Port {
	return p.peer
}

func TestSurfaceConnectionsThroughHTTP(t *testing.T) {
	peer := &connectionPeer{status: contracts.AuthorizationPending}
	s := newSurfaceWith(t, httpapi.Options{Connectors: connectionPorts{peer}})
	s.seed(t, func(u *store.UnitOfWork) error {
		return store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: s.provider, Role: contracts.RoleConnector, UpdatedAt: s.at})
	})
	body := httpapi.ConnectionRequest{Provider: s.provider, Name: "My account"}
	created := decodeInto[httpapi.ConnectionCreatedBody](t, s.do(t, call{method: "POST", path: "/connections", as: "owner", key: "create", body: body}), 201)
	c := created.Connection
	path := "/connections/" + c.ID.String()
	if c.State != lifecycle.ConnectionAuthorizing || c.Owner != s.owner || created.Authorization == nil {
		t.Fatalf("created: %+v", created)
	}
	replay := decodeInto[httpapi.ConnectionCreatedBody](t, s.do(t, call{method: "POST", path: "/connections", as: "owner", key: "create", body: body}), 201)
	if replay.Connection.ID != c.ID {
		t.Fatal("replay created another connection")
	}
	decodeInto[httpapi.ConnectionListBody](t, s.do(t, call{method: "GET", path: "/connections", as: "other"}), 200)
	decodeInto[lifecycle.Connection](t, s.do(t, call{method: "GET", path: path, as: "other"}), 200)
	s.do(t, call{method: "POST", path: path + "/authorize", as: "owner", body: struct{}{}}).want(t, 400)
	s.do(t, call{method: "POST", path: path + "/authorize", as: "other", key: "auth", body: struct{}{}}).want(t, 403)
	for range 2 {
		decodeInto[contracts.AuthorizationStarted](t, s.do(t, call{method: "POST", path: path + "/authorize", as: "owner", key: "auth", body: struct{}{}}), 200)
	}
	peer.mu.Lock()
	starts := peer.starts
	peer.mu.Unlock()
	if starts != 3 {
		t.Fatalf("began authorization %d times; want create, create replay, authorize once", starts)
	}
	s.do(t, call{method: "POST", path: path + "/complete", as: "owner", key: "complete", ifMatch: `"1"`, body: struct{}{}}).want(t, 409)
	peer.mu.Lock()
	peer.status = contracts.AuthorizationAuthorized
	peer.mu.Unlock()
	c = decodeInto[lifecycle.Connection](t, s.do(t, call{method: "POST", path: path + "/complete", as: "owner", key: "complete", ifMatch: `"1"`, body: struct{}{}}), 200)
	if c.State != lifecycle.ConnectionActive || c.Version != 2 || c.Account.Account != "octocat" {
		t.Fatalf("complete: %+v", c)
	}
	task := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "task", body: s.submitBody()}), 201)
	terms := httpapi.ConnectionGrantRequest{Task: task.Task.ID, Tools: []string{"read_issue"}, Permission: identity.PermissionAlways, CallLimit: 5, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	s.do(t, call{method: "POST", path: path + "/grants", as: "other", key: "grant", body: terms}).want(t, 403)
	g := decodeInto[lifecycle.ConnectionGrant](t, s.do(t, call{method: "POST", path: path + "/grants", as: "owner", key: "grant", body: terms}), 201)
	gp := "/connection-grants/" + g.ID.String()
	replayGrant := decodeInto[lifecycle.ConnectionGrant](t, s.do(t, call{method: "POST", path: path + "/grants", as: "owner", key: "grant", body: terms}), 201)
	if replayGrant.ID != g.ID {
		t.Fatal("grant replay duplicated")
	}
	decodeInto[httpapi.ConnectionGrantListBody](t, s.do(t, call{method: "GET", path: path + "/grants", as: "other"}), 200)
	s.do(t, call{method: "POST", path: gp + "/revoke", as: "other", key: "revoke", ifMatch: `"1"`, body: struct{}{}}).want(t, 403)
	peer.mu.Lock()
	peer.offline = true
	peer.mu.Unlock()
	c = decodeInto[lifecycle.Connection](t, s.do(t, call{method: "POST", path: path + "/remove", as: "owner", key: "remove", ifMatch: `"2"`, body: struct{}{}}), 202)
	if c.State != lifecycle.ConnectionRemoving {
		t.Fatalf("remove: %+v", c)
	}
	if got := decodeInto[lifecycle.ConnectionGrant](t, s.do(t, call{method: "GET", path: gp, as: "owner"}), 200); got.State != lifecycle.ConnectionGrantRevoked {
		t.Fatal("removal did not revoke grant")
	}
	peer.mu.Lock()
	peer.offline = false
	peer.mu.Unlock()
	c = decodeInto[lifecycle.Connection](t, s.do(t, call{method: "POST", path: path + "/remove", as: "owner", key: "remove", ifMatch: `"2"`, body: struct{}{}}), 200)
	if c.State != lifecycle.ConnectionRemoved {
		t.Fatalf("remove replay: %+v", c)
	}
	s.do(t, call{method: "POST", path: path + "/remove", as: "owner", key: "stale", ifMatch: `"2"`, body: struct{}{}}).want(t, 412)
}

func TestSurfaceConnectionsBeforeMigration(t *testing.T) {
	s := newSurface(t)
	db, err := sql.Open("pgx", s.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrator, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migrator.DownTo(s.ctx, 17); err != nil {
		t.Fatal(err)
	}
	if got := problemType(t, s.do(t, call{method: "POST", path: "/connections", as: "owner", key: "create", body: httpapi.ConnectionRequest{Provider: s.provider, Name: "Account"}}), 503); got != "not-migrated" {
		t.Fatalf("problem = %s", got)
	}
	listed := decodeInto[httpapi.ConnectionListBody](t, s.do(t, call{method: "GET", path: "/connections", as: "owner"}), 200)
	if len(listed.Connections) != 0 {
		t.Fatal("connections before migration")
	}
}

func (r reply) want(t *testing.T, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
}
