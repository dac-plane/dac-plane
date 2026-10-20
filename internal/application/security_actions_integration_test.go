//go:build integration

package application_test

import (
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSecurityStatefulActionsSerializeAcrossAttemptsAndFreshRetry(t *testing.T) {
	g := newGovernedFixture(t, true)
	s, rt := g.live(t, "second-action-attempt")
	g.ingestNow(t, g.event(rt, s, 1, simulator.RowsFrame(false, row(1, contracts.EntryRuntimeStarted, contracts.RuntimeStarted{NativeRef: "s-2"}))))
	g.raise(t, rt, s, 2, contracts.AttentionRequested{NativeRef: "action-2", Kind: lifecycle.AttentionActionApproval, Action: g.cmd.Request.Action, ActionDigest: g.cmd.Request.InputDigest, Scope: g.cmd.Request.Scope, ExpiresAt: g.at.Add(time.Hour)})
	request := g.attention(t, s)
	dc := g.decideCommand("approve-second", request, lifecycle.DecisionApproveStanding)
	dc.At = g.at
	decision := g.decide(t, dc)
	g.deliver(t, s)
	g.ingestNow(t, g.event(rt, s, 1, simulator.RowsFrame(false, row(3, contracts.EntryDecisionConsumed, contracts.DecisionConsumed{NativeRef: "action-2"}))))
	second := g.next(t)
	second.Request.Attempt = s.Attempt.ID
	second.Request.Operation = request.Operation
	var err error
	second.Grant, err = ids.Parse[ids.Grant](decision.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	second.Precondition.Operation = second.Request.Operation
	e := g.enforcement(t, simulator.AllowHook(), nil, g.cmd, second)
	g.enforce(t, e, g.cmd)
	g.enforce(t, e, second)
	pause := e.Control().Pause(g.cmd.Request.Operation, simulator.BeforeEffect)
	defer pause.Release()
	firstDone := make(chan error, 1)
	go func() { firstDone <- recoveryWorker(t, g.fixture, e).Tick(g.ctx) }()
	select {
	case <-pause.Reached():
	case <-g.ctx.Done():
		t.Fatal("first action did not reach its effect boundary")
	}
	// A separate worker may run the other Attempt, but the adapter's shared
	// physical resource must still refuse concurrent effects.
	if err := recoveryWorker(t, g.fixture, e).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	op, err := store.GetOperation(g.ctx, g.pool, g.tenant, second.Request.Operation)
	if err != nil || op.State != lifecycle.OperationRejected || g.resource.Ledger().Effects(second.Request.Operation) != 0 {
		t.Fatalf("parallel action=%+v %v", op, err)
	}
	pause.Release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if g.resource.Ledger().Effects(g.cmd.Request.Operation) != 1 {
		t.Fatal("first action did not finish once")
	}
	// Busy/stale failure does not authorize blind replay. A distinct action
	// observes the new token and uses its still-valid standing authority.
	g2 := &governedFixture{fixture: g.fixture, s: s, resource: g.resource, cmd: second}
	fresh := g2.next(t)
	e2 := g2.enforcement(t, simulator.AllowHook(), nil, fresh)
	g2.enforce(t, e2, fresh)
	if err := recoveryWorker(t, g.fixture, e2).Tick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if g.resource.Ledger().Effects(fresh.Request.Operation) != 1 || g.resource.Ledger().Effects(second.Request.Operation) != 0 {
		t.Fatal("fresh serialized retry failed or revived rejected work")
	}
}
