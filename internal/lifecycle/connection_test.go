package lifecycle

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
)

func newConnection(t *testing.T) Connection {
	t.Helper()
	c, err := NewConnection(newID[ids.Connection](t), newID[ids.Tenant](t), newID[ids.Provider](t), newID[ids.Principal](t), "Team GitHub", testNow)
	if err != nil {
		t.Fatalf("NewConnection() error = %v", err)
	}
	return c
}

func activeConnection(t *testing.T) Connection {
	t.Helper()
	c := newConnection(t)
	active, _, err := c.Complete(c.Owner, ConnectionAccount{Account: "octocat", Permissions: []string{"issues:write"}}, c.Version, testNow)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	return active
}

func newConnectionGrant(t *testing.T, c Connection) ConnectionGrant {
	t.Helper()
	g, err := NewConnectionGrant(newID[ids.ConnGrant](t), c, c.Owner, ConnectionGrantTerms{
		Agent: newID[ids.Agent](t), Tools: []string{"get_issue", "create_comment"},
		Permission: identity.PermissionAsk, CallLimit: 10, ExpiresAt: testNow.Add(time.Hour),
	}, testNow)
	if err != nil {
		t.Fatalf("NewConnectionGrant() error = %v", err)
	}
	return g
}

func TestConnectionFollowsConsentAndRemoval(t *testing.T) {
	c := newConnection(t)
	if c.State != ConnectionAuthorizing || c.Account != nil {
		t.Fatalf("new connection = %+v", c)
	}
	account := ConnectionAccount{Account: "octocat", Permissions: []string{"issues:write"}}
	if _, _, err := c.Complete(newID[ids.Principal](t), account, c.Version, testNow); !rejected(err) {
		t.Fatalf("Complete() by another principal error = %v, want rejection", err)
	}
	active, change, err := c.Complete(c.Owner, account, c.Version, testNow)
	if err != nil || active.State != ConnectionActive || active.Account == nil || active.Version != 2 || change.To != string(ConnectionActive) {
		t.Fatalf("Complete() = %+v, %+v, %v", active, change, err)
	}
	if err := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: c.Owner}, testNow).Validate(); err != nil {
		t.Fatalf("audit of completion: %v", err)
	}
	renewed, _, err := active.Complete(c.Owner, ConnectionAccount{Account: "octocat", Permissions: []string{"issues:read"}}, active.Version, testNow)
	if err != nil || renewed.State != ConnectionActive || renewed.Account.Permissions[0] != "issues:read" {
		t.Fatalf("renewed consent = %+v, %v", renewed, err)
	}
	if _, _, err := renewed.Complete(c.Owner, account, active.Version, testNow); !rejected(err) {
		t.Fatalf("Complete() on a stale version error = %v", err)
	}
	removing, _, err := renewed.Remove(c.Owner, renewed.Version, testNow)
	if err != nil || removing.State != ConnectionRemoving {
		t.Fatalf("Remove() = %+v, %v", removing, err)
	}
	if _, _, err := removing.Complete(c.Owner, account, removing.Version, testNow); !rejected(err) {
		t.Fatalf("Complete() while removing error = %v", err)
	}
	removed, _, err := removing.FinishRemoval(removing.Version, testNow)
	if err != nil || removed.State != ConnectionRemoved || removed.Account == nil {
		t.Fatalf("FinishRemoval() = %+v, %v", removed, err)
	}
	if _, _, err := removed.Remove(c.Owner, removed.Version, testNow); !rejected(err) {
		t.Fatalf("Remove() of a removed connection error = %v", err)
	}
}

func TestConnectionRejectsMalformedRecords(t *testing.T) {
	c := newConnection(t)
	for name, mutate := range map[string]func(*Connection){
		"blank name":        func(c *Connection) { c.Name = " " },
		"padded name":       func(c *Connection) { c.Name = "GitHub " },
		"long name":         func(c *Connection) { c.Name = strings.Repeat("n", MaxConnectionNameCharacters+1) },
		"no owner":          func(c *Connection) { c.Owner = ids.PrincipalID{} },
		"active no account": func(c *Connection) { c.State = ConnectionActive },
		"unknown state":     func(c *Connection) { c.State = "paused" },
		"blank account":     func(c *Connection) { c.Account = &ConnectionAccount{Account: " "} },
		"two line account":  func(c *Connection) { c.Account = &ConnectionAccount{Account: "a\nb"} },
		"too many permissions": func(c *Connection) {
			c.Account = &ConnectionAccount{Account: "a", Permissions: make([]string, MaxConnectionPermissions+1)}
		},
		"blank permission": func(c *Connection) { c.Account = &ConnectionAccount{Account: "a", Permissions: []string{""}} },
		"updated before":   func(c *Connection) { c.UpdatedAt = c.CreatedAt.Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestConnectionGrantCoversOnlyItsWork(t *testing.T) {
	c := activeConnection(t)
	g := newConnectionGrant(t, c)
	if g.Tools[0] != "create_comment" {
		t.Fatalf("tools are not sorted: %v", g.Tools)
	}
	task := newID[ids.Task](t)
	if !g.Covers(task, g.Agent, "get_issue", testNow) {
		t.Fatal("grant does not cover its agent's work")
	}
	for name, covered := range map[string]bool{
		"other agent":  g.Covers(task, newID[ids.Agent](t), "get_issue", testNow),
		"no agent":     g.Covers(task, ids.AgentID{}, "get_issue", testNow),
		"other tool":   g.Covers(task, g.Agent, "delete_repo", testNow),
		"at expiry":    g.Covers(task, g.Agent, "get_issue", g.ExpiresAt),
		"after expiry": g.Covers(task, g.Agent, "get_issue", g.ExpiresAt.Add(time.Second)),
	} {
		if covered {
			t.Errorf("%s: covered", name)
		}
	}
	taskGrant, err := NewConnectionGrant(newID[ids.ConnGrant](t), c, c.Owner, ConnectionGrantTerms{Task: task, Tools: []string{"get_issue"}, Permission: identity.PermissionAlways, CallLimit: 1, ExpiresAt: testNow.Add(time.Hour)}, testNow)
	if err != nil {
		t.Fatalf("NewConnectionGrant(task) error = %v", err)
	}
	if !taskGrant.Covers(task, newID[ids.Agent](t), "get_issue", testNow) || taskGrant.Covers(newID[ids.Task](t), ids.AgentID{}, "get_issue", testNow) {
		t.Fatal("task grant covers the wrong task")
	}
	if _, _, err := g.Revoke(newID[ids.Principal](t), g.Version, testNow); !rejected(err) {
		t.Fatalf("Revoke() by another principal error = %v", err)
	}
	revoked, change, err := g.Revoke(g.GrantedBy, g.Version, testNow)
	if err != nil || revoked.State != ConnectionGrantRevoked || revoked.Covers(task, g.Agent, "get_issue", testNow) {
		t.Fatalf("Revoke() = %+v, %v", revoked, err)
	}
	if err := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: g.GrantedBy}, testNow).Validate(); err != nil {
		t.Fatalf("audit of revocation: %v", err)
	}
	if _, _, err := revoked.Revoke(g.GrantedBy, revoked.Version, testNow); !rejected(err) {
		t.Fatalf("second Revoke() error = %v", err)
	}
}

func TestConnectionGrantIsIssuedOnlyByTheOwnerOfAnActiveConnection(t *testing.T) {
	pending := newConnection(t)
	terms := ConnectionGrantTerms{Agent: newID[ids.Agent](t), Tools: []string{"get_issue"}, Permission: identity.PermissionAsk, CallLimit: 1, ExpiresAt: testNow.Add(time.Hour)}
	if _, err := NewConnectionGrant(newID[ids.ConnGrant](t), pending, pending.Owner, terms, testNow); !rejected(err) {
		t.Fatalf("grant of an authorizing connection error = %v", err)
	}
	active := activeConnection(t)
	if _, err := NewConnectionGrant(newID[ids.ConnGrant](t), active, newID[ids.Principal](t), terms, testNow); !rejected(err) {
		t.Fatalf("grant by another principal error = %v", err)
	}
	task := newID[ids.Task](t)
	for name, mutate := range map[string]func(*ConnectionGrantTerms){
		"agent and task":  func(t *ConnectionGrantTerms) { t.Task = task },
		"neither":         func(t *ConnectionGrantTerms) { t.Agent = ids.AgentID{} },
		"no tools":        func(t *ConnectionGrantTerms) { t.Tools = nil },
		"duplicate tools": func(t *ConnectionGrantTerms) { t.Tools = []string{"get_issue", "get_issue"} },
		"bad tool":        func(t *ConnectionGrantTerms) { t.Tools = []string{"Get-Issue"} },
		"never":           func(t *ConnectionGrantTerms) { t.Permission = identity.PermissionNever },
		"zero limit":      func(t *ConnectionGrantTerms) { t.CallLimit = 0 },
		"limit too high":  func(t *ConnectionGrantTerms) { t.CallLimit = MaxGrantCallLimit + 1 },
		"already expired": func(t *ConnectionGrantTerms) { t.ExpiresAt = testNow },
		"too many tools":  func(t *ConnectionGrantTerms) { t.Tools = manyTools(MaxGrantTools + 1) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := terms
			mutate(&bad)
			if _, err := NewConnectionGrant(newID[ids.ConnGrant](t), active, active.Owner, bad, testNow); !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewConnectionGrant() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func manyTools(n int) []string {
	tools := make([]string, n)
	for i := range tools {
		tools[i] = "tool_" + strings.Repeat("a", i%40) + string(rune('a'+i%26))
	}
	return tools
}

func rejected(err error) bool {
	var transition *TransitionError
	return errors.As(err, &transition)
}
