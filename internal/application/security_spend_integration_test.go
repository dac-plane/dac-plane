//go:build integration

package application_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestSecuritySpendCancellationWinsBeforeAssertionScopeLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-cancel-assert")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at)
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(f.ctx) }()
	if _, err := store.GetBudgetForUpdate(f.ctx, u, f.tenant, s.Budget.ID); err != nil {
		t.Fatal(err)
	}
	var gatePID int
	if err := u.QueryRow(f.ctx, `select pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	waitBlockedBy := func(blocker int) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancel()
		for {
			var pid int
			if err := f.pool.QueryRow(ctx, `select coalesce((select pid from pg_stat_activity where datname=current_database() and $1=any(pg_blocking_pids(pid)) limit 1),0)`, blocker).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if pid != 0 {
				return pid
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("command did not reach the expected database lock")
			}
		}
	}
	// Cancellation holds Attempt/Task while waiting to release the budget.
	// The assertion can read the old committed snapshot, but must then wait
	// for cancellation's Attempt lock before it can authorize anything.
	cancelDone, assertDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := application.Cancel(f.ctx, f.pool, f.cancelCommand("cancel-before-assert", s.Task.ID, s.Task.Version))
		cancelDone <- err
	}()
	cancelPID := waitBlockedBy(gatePID)
	cmd := f.assertCommand("assert-after-cancel", s, assertion)
	go func() {
		_, err := application.Assert(f.ctx, f.pool, cmd)
		assertDone <- err
	}()
	waitBlockedBy(cancelPID)
	if err := u.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	if err := <-assertDone; !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("assertion after cancellation=%v, want refusal", err)
	}
	after := f.reload(t, s.Nonce)
	if after.Task.State != lifecycle.TaskCancelled || after.Attempt.State != lifecycle.AttemptTerminated || after.Reservation.State != lifecycle.ReservationReleased || after.Budget.Reserved != 0 || after.Challenge.State != lifecycle.SpendPending || after.Operation != s.Operation || after.Receipt != nil || after.Dispatch != nil {
		t.Fatalf("assertion changed cancelled scheduling: %+v", after)
	}
	if f.count(t, "spend_receipt", "true") != 0 || f.count(t, "command_record", "command='assert_spend'") != 0 || f.count(t, "outbox", "true") != 0 {
		t.Fatal("cancelled assertion persisted proof or dispatch")
	}
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if rt.Ledger().Effects(s.Operation.ID) != 0 || rt.Ledger().Deliveries(s.Operation.ID) != 0 {
		t.Fatal("cancelled assertion reached the runtime")
	}
}

func TestSecuritySpendConcurrentProofHasOneWinningCommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-concurrent")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at)
	if err != nil {
		t.Fatal(err)
	}
	start, done := make(chan struct{}), make(chan error, 2)
	for i := range 2 {
		cmd := f.assertCommand(fmt.Sprintf("security-racing-%d", i), s, assertion)
		go func() {
			<-start
			_, err := application.Assert(f.ctx, f.pool, cmd)
			done <- err
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-done; err == nil {
			winners++
		} else {
			requireRefusal(t, err, lifecycle.RejectTerminalState)
		}
	}
	if winners != 1 || f.count(t, "command_record", "command='assert_spend'") != 1 || f.count(t, "spend_receipt", "true") != 1 || f.count(t, "reservation", "true") != 1 {
		t.Fatalf("racing assertions produced %d winners or duplicate records", winners)
	}
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := rt.Ledger().Deliveries(s.Operation.ID); got != 1 {
		t.Fatalf("racing assertions produced %d effects", got)
	}
}

func TestSecuritySpendExpiryRecheckedAfterChallengeLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-lock")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at)
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.Begin(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Rollback(f.ctx) }()
	if _, err := store.GetSpendChallengeForUpdate(f.ctx, u, f.tenant, s.Challenge.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := application.Assert(f.ctx, f.pool, f.assertCommand("security-lock-assert", s, assertion))
		done <- err
	}()
	waitCtx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	for {
		var blocked bool
		if err := f.pool.QueryRow(waitCtx, `select exists(select 1 from pg_stat_activity where datname=current_database() and wait_event_type='Lock' and query like '%from spend_challenge%for update%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-waitCtx.Done():
			t.Fatal("assertion did not reach the challenge lock")
		}
	}
	if _, err := u.Exec(f.ctx, `update spend_challenge set expires_at=created_at+interval '1 microsecond' where tenant_id=$1 and spend_id=$2`, f.tenant.String(), s.Challenge.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := u.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, <-done, lifecycle.RejectRequestExpired)
	if after := f.reload(t, s.Nonce); after.Receipt != nil || after.Dispatch != nil || after.Reservation != s.Reservation {
		t.Fatalf("expired lock waiter changed scheduling: %+v", after)
	}
}

func TestSecuritySpendTermsCannotBeSubstituted(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-terms")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*lifecycle.SpendAssertion){
		"tenant":    func(a *lifecycle.SpendAssertion) { a.Terms.Tenant = mustID[ids.Tenant](t) },
		"principal": func(a *lifecycle.SpendAssertion) { a.Terms.Principal = mustID[ids.Principal](t) },
		"task":      func(a *lifecycle.SpendAssertion) { a.Terms.Task = mustID[ids.Task](t) },
		"attempt":   func(a *lifecycle.SpendAssertion) { a.Terms.Attempt = mustID[ids.Attempt](t) },
		"operation": func(a *lifecycle.SpendAssertion) { a.Terms.Operation = mustID[ids.Operation](t) },
		"amount":    func(a *lifecycle.SpendAssertion) { a.Terms.Amount++ },
		"unit":      func(a *lifecycle.SpendAssertion) { a.Terms.Unit = "other_unit" },
	} {
		t.Run(name, func(t *testing.T) {
			forged := assertion
			mutate(&forged)
			_, err := application.Assert(f.ctx, f.pool, f.assertCommand("security-terms-"+name, s, forged))
			requireRefusal(t, err, lifecycle.RejectAssertionMismatch)
		})
	}
	if after := f.reload(t, s.Nonce); after.Receipt != nil || after.Dispatch != nil || after.Reservation != s.Reservation || after.Budget != s.Budget {
		t.Fatalf("forged assertion changed accounting: %+v", after)
	}
}

func TestSecuritySpendBackdatedAssertionCannotExtendExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-expired")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Caller evidence time cannot stand in for the authorization clock.
	if _, err := f.pool.Exec(f.ctx, `update spend_challenge set expires_at=created_at+interval '1 microsecond' where tenant_id=$1 and spend_id=$2`, f.tenant.String(), s.Challenge.ID.String()); err != nil {
		t.Fatal(err)
	}
	assertion.AssertedAt = s.Challenge.CreatedAt
	_, err = application.Assert(f.ctx, f.pool, f.assertCommand("security-assert-expired", s, assertion))
	if err == nil {
		t.Fatal("expired challenge accepted using caller's backdated assertion time")
	}
	after := f.reload(t, s.Nonce)
	if after.Receipt != nil || after.Dispatch != nil || after.Operation.State != lifecycle.OperationRecorded || after.Challenge.State != lifecycle.SpendPending || after.Reservation != s.Reservation {
		t.Fatalf("expired assertion changed scheduling: %+v", after)
	}
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := len(rt.Ledger().Sent()); got != 0 {
		t.Fatalf("expired approval produced %d sends", got)
	}
}

func TestSecuritySpendProofCannotReplayThroughAnotherCommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t, lifecycle.HarnessLive)
	s := f.governed(t, "security-replay")
	assertion, err := (&simulator.Spender{Tenant: f.tenant, Principal: f.principal}).Assert(*s.Challenge, f.at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.assertCommand("security-original", s, assertion)
	accepted, err := application.Assert(f.ctx, f.pool, cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Nonce = "security-other"
	if _, err := application.Assert(f.ctx, f.pool, cmd); err == nil {
		t.Fatal("consumed spend assertion accepted through a different command nonce")
	}
	// An exact replay of the original command still succeeds after expiry.
	if _, err := f.pool.Exec(f.ctx, `update spend_challenge set expires_at=clock_timestamp()-interval '1 second', created_at=clock_timestamp()-interval '2 minutes' where tenant_id=$1 and spend_id=$2`, f.tenant.String(), s.Challenge.ID.String()); err != nil {
		t.Fatal(err)
	}
	cmd.Nonce = "security-original"
	again, err := application.Assert(f.ctx, f.pool, cmd)
	if err != nil || again.Dispatch.ID != accepted.Dispatch.ID || *again.Receipt != *accepted.Receipt || again.Reservation != accepted.Reservation {
		t.Fatalf("original replay = %+v, %v", again, err)
	}
	rt := f.runtime(t, s.Operation.ID)
	f.tick(t, rt)
	if got := rt.Ledger().Deliveries(s.Operation.ID); got > 1 {
		t.Fatalf("replayed proof produced %d deliveries", got)
	}
}
