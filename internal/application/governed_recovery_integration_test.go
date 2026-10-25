//go:build integration

package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/wiretest"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

func TestGovernedActionExpiresDuringSlowBatch(t *testing.T) {
	g := newGovernedFixture(t, true)
	second := g.next(t)
	e := g.enforcement(t, simulator.AllowHook(), nil, g.cmd, second)
	g.enforce(t, e, g.cmd)
	g.enforce(t, e, second)
	expires := time.Now().Add(300 * time.Millisecond)
	if _, err := g.pool.Exec(g.ctx, `update authority_grant set expires_at=$3 where tenant_id=$1 and grant_id=$2`, g.tenant.String(), g.cmd.Grant.String(), expires); err != nil {
		t.Fatal(err)
	}
	p := hookPort{Port: e, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
		if r.Method == contracts.MethodExecuteAction && r.Operation == g.cmd.Request.Operation {
			select {
			case <-time.After(time.Until(expires) + 20*time.Millisecond):
			case <-ctx.Done():
				return contracts.Reply{}, ctx.Err()
			}
			// Reject without an effect so the next action's observation stays fresh.
			return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeFailed, Durability: lifecycle.SendRejected, Failure: &contracts.Failure{Code: contracts.FailureProviderFailed, Message: "scripted no-effect rejection"}}, nil
		}
		return e.Send(ctx, r)
	}}
	g.tick(t, p)
	assertExpiredGovernedAction(t, g, second.Request.Operation)
}

func TestGovernedActionExpiresWhileWaitingForDispatchLock(t *testing.T) {
	for _, table := range []string{"policy", "provider_operation"} {
		t.Run(table, func(t *testing.T) {
			testGovernedActionExpiresWhileLocked(t, table)
		})
	}
}

func testGovernedActionExpiresWhileLocked(t *testing.T, table string) {
	g := newGovernedFixture(t, true)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	expires := time.Now().Add(300 * time.Millisecond)
	if _, err := g.pool.Exec(g.ctx, `update authority_grant set expires_at=$3 where tenant_id=$1 and grant_id=$2`, g.tenant.String(), g.cmd.Grant.String(), expires); err != nil {
		t.Fatal(err)
	}
	u, err := store.Begin(g.ctx, g.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(g.ctx) }()
	if table == "policy" {
		_, err = store.GetPolicyForUpdate(g.ctx, u, g.tenant)
	} else {
		_, err = u.Exec(g.ctx, `select operation_id from provider_operation where tenant_id=$1 and operation_id=$2 for update`, g.tenant.String(), g.cmd.Request.Operation.String())
	}
	if err != nil {
		t.Fatal(err)
	}
	w := governedRecoveryWorker(t, g, e)
	done := make(chan error, 1)
	go func() { done <- w.Tick(g.ctx) }()
	// Observe the actual blocked query before crossing expiry; scheduling
	// the goroutine alone does not prove it used a pre-expiry timestamp.
	waitCtx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	for {
		var blocked bool
		if err := g.pool.QueryRow(waitCtx, `select exists(select 1 from pg_stat_activity where wait_event_type='Lock' and query like $1)`, "%from "+table+"%for update%").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-waitCtx.Done():
			t.Fatal("dispatch did not block on the row lock")
		}
	}
	select {
	case <-time.After(time.Until(expires) + 20*time.Millisecond):
	case <-g.ctx.Done():
		t.Fatal(g.ctx.Err())
	}
	if err := u.Commit(g.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertExpiredGovernedAction(t, g, g.cmd.Request.Operation)
}

func assertExpiredGovernedAction(t *testing.T, g *governedFixture, operation ids.OperationID) {
	t.Helper()
	if got := g.resource.Ledger().Effects(operation); got != 0 {
		t.Fatalf("expired authority produced %d effects", got)
	}
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, operation)
	if err != nil || op.State != lifecycle.OperationRejected {
		t.Fatalf("expired operation=%+v, %v", op, err)
	}
	var reason string
	if err := g.pool.QueryRow(g.ctx, `select last_error from outbox where tenant_id=$1 and record_id=$2`, g.tenant.String(), operation.String()).Scan(&reason); err != nil || reason != "expired" {
		t.Fatalf("expiry reason=%q, %v", reason, err)
	}
}

func TestGovernedActionNeverSentReclaimRejects(t *testing.T) {
	g := newGovernedFixture(t, true)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	g.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.ClaimAttemptLease(g.ctx, u, g.tenant, g.s.Attempt.ID, "other-worker", time.Hour, time.Now())
		return err
	})
	g.tick(t, e)
	if err := application.Revoke(g.ctx, g.pool, application.RevokeAuthority{Command: application.Command{Actor: g.actor, Nonce: "revoke-after-yield"}, Grant: g.cmd.Grant, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	g.tick(t, e)
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationRejected {
		t.Fatalf("never-sent operation=%+v, %v", op, err)
	}
	if got := g.resource.Ledger().Effects(g.cmd.Request.Operation); got != 0 {
		t.Fatalf("revoked action produced %d effects", got)
	}
	if err := governedRecoveryWorker(t, g, e).Reconcile(g.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Ledger().Subscriptions()); n != 0 {
		t.Fatalf("never-sent action polled %d receipt streams", n)
	}
}

// notDeliveredEnforcement proves the first send wrote nothing and then
// delegates, so the retry is the delivery the provider records.
type notDeliveredOnce struct {
	inner        contracts.Port
	notDelivered *atomic.Bool
}

func (p *notDeliveredOnce) Role() contracts.Role { return p.inner.Role() }

func (p *notDeliveredOnce) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if p.notDelivered.CompareAndSwap(false, true) {
		return contracts.Reply{}, contracts.ErrNotDelivered
	}
	return p.inner.Send(ctx, request)
}

func (p *notDeliveredOnce) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	return p.inner.Cancel(ctx, cancel)
}

func (p *notDeliveredOnce) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	return p.inner.Subscribe(ctx, cursor)
}

// TestGovernedActionNotDeliveredIsRetriedNotUncertain proves a governed send
// the transport delivered nothing of is released, not resolved as an outcome:
// the operation stays dispatched, the send marker is cleared, and the next
// tick retries the send to its once-per-operation effect. Recording it as
// send_unknown would park the action on reconciliation that can never see a
// receipt for a request the provider never received.
func TestGovernedActionNotDeliveredIsRetriedNotUncertain(t *testing.T) {
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	var sawNotDelivered atomic.Bool
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "governed-recovery",
		wiretest.Wrap(t, g.pool, g.tenant, g.provider, e, func(p contracts.Port) contracts.Port {
			return &notDeliveredOnce{inner: p, notDelivered: &sawNotDelivered}
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if !sawNotDelivered.Load() {
		t.Fatal("the first send never returned proven not-delivered")
	}
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationDispatched {
		t.Fatalf("not-delivered operation=%+v, %v; want dispatched", op, err)
	}
	var marker string
	if err := g.pool.QueryRow(g.ctx, `select last_error from outbox where tenant_id=$1 and record_id=$2`, g.tenant.String(), g.cmd.Request.Operation.String()).Scan(&marker); err != nil || marker != "" {
		t.Fatalf("released marker=%q, %v; want the send marker cleared", marker, err)
	}
	if got := e.Ledger().Deliveries(g.cmd.Request.Operation); got != 1 {
		// One delivery is the pre-action hook the submission enforced; the
		// not-delivered execute send must not have reached the provider.
		t.Fatalf("deliveries=%d after a not-delivered send, want only the hook", got)
	}
	if err := w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.Ledger().Deliveries(g.cmd.Request.Operation); got != 2 {
		t.Fatalf("deliveries=%d, want the retried delivery on top of the hook", got)
	}
	if got := e.Ledger().Effects(g.cmd.Request.Operation); got != 2 {
		// One effect is the hook's own decision record; the retried execute
		// adds exactly one resource.mutate — never a duplicate execution.
		t.Fatalf("effects=%d, want the hook decision plus one execution", got)
	}
	op, err = store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationSucceeded {
		t.Fatalf("retried operation=%+v, %v; want succeeded", op, err)
	}
}

// TestGovernedActionNotDeliveredThenRevokedRejects proves the cleared send
// marker carries into a give-up: an action the transport proved it never
// delivered, whose grant is revoked before the retry, is rejected rather than
// left uncertain waiting for a receipt the provider can never produce.
func TestGovernedActionNotDeliveredThenRevokedRejects(t *testing.T) {
	g := newGovernedFixture(t, true)
	e := g.enforcement(t, simulator.AllowHook(), nil)
	g.enforce(t, e, g.cmd)
	var sawNotDelivered atomic.Bool
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "governed-recovery",
		wiretest.Wrap(t, g.pool, g.tenant, g.provider, e, func(p contracts.Port) contracts.Port {
			return &notDeliveredOnce{inner: p, notDelivered: &sawNotDelivered}
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if !sawNotDelivered.Load() {
		t.Fatal("the first send never returned proven not-delivered")
	}
	if err := application.Revoke(g.ctx, g.pool, application.RevokeAuthority{Command: application.Command{Actor: g.actor, Nonce: "revoke-unsent"}, Grant: g.cmd.Grant, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationRejected {
		t.Fatalf("revoked not-delivered operation=%+v, %v; want rejected", op, err)
	}
	if got := g.resource.Ledger().Effects(g.cmd.Request.Operation); got != 0 {
		t.Fatalf("revoked action produced %d effects", got)
	}
}

type receiptResetPort struct {
	contracts.Port
	tooOld            bool
	replayUnavailable bool
}

func (p receiptResetPort) Subscribe(ctx context.Context, c contracts.Cursor) (contracts.Stream, error) {
	if p.replayUnavailable && c.Generation > 1 {
		return nil, errors.New("receipt replay temporarily unavailable")
	}
	if p.tooOld && c.After > 0 {
		return &receiptResetStream{frame: contracts.StreamFrame{Kind: contracts.StreamCursorTooOld}}, nil
	}
	if !p.tooOld && c.Generation == 1 {
		return &receiptResetStream{frame: contracts.StreamFrame{Kind: contracts.StreamCleared, NewGeneration: 2}}, nil
	}
	return p.Port.Subscribe(ctx, c)
}

type receiptResetStream struct {
	frame contracts.StreamFrame
	sent  bool
}

func (s *receiptResetStream) Next(context.Context) (contracts.StreamFrame, error) {
	if s.sent {
		return contracts.StreamFrame{}, io.EOF
	}
	s.sent = true
	return s.frame, nil
}

func (*receiptResetStream) Close() error { return nil }

func TestGovernedActionReceiptRehydrates(t *testing.T) {
	for _, mode := range []string{"cleared", "cursor_too_old"} {
		t.Run(mode, func(t *testing.T) {
			g := newGovernedFixture(t, false)
			e := g.enforcement(t, simulator.AllowHook(), errors.New("reply lost"))
			g.enforce(t, e, g.cmd)
			g.tick(t, e)
			if mode == "cursor_too_old" {
				g.seed(t, func(u *store.UnitOfWork) error {
					_, err := store.AdvanceCursor(g.ctx, u, contracts.Cursor{Tenant: g.tenant, Attempt: g.s.Attempt.ID, Operation: g.cmd.Request.Operation, Stream: "dispatch", Generation: 1, After: 10})
					return err
				})
			}
			w := governedRecoveryWorker(t, g, receiptResetPort{Port: e, tooOld: mode == "cursor_too_old"})
			for range 2 {
				if err := w.Reconcile(g.ctx); err != nil {
					t.Fatal(err)
				}
			}
			v, err := application.GetGovernedAction(g.ctx, g.pool, g.actor, g.cmd.Request.Operation)
			if err != nil || v.Receipt == nil {
				t.Fatalf("receipt not recovered: db=%d receipt=%+v, %v", g.count(t, "enforcement_receipt", "operation_id=$2", g.cmd.Request.Operation.String()), v.Receipt, err)
			}
			op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
			if err != nil || op.State != lifecycle.OperationSucceeded {
				t.Fatalf("reconciled operation=%+v, %v", op, err)
			}
			if got := g.count(t, "outbox", "payload->>'method'='post_action'"); got != 1 {
				t.Fatalf("recovered post-hook intents=%d, want 1", got)
			}
			postHook := hookPort{Port: e, send: func(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
				if request.Method != contracts.MethodPostAction {
					return e.Send(ctx, request)
				}
				result, err := json.Marshal(contracts.HookResponse{Kind: contracts.HookPostAction, AgentMessage: "recovered post hook"})
				if err != nil {
					return contracts.Reply{}, err
				}
				return contracts.Reply{Operation: request.Operation, Delivery: request.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: result}, nil
			}}
			if err := governedRecoveryWorker(t, g, postHook).Tick(g.ctx); err != nil {
				t.Fatal(err)
			}
			if evidence, err := store.GetHookStageEvidence(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, contracts.HookPostAction); err != nil || evidence.OutputDigest == "" {
				t.Fatalf("recovered post-hook evidence=%+v, %v", evidence, err)
			}
			if got := g.resource.Ledger().Effects(g.cmd.Request.Operation); got != 1 {
				t.Fatalf("recovery effects=%d", got)
			}
			if got := e.Ledger().Deliveries(g.cmd.Request.Operation); got != 2 {
				t.Fatalf("recovery redispatched action: %d deliveries", got)
			}
			cursor, err := store.GetCursor(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, "dispatch")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cleared" && cursor.Generation != 2 || mode == "cursor_too_old" && cursor.After != 10 {
				t.Fatalf("unexpected durable cursor: %+v", cursor)
			}
		})
	}
}

func TestGovernedActionReceiptRehydrateUnavailable(t *testing.T) {
	g := newGovernedFixture(t, false)
	e := g.enforcement(t, simulator.AllowHook(), errors.New("reply lost"))
	g.enforce(t, e, g.cmd)
	g.tick(t, e)
	w := governedRecoveryWorker(t, g, receiptResetPort{Port: e, replayUnavailable: true})
	for range 2 {
		if err := w.Reconcile(g.ctx); err != nil {
			t.Fatal(err)
		}
	}
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation)
	if err != nil || op.State != lifecycle.OperationUncertain {
		t.Fatalf("unavailable replay operation=%+v, %v", op, err)
	}
	cursor, err := store.GetCursor(g.ctx, g.pool, g.tenant, g.cmd.Request.Operation, "dispatch")
	if err != nil || cursor.Generation != 2 {
		t.Fatalf("reset not retained: %+v, %v", cursor, err)
	}
	if got := e.Ledger().Deliveries(g.cmd.Request.Operation); got != 2 {
		t.Fatalf("replay failure redispatched: %d deliveries", got)
	}
}

func governedRecoveryWorker(t *testing.T, g *governedFixture, port contracts.Port) *worker.Worker {
	t.Helper()
	w, err := worker.New(g.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, worker.DefaultConfig(), "governed-recovery",
		wiretest.Ports(t, g.pool, g.tenant, nil, wiretest.Enforcement(map[ids.ProviderID]contracts.Port{g.provider: port})))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestGovernedActionDowngradePreservesPendingApproval(t *testing.T) {
	f := newFixture(t, lifecycle.HarnessLive)
	f.seed(t, func(u *store.UnitOfWork) error {
		_, err := store.UpdatePolicyGeneration(f.ctx, u, store.PolicyUpdate{Tenant: f.tenant, ExpectedGeneration: 1, NextGeneration: 1, Settings: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, Ceiling: map[identity.ActionClass]identity.Permission{"resource.mutate": identity.PermissionAlways}, At: f.at})
		return err
	})
	s, rt := f.executing(t, "pending-scope")
	scope := identity.Scope{Class: identity.ScopeSubject, Target: "screen"}
	f.raise(t, rt, s, 2, contracts.AttentionRequested{NativeRef: "pending-scope", Kind: lifecycle.AttentionActionApproval, Action: "resource.mutate", ActionDigest: identity.DigestOf([]byte(`{"resource":"screen","value":1}`)), Scope: scope, ExpiresAt: f.at.Add(time.Hour)})
	request := f.attention(t, s)
	db, err := sql.Open("pgx", f.pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	provider, err := migrations.NewProvider(db, migrations.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(f.ctx, 5, false); err == nil || !strings.Contains(err.Error(), "cannot downgrade governed actions") {
		t.Fatalf("downgrade did not refuse version 5 evidence: %v", err)
	}
	if version, err := provider.GetDBVersion(f.ctx); err != nil || version != migrations.CurrentVersion {
		t.Fatalf("failed downgrade changed schema version: %d, %v", version, err)
	}
	preserved, err := store.GetAttention(f.ctx, f.pool, f.tenant, request.ID)
	if err != nil || preserved.Scope != scope {
		t.Fatalf("failed downgrade lost approval scope: %+v, %v", preserved.Scope, err)
	}
	decision := f.decide(t, f.decideCommand("approve-preserved-scope", preserved, lifecycle.DecisionApproveOnce))
	if decision.GrantID == "" {
		t.Fatal("preserved approval did not issue authority")
	}
}
