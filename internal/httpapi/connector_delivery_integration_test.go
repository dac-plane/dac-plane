//go:build integration

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dac-plane/dac-plane/internal/connectorgateway"
	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/dac-plane/dac-plane/internal/simulator"
	"github.com/dac-plane/dac-plane/internal/testsupport/wiretest"
	"github.com/dac-plane/dac-plane/internal/worker"
)

func TestConnectorGatewayDeliveryRequiresNegotiation(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{true: "capable", false: "old adapter"}[capable], func(t *testing.T) {
			s := newSurface(t)
			submission := decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: "owner", key: "task", body: s.submitBody()}), 201)
			provider := mustID[ids.Provider](t)
			s.seed(t, func(u *store.UnitOfWork) error {
				if err := store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: provider, Role: contracts.RoleConnector, UpdatedAt: s.at}); err != nil {
					return err
				}
				c, err := lifecycle.NewConnection(mustID[ids.Connection](t), s.tenant, provider, s.owner, "Fixture", s.at)
				if err != nil {
					return err
				}
				if err := store.InsertConnection(s.ctx, u, c); err != nil {
					return err
				}
				c, err = store.CompleteConnection(s.ctx, u, s.tenant, c.ID, lifecycle.ConnectionAccount{Account: "fixture", Permissions: []string{}}, store.Transition{ExpectedVersion: 1, At: time.Now().UTC(), Actor: lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: s.owner}, AuditID: mustID[ids.Audit](t)})
				if err != nil {
					return err
				}
				g, err := lifecycle.NewConnectionGrant(mustID[ids.ConnGrant](t), c, s.owner, lifecycle.ConnectionGrantTerms{Task: submission.Task.ID, Tools: []string{"get_issue"}, Permission: identity.PermissionAlways, CallLimit: 5, ExpiresAt: time.Now().UTC().Add(time.Hour)}, time.Now().UTC())
				if err != nil {
					return err
				}
				return store.InsertConnectionGrant(s.ctx, u, g)
			})
			s.dispatchPendingEnvironments(t)
			hello := simulator.DefaultHello()
			if capable {
				hello.Offered = []contracts.Capability{contracts.CapabilityConnectorGateway}
			}
			runtime, err := simulator.NewRuntime(simulator.Config{Tenant: s.tenant, Provider: s.provider, Hello: hello}, simulator.RuntimeScenario{Operations: map[ids.OperationID]simulator.RuntimeOp{submission.Operation.ID: {Reply: simulator.Accepted(nil), Effect: simulator.TurnStarted}}})
			if err != nil {
				t.Fatal(err)
			}
			wire := wiretest.For(t, s.pool)
			wire.Serve(t, s.tenant, s.provider, runtime)
			tokens, err := connectorgateway.NewTokens(bytes.Repeat([]byte("k"), 32))
			if err != nil {
				t.Fatal(err)
			}
			w, err := worker.New(s.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "gateway-delivery", worker.WithPorts(wire.Registry), worker.WithConnectorGateway(func(tenant ids.TenantID, attempt ids.AttemptID) *contracts.GatewayAccess {
				return tokens.Access("https://dac.example/connectors/mcp", tenant, attempt)
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Tick(s.ctx); err != nil {
				t.Fatal(err)
			}
			var start contracts.RuntimeStart
			for _, r := range runtime.Ledger().Sent() {
				if r.Operation == submission.Operation.ID {
					if err := json.Unmarshal(r.Arguments, &start); err != nil {
						t.Fatal(err)
					}
				}
			}
			if (start.Gateway != nil) != capable {
				t.Fatalf("gateway = %+v capable=%v", start.Gateway, capable)
			}
			if capable {
				token := tokens.Issue(s.tenant, submission.Attempt.ID)
				if start.Gateway.Token != token {
					t.Fatal("wrong gateway subject")
				}
				var payload string
				if err := s.pool.QueryRow(s.ctx, `select payload::text from outbox where tenant_id=$1 and outbox_id=$2`, s.tenant.String(), submission.Operation.ID.String()).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(payload, token) {
					t.Fatal("gateway token persisted in product outbox")
				}
			}
		})
	}
}
