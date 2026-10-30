//go:build integration

package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type governedFixture struct {
	*fixture
	s        application.Submission
	resource *simulator.Resource
	cmd      application.GovernedActionRequest
	decision application.DecisionView
}

func newGovernedFixture(t *testing.T, standing bool) *governedFixture {
	t.Helper()
	f := newFixture(t, lifecycle.HarnessLive)
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, u, store.PolicyUpdate{Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 1, Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, Ceiling: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, At: f.at})
		return err
	})
	s, rt := f.executing(t, "governed")
	resource := simulator.NewResource("screen")
	input := json.RawMessage(`{"resource":"screen","value":"safe"}`)
	f.raise(t, rt, s, 2, contracts.AttentionRequested{NativeRef: "action-1", Kind: lifecycle.AttentionActionApproval, Action: "resource.mutate", ActionDigest: identity.DigestOf(input), Scope: identity.Scope{Class: identity.ScopeSubject, Target: "screen"}, ExpiresAt: f.at.Add(time.Hour)})
	request := f.attention(t, s)
	kind := lifecycle.DecisionApproveOnce
	if standing {
		kind = lifecycle.DecisionApproveStanding
	}
	dc := f.decideCommand("approve", request, kind)
	dc.At = f.at
	d := f.decide(t, dc)
	grant, err := ids.Parse[ids.Grant](d.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	f.deliver(t, s)
	f.ingestNow(t, f.event(rt, s, 1, simulator.RowsFrame(false, row(3, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "action-1"}))))
	return &governedFixture{fixture: f, s: s, resource: resource, decision: d, cmd: application.GovernedActionRequest{Request: identity.ActionRequest{Tenant: f.tenant, Attempt: s.Attempt.ID, Provider: f.provider, Operation: request.Operation, Action: "resource.mutate", Scope: request.Scope, InputDigest: identity.DigestOf(input)}, Grant: grant, Input: input, Deadline: f.at.Add(time.Hour), Precondition: &contracts.ActionPrecondition{Operation: request.Operation, Resource: "screen", Token: resource.Token()}}}
}

func (g *governedFixture) enforcement(t *testing.T, response contracts.HookResponse, executeErr error, commands ...application.GovernedActionRequest) *simulator.Enforcement {
	t.Helper()
	ops := map[ids.OperationID]simulator.EnforcementOp{}
	if len(commands) == 0 {
		commands = []application.GovernedActionRequest{g.cmd}
	}
	for _, cmd := range commands {
		ops[cmd.Request.Operation] = simulator.EnforcementOp{Response: response, Resource: g.resource, ExecuteError: executeErr}
	}
	e, err := simulator.NewEnforcement(simulator.Config{Tenant: g.tenant, Provider: g.provider}, simulator.EnforcementScenario{Operations: ops})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (g *governedFixture) enforcementWithHooks(t *testing.T, response contracts.HookResponse, hooks map[simulator.HookStageKey]simulator.EnforcementOp) *simulator.Enforcement {
	t.Helper()
	ops := map[ids.OperationID]simulator.EnforcementOp{g.cmd.Request.Operation: {Response: response, Resource: g.resource}}
	e, err := simulator.NewEnforcement(simulator.Config{Tenant: g.tenant, Provider: g.provider}, simulator.EnforcementScenario{Operations: ops, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (g *governedFixture) enforce(t *testing.T, p contracts.Port, cmd application.GovernedActionRequest) application.GovernedActionView {
	t.Helper()
	v, err := application.EnforceGovernedAction(g.ctx, g.pool, g.peer(), p, cmd, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (g *governedFixture) next(t *testing.T) application.GovernedActionRequest {
	t.Helper()
	cmd := g.cmd
	cmd.Request.Operation = mustID[ids.Operation](t)
	p := *cmd.Precondition
	p.Operation = cmd.Request.Operation
	p.Token = g.resource.Token()
	cmd.Precondition = &p
	return cmd
}

func TestGovernedActionOnceReceiptAndReplay(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	v := g.enforce(t, e, g.cmd)
	if v.Use.Outcome != identity.UseAllowed || v.Use.Source != identity.SourceOneTimeApproval {
		t.Fatalf("use=%+v", v.Use)
	}
	g.tick(t, e)
	view, err := application.GetGovernedAction(g.ctx, g.pool, g.actor, g.cmd.Request.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if view.Receipt == nil || view.Receipt.Execution.Decision.String() != g.decision.DecisionID || view.Receipt.Execution.Request.Scope != g.cmd.Request.Scope {
		t.Fatalf("receipt=%+v", view.Receipt)
	}
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	if n := g.resource.Ledger().Effects(g.cmd.Request.Operation); n != 1 {
		t.Fatalf("effects=%d", n)
	}
	if n := g.count(t, "enforcement_receipt", "true"); n != 1 {
		t.Fatalf("receipts=%d", n)
	}
	grant, err := store.GetGrant(g.ctx, g.pool, g.tenant, g.cmd.Grant)
	if err != nil || grant.RetiredAt.IsZero() {
		t.Fatalf("retirement=%+v, %v", grant, err)
	}
	changed := g.cmd
	changed.Input = []byte(`{"resource":"screen","value":"changed"}`)
	changed.Request.InputDigest = identity.DigestOf(changed.Input)
	if _, err := application.EnforceGovernedAction(g.ctx, g.pool, g.peer(), e, changed, time.Now()); !errors.Is(err, application.ErrPayloadMismatch) {
		t.Fatalf("changed replay=%v", err)
	}
	// The runtime parent remains the cancellation target after action dispatch.
	current := g.reload(t, "governed")
	cancel := g.cancelCommand("cancel", current.Task.ID, current.Task.Version)
	if _, err := application.Cancel(g.ctx, g.pool, cancel); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := g.pool.QueryRow(g.ctx, `select payload from outbox where tenant_id=$1 and event='request_cancel'`, g.tenant.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	body, err := worker.ParseDispatchBody(payload)
	if err != nil || body.Operation != g.s.Operation.ID {
		t.Fatalf("cancel operation=%s %v", body.Operation, err)
	}
}

func TestGovernedActionPostHookIsDurableAndSafe(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	response := contracts.HookResponse{Kind: contracts.HookPostAction, UserMessage: "private-user-marker", AgentMessage: "private-agent-marker", AdditionalContext: []string{"private-context-marker"}, Usage: []contracts.UsageMeter{{Unit: "tokens", Amount: 7}}}
	preResponse := simulator.AllowHook()
	preResponse.UserMessage = "private-pre-user-marker"
	preResponse.AdditionalContext = []string{"private-pre-context-marker"}
	preResponse.Usage = []contracts.UsageMeter{{Unit: "tokens", Amount: 3}}
	e := g.enforcementWithHooks(t, preResponse, map[simulator.HookStageKey]simulator.EnforcementOp{{Operation: g.cmd.Request.Operation, Kind: contracts.HookPostAction}: {Response: response}})
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	if op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation); err != nil || op.State != lifecycle.OperationSucceeded {
		t.Fatalf("operation=%+v, %v", op, err)
	}
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post evidence before hook delivery=%+v, %v", err, err)
	}
	g.tick(t, e)
	evidence, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction)
	if err != nil || evidence.OutputDigest == "" || evidence.ResponseDigest == "" || len(evidence.Usage) != 1 {
		t.Fatalf("post evidence=%+v, %v", evidence, err)
	}
	var stored string
	if err := g.pool.QueryRow(g.ctx, `select row_to_json(h)::text from hook_stage_evidence h where tenant_id=$1 and operation_id=$2`, g.tenant.String(), g.cmd.Request.Operation.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-pre-user-marker", "private-pre-context-marker", "private-user-marker", "private-agent-marker", "private-context-marker", string(g.cmd.Input)} {
		if strings.Contains(stored, secret) {
			t.Fatalf("durable hook evidence contains raw value %q", secret)
		}
	}
	if got := e.Ledger().Effects(g.cmd.Request.Operation); got != 3 {
		t.Fatalf("external effects=%d, want one action and one post hook", got)
	}
}

func TestGovernedActionLostPostHookReplyRecoversWithoutRepeatingAction(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcementWithHooks(t, simulator.AllowHook(), map[simulator.HookStageKey]simulator.EnforcementOp{{Operation: g.cmd.Request.Operation, Kind: contracts.HookPostAction}: {Response: contracts.HookResponse{Kind: contracts.HookPostAction, AgentMessage: "recorded"}, SendError: errors.New("reply lost")}})
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	g.tick(t, e)
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evidence before recovery=%v", err)
	}
	if _, err := g.pool.Exec(g.ctx, `update outbox set next_attempt_at=$2 where tenant_id=$1 and payload->>'method'='post_action'`, g.tenant.String(), time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	g.tick(t, e)
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); err != nil {
		t.Fatalf("recovered evidence: %v", err)
	}
	if got := e.Ledger().Effects(g.cmd.Request.Operation); got != 3 {
		t.Fatalf("external effects=%d, want pre hook, action and one post hook", got)
	}
	if op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation); err != nil || op.State != lifecycle.OperationSucceeded {
		t.Fatalf("operation=%+v, %v", op, err)
	}
}

func TestGovernedActionMalformedPostHookReplyDoesNotAdmitEvidence(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcementWithHooks(t, simulator.AllowHook(), map[simulator.HookStageKey]simulator.EnforcementOp{{Operation: g.cmd.Request.Operation, Kind: contracts.HookPostAction}: {Response: contracts.HookResponse{Kind: contracts.HookPostAction}}})
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	malformed := hookPort{Port: e, send: func(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
		reply, err := e.Send(ctx, request)
		if request.Method == contracts.MethodPostAction && err == nil {
			reply.Result = json.RawMessage(`{"kind":"post_action","raw_secret":"must not be stored"}`)
		}
		return reply, err
	}}
	g.tick(t, malformed)
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("malformed response evidence=%v", err)
	}
	if op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation); err != nil || op.State != lifecycle.OperationSucceeded {
		t.Fatalf("operation=%+v, %v", op, err)
	}
}

func TestGovernedActionStaleHookClaimCannotCommitEvidence(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcementWithHooks(t, simulator.AllowHook(), map[simulator.HookStageKey]simulator.EnforcementOp{{Operation: g.cmd.Request.Operation, Kind: contracts.HookPostAction}: {Response: contracts.HookResponse{Kind: contracts.HookPostAction}}})
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	stale := hookPort{Port: e, send: func(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
		reply, err := e.Send(ctx, request)
		if request.Method == contracts.MethodPostAction && err == nil {
			_, updateErr := g.pool.Exec(g.ctx, `update outbox set lease_epoch=lease_epoch+1,lease_expires_at=clock_timestamp()+interval '1 hour' where tenant_id=$1 and payload->>'method'='post_action'`, g.tenant.String())
			if updateErr != nil {
				return contracts.Reply{}, updateErr
			}
		}
		return reply, err
	}}
	g.tick(t, stale)
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale claimant committed evidence=%v", err)
	}
	if _, err := g.pool.Exec(g.ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and payload->>'method'='post_action'`, g.tenant.String()); err != nil {
		t.Fatal(err)
	}
	g.tick(t, e)
	if _, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); err != nil {
		t.Fatalf("takeover evidence=%v", err)
	}
	if got := e.Ledger().Effects(g.cmd.Request.Operation); got != 3 {
		t.Fatalf("effects=%d, want one each for pre hook, action and post hook", got)
	}
}

func TestGovernedActionFailureHookUsesTypedFailureStage(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	cmd := g.cmd
	pre := *cmd.Precondition
	pre.Token = identity.DigestOf([]byte("stale resource"))
	cmd.Precondition = &pre
	response := contracts.HookResponse{Kind: contracts.HookActionFailure, AgentMessage: "failure noted"}
	e := g.enforcementWithHooks(t, simulator.AllowHook(), map[simulator.HookStageKey]simulator.EnforcementOp{{Operation: cmd.Request.Operation, Kind: contracts.HookActionFailure}: {Response: response}})
	g.enforce(t, e, cmd)
	g.tick(t, e)
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationRejected {
		t.Fatalf("operation=%+v, %v", op, err)
	}
	g.tick(t, e)
	evidence, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, cmd.Request.Operation, contracts.HookActionFailure)
	if err != nil || evidence.FailureCode != contracts.FailureStalePrecondition || evidence.OutputDigest != "" {
		t.Fatalf("failure evidence=%+v, %v", evidence, err)
	}
	if e.Ledger().Effects(cmd.Request.Operation) != 2 {
		t.Fatal("rejected action was effected or failure hook missing")
	}
}

func TestGovernedActionHookDenialNoExecutableOperation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"deny", "missing", "transport", "malformed", "timeout", "rewrite_target"} {
		t.Run(mode, func(t *testing.T) {
			g := newGovernedFixture(t, false)
			e := g.enforcement(t, simulator.DenyHook(), nil)
			var p contracts.Port = e
			switch mode {
			case "missing":
				p = nil
			case "transport":
				p = hookPort{Port: e, send: func(context.Context, contracts.Request) (contracts.Reply, error) {
					return contracts.Reply{}, errors.New("unavailable")
				}}
			case "malformed":
				p = hookPort{Port: e, send: func(_ context.Context, r contracts.Request) (contracts.Reply, error) {
					return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: json.RawMessage(`{"kind":"pre_action","permission":"allow","unexpected":true}`)}, nil
				}}
			case "timeout":
				g.cmd.Deadline = time.Now().Add(-time.Second)
			case "rewrite_target":
				p = g.enforcement(t, contracts.HookResponse{Kind: contracts.HookPreAction, Permission: contracts.PermissionAllow, UpdatedInput: json.RawMessage(`{"resource":"other","value":1}`)}, nil)
			}
			v := g.enforce(t, p, g.cmd)
			if v.Use.Outcome != identity.UseDenied || v.Use.Reason != identity.DenialHookRejected {
				t.Fatalf("denial=%+v hook=%+v", v.Use, v.Hook)
			}
			if n := g.count(t, "provider_operation", "operation_id=$2", g.cmd.Request.Operation.String()); n != 0 {
				t.Fatalf("denied op=%d", n)
			}
			if n := g.count(t, "outbox", "record_id=$2", g.cmd.Request.Operation.String()); n != 0 {
				t.Fatalf("denied outbox=%d", n)
			}
			if n := g.resource.Ledger().Effects(g.cmd.Request.Operation); n != 0 {
				t.Fatalf("denied effects=%d", n)
			}
		})
	}
}

type hookPort struct {
	contracts.Port
	send func(context.Context, contracts.Request) (contracts.Reply, error)
}

func (p hookPort) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	return p.send(ctx, r)
}

func TestGovernedActionStandingRewriteRevokeAndStaleToken(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, true)
	response := simulator.AllowHook()
	response.UpdatedInput = json.RawMessage(`{"resource":"screen","value":"rewritten"}`)
	second := g.next(t)
	e := g.enforcement(t, response, nil, g.cmd, second)
	v := g.enforce(t, e, g.cmd)
	if v.Use.InputDigest != identity.DigestOf(response.UpdatedInput) || !v.Hook.Rewritten {
		t.Fatalf("rewrite=%+v", v)
	}
	g.tick(t, e)
	// A token observed before the first action can no longer authorize state.
	v = g.enforce(t, e, second)
	if v.Use.Outcome != identity.UseAllowed {
		t.Fatalf("second=%+v", v)
	}
	g.tick(t, e)
	if n := g.resource.Ledger().Effects(second.Request.Operation); n != 0 {
		t.Fatalf("stale effects=%d", n)
	}
	if _, attempt := g.states(t, g.s); attempt != lifecycle.AttemptExecuting {
		t.Fatalf("action rejection failed attempt: %s", attempt)
	}
	third := g.next(t)
	e = g.enforcement(t, simulator.AllowHook(), nil, third)
	g.enforce(t, e, third)
	g.tick(t, e)
	if g.resource.Ledger().Effects(third.Request.Operation) != 1 {
		t.Fatal("fresh observation did not execute")
	}
	if err := application.Revoke(g.ctx, g.pool, application.RevokeAuthority{Command: application.Command{Actor: g.actor, Nonce: "revoke"}, Grant: g.cmd.Grant, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fourth := g.next(t)
	e = g.enforcement(t, simulator.AllowHook(), nil, fourth)
	v = g.enforce(t, e, fourth)
	if v.Use.Reason != identity.DenialRevoked {
		t.Fatalf("revoked=%+v", v.Use)
	}
}

func TestGovernedActionAuthorityDenials(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"no_grant", "wrong_operation", "input_mismatch", "stale_generation", "above_ceiling", "expired"} {
		t.Run(kind, func(t *testing.T) {
			standing := kind == "stale_generation" || kind == "above_ceiling"
			g := newGovernedFixture(t, standing)
			want := identity.DenialNoGrant
			switch kind {
			case "no_grant":
				g.cmd.Grant = ids.GrantID{}
			case "wrong_operation":
				g.cmd = g.next(t)
				want = identity.DenialWrongOperation
			case "input_mismatch":
				g.cmd.Input = []byte(`{"resource":"screen","value":"changed"}`)
				g.cmd.Request.InputDigest = identity.DigestOf(g.cmd.Input)
				want = identity.DenialInputMismatch
			case "stale_generation", "above_ceiling":
				generation := uint64(2)
				permission := identity.PermissionAlways
				want = identity.DenialStaleGeneration
				if kind == "above_ceiling" {
					generation = 1
					permission = identity.PermissionAsk
					want = identity.DenialAboveCeiling
				}
				g.seed(t, func(u *store.UnitOfWork) error {
					_, err := store.UpdatePolicyGeneration(g.ctx, u, store.PolicyUpdate{Tenant: g.tenant, ExpectedGeneration: 1, NextGeneration: generation, Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, Ceiling: map[identity.ActionClass]identity.Permission{"resource.mutate": permission}, At: time.Now()})
					return err
				})
			case "expired":
				// A short-lived approval expires after issuance, before interception.
				if _, err := g.pool.Exec(g.ctx, `update authority_grant set expires_at=issued_at+interval '1 microsecond' where tenant_id=$1 and grant_id=$2`, g.tenant.String(), g.cmd.Grant.String()); err != nil {
					t.Fatal(err)
				}
				want = identity.DenialExpired
			}
			e := g.enforcement(t, simulator.AllowHook(), nil)
			v := g.enforce(t, e, g.cmd)
			if v.Use.Reason != want {
				t.Fatalf("denial=%+v want %s", v.Use, want)
			}
			// Drain the real dispatch and reconciliation paths: interception
			// denial must not leave executable work for a later worker tick.
			if err := recoveryWorker(t, g.fixture, e).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if err := recoveryWorker(t, g.fixture, e).Reconcile(g.ctx); err != nil {
				t.Fatal(err)
			}
			if g.count(t, "outbox", "record_id=$2", g.cmd.Request.Operation.String()) != 0 || g.count(t, "provider_operation", "operation_id=$2", g.cmd.Request.Operation.String()) != 0 {
				t.Fatal("denial left executable operation")
			}
			if g.resource.Ledger().Effects(g.cmd.Request.Operation) != 0 {
				t.Fatal("denial executed")
			}
		})
	}
}

func TestGovernedActionConcurrentReplay(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := application.EnforceGovernedAction(g.ctx, g.pool, g.peer(), e, g.cmd, time.Now())
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	g.tick(t, e)
	if n := g.resource.Ledger().Effects(g.cmd.Request.Operation); n != 1 {
		t.Fatalf("effects=%d", n)
	}
	if n := g.count(t, "authority_use", "true"); n != 1 {
		t.Fatalf("uses=%d", n)
	}
}

func TestGovernedActionLostReplyNeverRepeatsEffect(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), errors.New("reply lost"))
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	g.tick(t, e)
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationUncertain {
		t.Fatalf("operation=%+v, %v", op, err)
	}
	if g.resource.Ledger().Effects(g.cmd.Request.Operation) != 1 {
		t.Fatal("effect repeated or missing")
	}
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "reconciler",
		wiretest.Ports(t, g.pool, g.tenant, nil, wiretest.Enforcement(map[ids.ProviderID]contracts.Port{g.provider: e})))
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Reconcile(g.ctx); err != nil {
		t.Fatal(err)
	}
	v, err := application.GetGovernedAction(g.ctx, g.pool, g.actor, g.cmd.Request.Operation)
	if err != nil || v.Receipt == nil {
		t.Fatalf("reconciled receipt=%+v, %v", v, err)
	}
}

func TestGovernedActionRechecksQueuedAuthority(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"revoked", "attempt_failed"} {
		t.Run(kind, func(t *testing.T) {
			g := newGovernedFixture(t, true)
			e := g.enforcement(t, simulator.AllowHook(), nil)
			g.enforce(t, e, g.cmd)
			if kind == "revoked" {
				if err := application.Revoke(g.ctx, g.pool, application.RevokeAuthority{Command: application.Command{Actor: g.actor, Nonce: "revoke"}, Grant: g.cmd.Grant, At: time.Now()}); err != nil {
					t.Fatal(err)
				}
			} else {
				rt := g.runtime(t, g.s.Operation.ID)
				// Failure is an authenticated runtime observation, not a direct store mutation.
				g.ingestNow(t, g.event(rt, g.s, 1, simulator.RowsFrame(false, row(4, contracts.EntryTurnStarted, contracts.TurnStarted{NativeRef: "turn", SessionRef: "s-1"}))))
				g.ingestNow(t, g.event(rt, g.s, 1, simulator.RowsFrame(false, row(5, contracts.EntryTurnFinished, contracts.TurnFinished{TurnRef: "turn", SessionRef: "s-1", Outcome: lifecycle.TurnFailed, Failure: &lifecycle.TurnFailure{Code: lifecycle.TurnFailureToolFailed, Summary: "failed", DetailsDigest: identity.DigestOf([]byte("failure"))}, EndedAt: time.Now()}))))
			}
			g.tick(t, e)
			if g.resource.Ledger().Effects(g.cmd.Request.Operation) != 0 {
				t.Fatal("queued authority executed after revocation/termination")
			}
		})
	}
}

func TestGovernedActionSerialResourceAndEffectBoundary(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"busy", "state_changed"} {
		t.Run(kind, func(t *testing.T) {
			g := newGovernedFixture(t, true)
			second := g.next(t)
			e := g.enforcement(t, simulator.AllowHook(), nil, g.cmd, second)
			g.enforce(t, e, g.cmd)
			g.enforce(t, e, second)
			pause := e.Control().Pause(g.cmd.Request.Operation, simulator.BeforeEffect)
			w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "first",
				wiretest.Ports(t, g.pool, g.tenant, map[ids.ProviderID]contracts.Port{g.provider: e}))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- w.Tick(g.ctx) }()
			select {
			case <-pause.Reached():
			case <-g.ctx.Done():
				t.Fatal("pause not reached")
			}
			if kind == "state_changed" {
				g.resource.Change()
			} else {
				// The resource itself rejects a parallel physical action even when
				// reasoning came from another Attempt with a separate worker lease.
				ticket, failure := g.resource.Begin(*second.Precondition)
				if ticket != nil || failure == nil || failure.Code != contracts.FailureResourceBusy {
					t.Fatalf("parallel=%+v %+v", ticket, failure)
				}
			}
			pause.Release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			want := 1
			if kind == "state_changed" {
				want = 0
			}
			if n := g.resource.Ledger().Effects(g.cmd.Request.Operation); n != want {
				t.Fatalf("effect=%d want %d", n, want)
			}
		})
	}
}

func TestGovernedActionRejectsRuntimeOperationCollision(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	cmd := g.cmd
	cmd.Request.Operation = g.s.Operation.ID
	pre := *cmd.Precondition
	pre.Operation = cmd.Request.Operation
	cmd.Precondition = &pre
	cmd.Grant = ids.GrantID{}
	e := g.enforcement(t, simulator.DenyHook(), nil, cmd)
	if _, err := application.EnforceGovernedAction(g.ctx, g.pool, g.peer(), e, cmd, time.Now()); !errors.Is(err, application.ErrPayloadMismatch) {
		t.Fatalf("collision=%v", err)
	}
	if n := g.count(t, "governed_action", "operation_id=$2", g.s.Operation.ID.String()); n != 0 {
		t.Fatal("runtime was reclassified as a denied action")
	}
}

func TestGovernedActionCrashMarkerNeverResends(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	pause := e.Control().Pause(g.cmd.Request.Operation, simulator.AfterEffect)
	cfg := worker.DefaultConfig()
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, cfg, "crashing-worker",
		wiretest.Ports(t, g.pool, g.tenant, map[ids.ProviderID]contracts.Port{g.provider: e}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Tick(ctx) }()
	select {
	case <-pause.Reached():
	case err := <-done:
		t.Fatalf("dispatch ended before the crash boundary: %v", err)
	case <-g.ctx.Done():
		t.Fatal("effect boundary not reached")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed dispatch error=%v, want context cancellation", err)
	}
	// Crash only after the effect, then expire its claims explicitly. A tiny
	// real-time lease can lapse during preparation on a slower CI runner and
	// correctly yield before sending, which does not simulate this crash.
	g.seed(t, func(u *store.UnitOfWork) error {
		tag, err := u.Exec(g.ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and record_id=$2 and status='processing'`, g.tenant.String(), g.cmd.Request.Operation.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("crash did not retain the claimed action outbox row")
		}
		tag, err = u.Exec(g.ctx, `update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and attempt_id=$2 and lease_owner='crashing-worker'`, g.tenant.String(), g.s.Attempt.ID.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("crash did not retain the attempt lease")
		}
		return nil
	})
	// A replacement may reclaim, but must reconcile the durable send marker.
	g.tick(t, e)
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationUncertain {
		t.Fatalf("crash state=%+v %v", op, err)
	}
	if n := g.resource.Ledger().Effects(g.cmd.Request.Operation); n != 1 {
		t.Fatalf("crash effects=%d", n)
	}
	if n := e.Ledger().Deliveries(g.cmd.Request.Operation); n != 2 {
		t.Fatalf("deliveries=%d want hook plus one execution", n)
	}
}

func TestGovernedActionOneTimeGrantRacesDifferentOperations(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	other := g.next(t)
	e := g.enforcement(t, simulator.AllowHook(), nil, g.cmd, other)
	var wg sync.WaitGroup
	results := make(chan application.GovernedActionView, 2)
	for _, cmd := range []application.GovernedActionRequest{g.cmd, other} {
		wg.Go(func() {
			v, err := application.EnforceGovernedAction(g.ctx, g.pool, g.peer(), e, cmd, time.Now())
			if err != nil {
				t.Error(err)
			}
			results <- v
		})
	}
	wg.Wait()
	close(results)
	allowed := 0
	for v := range results {
		if v.Use.Outcome == identity.UseAllowed {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("allowances=%d", allowed)
	}
	g.tick(t, e)
	if len(g.resource.Ledger().Applied()) != 1 {
		t.Fatal("racing operations did not produce exactly one effect")
	}
}

func TestGovernedActionCannotReportRuntimeLifecycle(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	event := contracts.Event{Tenant: g.tenant, Attempt: g.s.Attempt.ID, Provider: g.provider, Operation: g.cmd.Request.Operation, Stream: "runtime", Generation: 1, Body: simulator.RowsFrame(false, row(1, contracts.EntryRuntimeStopped, contracts.RuntimeStopped{HadActiveRun: true}))}
	if _, err := application.Ingest(g.ctx, g.pool, g.peer(), event, time.Now()); err == nil {
		t.Fatal("governed operation admitted as runtime lifecycle")
	}
	if _, attempt := g.states(t, g.s); attempt != lifecycle.AttemptExecuting {
		t.Fatalf("attempt=%s", attempt)
	}
}

func TestGovernedActionScopedNeverBlocksIssuance(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, u, store.PolicyUpdate{Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 1, Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, Ceiling: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, Denies: []identity.DenyPolicy{{Action: "resource.mutate", Target: "screen"}}, At: f.at})
		return err
	})
	s, rt := f.executing(t, "never")
	f.raise(t, rt, s, 2, contracts.AttentionRequested{NativeRef: "never-1", Kind: lifecycle.AttentionActionApproval, Action: "resource.mutate", ActionDigest: identity.DigestOf([]byte(`{"resource":"screen","value":1}`)), Scope: identity.Scope{Class: identity.ScopeSubject, Target: "screen"}, ExpiresAt: f.at.Add(time.Hour)})
	request := f.attention(t, s)
	if _, err := application.Decide(f.ctx, f.pool, f.decideCommand("never", request, lifecycle.DecisionApproveOnce)); !errors.Is(err, identity.DenialByPolicy) {
		t.Fatalf("never approval=%v", err)
	}
	if n := f.count(t, "authority_grant", "true"); n != 0 {
		t.Fatalf("forbidden grants=%d", n)
	}
	if n := f.count(t, "decision", "true"); n != 0 {
		t.Fatalf("partial decision=%d", n)
	}
}

func TestGovernedActionDigestsExactInputBytes(t *testing.T) {
	t.Parallel()
	g := newGovernedFixture(t, true)
	g.cmd.Input = json.RawMessage("{\n  \"resource\": \"screen\", \"value\": 1\n}")
	g.cmd.Request.InputDigest = identity.DigestOf(g.cmd.Input)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	v := g.enforce(t, e, g.cmd)
	if v.Use.Outcome != identity.UseAllowed || v.Use.InputDigest != g.cmd.Request.InputDigest {
		t.Fatalf("exact bytes=%+v", v.Use)
	}
	g.tick(t, e)
	if g.resource.Ledger().Effects(g.cmd.Request.Operation) != 1 {
		t.Fatal("exact-byte input failed to execute")
	}
}
