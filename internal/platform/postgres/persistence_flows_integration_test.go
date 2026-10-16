//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func TestPersistenceCrashWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	now := time.Now().UTC()

	t.Run("decision completes after crash between insert and move", func(t *testing.T) {
		f := seedTenant(t, ctx, pool)
		uow, _ := store.Begin(ctx, pool)
		defer func() { _ = uow.Rollback(ctx) }()
		request, _ := approvalFixture(t, ctx, uow, f, now)
		dec := lifecycle.Decision{
			ID: mustID[ids.Decision](t), Tenant: f.tenant, Request: request.ID,
			RequestVersion: 1, Principal: f.principal, Kind: lifecycle.DecisionApproveOnce, DecidedAt: now,
		}
		// Stage the crash: the decision row committed, the request never moved.
		if _, err := uow.Exec(ctx, `
			insert into decision (tenant_id, decision_id, attention_id, request_version, principal_id, kind, answer_digest, decided_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			f.tenant.String(), dec.ID.String(), dec.Request.String(), 1,
			dec.Principal.String(), string(dec.Kind), nil, dec.DecidedAt,
		); err != nil {
			t.Fatalf("stage decision row: %v", err)
		}
		tr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
		decided, recorded, replayed, err := store.DecideAttention(ctx, uow, dec, 1, tr)
		if err != nil || !replayed || decided.State != lifecycle.AttentionDecided {
			t.Fatalf("DecideAttention() completion = %v, %v, %v, %v", decided, recorded, replayed, err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		if got := countRows(t, ctx, pool, "decision", "tenant_id = $1", f.tenant.String()); got != 1 {
			t.Fatalf("decision rows = %d, want 1", got)
		}
	})

	t.Run("review completes after crash between insert and move", func(t *testing.T) {
		f := seedTenant(t, ctx, pool)
		uow, _ := store.Begin(ctx, pool)
		defer func() { _ = uow.Rollback(ctx) }()
		task := createTask(t, ctx, uow, f, now)
		attempt := createAttempt(t, ctx, uow, f, task, 1, now)
		operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
		if err != nil {
			t.Fatalf("NewProviderOperation() error = %v", err)
		}
		if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
			t.Fatalf("InsertOperation() error = %v", err)
		}
		manifest := lifecycle.ArtifactManifest{
			Artifact: mustID[ids.Artifact](t), Tenant: f.tenant, Task: task.ID, Attempt: attempt.ID,
			Operation: operation.ID, Producer: f.provider, Kind: lifecycle.ArtifactCodeChange,
			Digest: identity.DigestOf([]byte("bytes")), Size: 5, MediaType: "text/plain", WriterEpoch: 1,
		}
		if _, _, err := store.InsertManifest(ctx, uow, manifest); err != nil {
			t.Fatalf("InsertManifest() error = %v", err)
		}
		artifact := lifecycle.Artifact{ArtifactManifest: manifest, CommittedAt: now}
		if _, _, err := store.InsertArtifact(ctx, uow, artifact); err != nil {
			t.Fatalf("InsertArtifact() error = %v", err)
		}
		revision, err := lifecycle.NewResultRevision(mustID[ids.Revision](t), attempt.ID, []lifecycle.Artifact{artifact}, nil, now)
		if err != nil {
			t.Fatalf("NewResultRevision() error = %v", err)
		}
		if _, _, err := store.InsertRevision(ctx, uow, revision); err != nil {
			t.Fatalf("InsertRevision() error = %v", err)
		}
		for _, event := range []lifecycle.TaskEvent{lifecycle.TaskSubmit, lifecycle.TaskDispatch, lifecycle.TaskProposeResult, lifecycle.TaskVerificationPassed} {
			current, err := store.GetTask(ctx, uow, f.tenant, task.ID)
			if err != nil {
				t.Fatalf("GetTask() error = %v", err)
			}
			tr := store.Transition{ExpectedVersion: current.Version, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
			if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, event, "", tr); err != nil {
				t.Fatalf("ApplyTask(%s) error = %v", event, err)
			}
		}
		rev := lifecycle.ResultReview{
			ID: mustID[ids.Review](t), Tenant: f.tenant, Task: task.ID, Revision: revision.ID,
			Reviewer: f.principal, Disposition: lifecycle.DispositionAccept, TaskVersion: 5, ReviewedAt: now,
		}
		// Stage the crash: the review row committed, the task never moved.
		if _, err := uow.Exec(ctx, `
			insert into result_review (tenant_id, review_id, task_id, revision_id, reviewer_id, disposition, task_version, reviewed_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			f.tenant.String(), rev.ID.String(), rev.Task.String(), rev.Revision.String(),
			rev.Reviewer.String(), string(rev.Disposition), 5, rev.ReviewedAt,
		); err != nil {
			t.Fatalf("stage review row: %v", err)
		}
		tr := store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
		accepted, _, replayed, err := store.RecordReview(ctx, uow, revision, rev, []ids.PrincipalID{f.principal}, tr)
		if err != nil || !replayed || accepted.State != lifecycle.TaskSucceeded {
			t.Fatalf("RecordReview() completion = %v, %v, %v", accepted, replayed, err)
		}
		if err := uow.Commit(ctx); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
	})
}

func TestPersistenceHarnessAndMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()
	fresh := func() store.Transition {
		return store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	}

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	// Harness: off -> shadow -> live -> shadow, plus a refused promotion by
	// a non-principal actor.
	harness := lifecycle.HarnessQualification{Tenant: f.tenant, Provider: f.provider, Mode: lifecycle.HarnessOff, Version: 1, UpdatedAt: now}
	if _, err := store.UpsertHarness(ctx, uow, harness); err != nil {
		t.Fatalf("UpsertHarness() error = %v", err)
	}
	for _, step := range []struct {
		event lifecycle.HarnessEvent
		want  lifecycle.HarnessMode
	}{
		{lifecycle.HarnessPromote, lifecycle.HarnessShadow},
		{lifecycle.HarnessPromote, lifecycle.HarnessLive},
		{lifecycle.HarnessDemote, lifecycle.HarnessShadow},
	} {
		tr := fresh()
		current, err := store.GetHarness(ctx, uow, f.tenant, f.provider)
		if err != nil {
			t.Fatalf("GetHarness() error = %v", err)
		}
		tr.ExpectedVersion = current.Version
		moved, err := store.ApplyHarness(ctx, uow, f.tenant, f.provider, step.event, tr)
		if err != nil || moved.Mode != step.want {
			t.Fatalf("ApplyHarness(%s) = %v, %v", step.event, moved, err)
		}
	}
	providerActor := lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: f.provider}
	current, _ := store.GetHarness(ctx, uow, f.tenant, f.provider)
	badActor := fresh()
	badActor.ExpectedVersion, badActor.Actor = current.Version, providerActor
	if _, err := store.ApplyHarness(ctx, uow, f.tenant, f.provider, lifecycle.HarnessPromote, badActor); lifecycle.RejectionOf(err) != lifecycle.RejectUnauthorizedActor {
		t.Fatalf("provider promote = %v, want unauthorized_actor", err)
	}

	// Migration: preview -> execute -> complete; executing from hold is refused.
	passID := mustID[ids.Migration](t)
	pass, err := lifecycle.NewMigrationPass(passID, f.tenant, f.provider, mustID[ids.Provider](t), now)
	if err != nil {
		t.Fatalf("NewMigrationPass() error = %v", err)
	}
	if _, _, err := store.InsertMigrationPass(ctx, uow, pass); err != nil {
		t.Fatalf("InsertMigrationPass() error = %v", err)
	}
	if _, replayed, err := store.InsertMigrationPass(ctx, uow, pass); err != nil || !replayed {
		t.Fatalf("replay InsertMigrationPass() = %v, %v, want replayed", replayed, err)
	}
	heldID := mustID[ids.Migration](t)
	held, err := lifecycle.NewMigrationPass(heldID, f.tenant, f.provider, mustID[ids.Provider](t), now)
	if err != nil {
		t.Fatalf("NewMigrationPass() error = %v", err)
	}
	if _, _, err := store.InsertMigrationPass(ctx, uow, held); err != nil {
		t.Fatalf("InsertMigrationPass() error = %v", err)
	}
	for _, step := range []struct {
		id     ids.MigrationID
		event  lifecycle.MigrationEvent
		want   lifecycle.MigrationState
		system bool
	}{
		{passID, lifecycle.MigrationPreview, lifecycle.MigrationPreviewed, false},
		{heldID, lifecycle.MigrationHold, lifecycle.MigrationHeld, false},
		{passID, lifecycle.MigrationExecute, lifecycle.MigrationExecuting, false},
		{passID, lifecycle.MigrationComplete, lifecycle.MigrationCompleted, true},
	} {
		current, err := store.GetMigrationPass(ctx, uow, f.tenant, step.id)
		if err != nil {
			t.Fatalf("GetMigrationPass() error = %v", err)
		}
		tr := fresh()
		tr.ExpectedVersion = current.Version
		if step.system {
			tr.Actor = lifecycle.Actor{Kind: lifecycle.ActorSystem}
		}
		moved, err := store.ApplyMigrationPass(ctx, uow, f.tenant, step.id, step.event, tr)
		if err != nil || moved.State != step.want {
			t.Fatalf("ApplyMigrationPass(%s) = %v, %v", step.event, moved, err)
		}
	}
	stuck, _ := store.GetMigrationPass(ctx, uow, f.tenant, heldID)
	stuckTr := fresh()
	stuckTr.ExpectedVersion = stuck.Version
	if _, err := store.ApplyMigrationPass(ctx, uow, f.tenant, heldID, lifecycle.MigrationExecute, stuckTr); lifecycle.RejectionOf(err) != lifecycle.RejectInvalidTransition {
		t.Fatalf("execute-from-hold = %v, want invalid_transition", err)
	}

	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceSpendBudgetRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()
	fresh := func() store.Transition {
		return store.Transition{At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
	}

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	// Spend expiry, budget release, stale policy update.
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	expiring, err := lifecycle.BeginSpend(mustID[ids.Spend](t), lifecycle.SpendTerms{
		Tenant: f.tenant, Principal: f.principal, Task: task.ID, Attempt: attempt.ID,
		Operation: operation.ID, Amount: 5, Unit: "tokens", Nonce: identity.DigestOf([]byte("nonce-expire")),
	}, now.Add(time.Second), now)
	if err != nil {
		t.Fatalf("BeginSpend() error = %v", err)
	}
	if _, _, err := store.InsertSpendChallenge(ctx, uow, expiring); err != nil {
		t.Fatalf("InsertSpendChallenge() error = %v", err)
	}
	expired, err := store.ExpireSpendChallenge(ctx, uow, f.tenant, expiring.ID, 1, now.Add(2*time.Second), fresh())
	if err != nil || expired.State != lifecycle.SpendExpired {
		t.Fatalf("ExpireSpendChallenge() = %v, %v", expired, err)
	}
	if _, err := store.ExpireSpendChallenge(ctx, uow, f.tenant, expiring.ID, 2, now.Add(3*time.Second), fresh()); lifecycle.RejectionOf(err) != lifecycle.RejectTerminalState {
		t.Fatalf("re-expire = %v, want terminal_state", err)
	}

	budget, err := lifecycle.NewBudget(mustID[ids.Budget](t), f.tenant, task.ID, "tokens", 100, now)
	if err != nil {
		t.Fatalf("NewBudget() error = %v", err)
	}
	if _, _, err := store.InsertBudget(ctx, uow, budget); err != nil {
		t.Fatalf("InsertBudget() error = %v", err)
	}
	hold := fresh()
	hold.ExpectedVersion = 1
	_, reservation, _, err := store.ReserveBudget(ctx, uow, store.ReservationRequest{Tenant: f.tenant, Budget: budget.ID, Reservation: mustID[ids.Reservation](t), Attempt: attempt.ID, Amount: 60}, hold)
	if err != nil {
		t.Fatalf("ReserveBudget() error = %v", err)
	}
	free := fresh()
	free.ExpectedVersion = 2
	released, freed, err := store.ReleaseBudget(ctx, uow, f.tenant, budget.ID, reservation.ID, free)
	if err != nil || released.Reserved != 0 || freed.State != lifecycle.ReservationReleased {
		t.Fatalf("ReleaseBudget() = %v, %v, %v", released, freed, err)
	}
	again := fresh()
	again.ExpectedVersion = 3
	if _, _, err := store.ReleaseBudget(ctx, uow, f.tenant, budget.ID, reservation.ID, again); lifecycle.RejectionOf(err) != lifecycle.RejectTerminalState {
		t.Fatalf("double release = %v, want terminal_state", err)
	}

	if _, err := store.UpdatePolicyGeneration(ctx, uow, store.PolicyUpdate{Tenant: f.tenant, ExpectedGeneration: 99, NextGeneration: 100, At: now}); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("stale UpdatePolicyGeneration() = %v, want stale version", err)
	}

	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceGrantsAndPrincipals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	// Standing grant revoke and replay.
	standing := identity.AuthorityGrant{
		ID: mustID[ids.Grant](t), Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider,
		Action: "message.send", Mode: identity.GrantStanding, Scope: identity.Scope{Class: identity.ScopeGlobal},
		GrantedBy: f.principal, PolicyGeneration: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := identity.IssueGrant(standing, identity.Policy{
		Tenant: f.tenant, Generation: 1,
		Settings: map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
		Ceiling:  map[identity.ActionClass]identity.Permission{"message.send": identity.PermissionAlways},
	}); err != nil {
		t.Fatalf("IssueGrant() error = %v", err)
	}
	if err := store.InsertGrant(ctx, uow, standing); err != nil {
		t.Fatalf("InsertGrant() error = %v", err)
	}
	if _, replayed, err := store.RevokeGrant(ctx, uow, f.tenant, standing.ID, now); err != nil || replayed {
		t.Fatalf("RevokeGrant() = %v, %v", replayed, err)
	}
	if _, replayed, err := store.RevokeGrant(ctx, uow, f.tenant, standing.ID, now); err != nil || !replayed {
		t.Fatalf("replay RevokeGrant() = %v, %v, want replayed", replayed, err)
	}

	// Principal mismatch and cursor miss.
	changed := identity.Principal{Tenant: f.tenant, ID: f.principal, Kind: identity.PrincipalService}
	if err := store.InsertPrincipal(ctx, uow, changed); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("changed-kind InsertPrincipal() = %v, want payload mismatch", err)
	}
	if _, err := store.GetCursor(ctx, uow, f.tenant, mustID[ids.Operation](t), "transcript"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetCursor() miss = %v, want not found", err)
	}

	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceObservationsAndEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	attempt := createAttempt(t, ctx, uow, f, task, 1, now)
	operation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, attempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, operation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	// Turn ends, bindings, observations, hook evidence.
	turn := lifecycle.TurnEnd{
		Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider,
		SessionRef: "session-1", TurnRef: "turn-1", Outcome: lifecycle.TurnCompleted, EndedAt: now,
	}
	if _, _, err := store.InsertTurnEnd(ctx, uow, turn); err != nil {
		t.Fatalf("InsertTurnEnd() error = %v", err)
	}
	if _, replayed, err := store.InsertTurnEnd(ctx, uow, turn); err != nil || !replayed {
		t.Fatalf("replay InsertTurnEnd() = %v, %v, want replayed", replayed, err)
	}
	changedTurn := turn
	changedTurn.Outcome = lifecycle.TurnInterrupted
	if _, _, err := store.InsertTurnEnd(ctx, uow, changedTurn); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("changed TurnEnd = %v, want payload mismatch", err)
	}

	binding := lifecycle.Binding{
		Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider, Operation: operation.ID,
		Kind: lifecycle.ResourceRuntimeSession, NativeRef: "box-1", BoundAt: now,
	}
	if _, _, err := store.InsertBinding(ctx, uow, binding); err != nil {
		t.Fatalf("InsertBinding() error = %v", err)
	}
	otherAttempt := createAttempt(t, ctx, uow, f, task, 2, now)
	otherOperation, err := lifecycle.NewProviderOperation(mustID[ids.Operation](t), f.tenant, otherAttempt.ID, f.provider, now)
	if err != nil {
		t.Fatalf("NewProviderOperation() error = %v", err)
	}
	if _, _, err := store.InsertOperation(ctx, uow, otherOperation); err != nil {
		t.Fatalf("InsertOperation() error = %v", err)
	}
	rebound := binding
	rebound.Attempt, rebound.Operation = otherAttempt.ID, otherOperation.ID
	if _, _, err := store.InsertBinding(ctx, uow, rebound); !errors.Is(err, store.ErrPayloadMismatch) {
		t.Fatalf("rebind = %v, want payload mismatch", err)
	}

	observed := lifecycle.EnvironmentObservation{
		Tenant: f.tenant, Attempt: attempt.ID, Provider: f.provider,
		ResourceRef: "box-1", State: lifecycle.RunRunning, Sequence: 3, ObservedAt: now,
	}
	if _, _, err := store.UpsertObservation(ctx, uow, observed); err != nil {
		t.Fatalf("UpsertObservation() error = %v", err)
	}
	stale := observed
	stale.Sequence = 2
	stale.State = lifecycle.RunStarting
	if stored, replayed, err := store.UpsertObservation(ctx, uow, stale); err != nil || !replayed || stored.Sequence != 3 {
		t.Fatalf("stale UpsertObservation() = %v, %v, %v", stored, replayed, err)
	}
	advanced := observed
	advanced.Sequence = 5
	advanced.State = lifecycle.RunHibernated
	if stored, replayed, err := store.UpsertObservation(ctx, uow, advanced); err != nil || replayed || stored.Sequence != 5 {
		t.Fatalf("advanced UpsertObservation() = %v, %v, %v", stored, replayed, err)
	}

	evidence := contracts.HookEvidence{
		Operation: operation.ID, Outcome: contracts.HookDenied,
		OriginalDigest: identity.DigestOf([]byte(`{"in":1}`)),
	}
	if _, _, err := store.InsertHookEvidence(ctx, uow, f.tenant, evidence); err != nil {
		t.Fatalf("InsertHookEvidence() error = %v", err)
	}
	if _, replayed, err := store.InsertHookEvidence(ctx, uow, f.tenant, evidence); err != nil || !replayed {
		t.Fatalf("replay InsertHookEvidence() = %v, %v, want replayed", replayed, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPersistenceConcurrentWriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	setup, _ := store.Begin(ctx, pool)
	task := createTask(t, ctx, setup, f, now)
	if err := setup.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// Two writers race the same version: the row lock serializes them, the
	// loser finds the winner's durable row and replays the same command.
	first, _ := store.Begin(ctx, pool)
	second, _ := store.Begin(ctx, pool)
	defer func() { _ = second.Rollback(ctx) }()
	submit := func(uow *store.UnitOfWork) (bool, error) {
		tr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
		_, replayed, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, lifecycle.TaskSubmit, "", tr)
		return replayed, err
	}
	type outcome struct {
		replayed bool
		err      error
	}
	secondDone := make(chan outcome, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		replayed, err := submit(second)
		secondDone <- outcome{replayed, err}
	}()
	if _, err := submit(first); err != nil {
		t.Fatalf("first ApplyTask() error = %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	loser := <-secondDone
	if loser.err != nil || !loser.replayed {
		t.Fatalf("racing ApplyTask() = %v, %v, want replayed", loser.replayed, loser.err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("second Commit() error = %v", err)
	}

	// A different command racing the same version goes stale instead.
	third, _ := store.Begin(ctx, pool)
	defer func() { _ = third.Rollback(ctx) }()
	tr := store.Transition{ExpectedVersion: 1, At: now, Actor: f.actor, AuditID: newAuditID(t)}
	if _, _, err := store.ApplyTask(ctx, third, f.tenant, task.ID, lifecycle.TaskCancel, "", tr); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("overtaken ApplyTask() = %v, want stale version", err)
	}
}

func TestPersistenceEmptyResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)

	if rows, err := store.OpenOutboxEntries(ctx, pool, f.tenant, 0); err != nil || len(rows) != 0 {
		t.Fatalf("OpenOutboxEntries() = %v, %v, want empty", rows, err)
	}
	if ops, err := store.UncertainOperations(ctx, pool, f.tenant); err != nil || len(ops) != 0 {
		t.Fatalf("UncertainOperations() = %v, %v, want empty", ops, err)
	}
	if claims, err := store.OpenClaims(ctx, pool, f.tenant); err != nil || len(claims) != 0 {
		t.Fatalf("OpenClaims() = %v, %v, want empty", claims, err)
	}
	if cursors, err := store.ListCursors(ctx, pool, f.tenant, mustID[ids.Attempt](t)); err != nil || len(cursors) != 0 {
		t.Fatalf("ListCursors() = %v, %v, want empty", cursors, err)
	}
}

func TestPersistenceOutboxLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databaseURL := testpostgres.Start(t)
	migrate(t, ctx, databaseURL)
	pool := openPool(t, ctx, databaseURL)
	f := seedTenant(t, ctx, pool)
	now := time.Now().UTC()

	uow, _ := store.Begin(ctx, pool)
	defer func() { _ = uow.Rollback(ctx) }()
	task := createTask(t, ctx, uow, f, now)
	for _, event := range []lifecycle.TaskEvent{lifecycle.TaskSubmit, lifecycle.TaskDispatch} {
		current, err := store.GetTask(ctx, uow, f.tenant, task.ID)
		if err != nil {
			t.Fatalf("GetTask() error = %v", err)
		}
		tr := store.Transition{ExpectedVersion: current.Version, At: now, Actor: f.actor, AuditID: newAuditID(t), Outbox: newOutbox()}
		if _, _, err := store.ApplyTask(ctx, uow, f.tenant, task.ID, event, "", tr); err != nil {
			t.Fatalf("ApplyTask(%s) error = %v", event, err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	one, err := store.OpenOutboxEntries(ctx, pool, f.tenant, 1)
	if err != nil || len(one) != 1 {
		t.Fatalf("OpenOutboxEntries(1) = %v, %v, want 1 row", one, err)
	}
	if one[0].CreatedAt.IsZero() || len(one[0].Payload) == 0 {
		t.Fatalf("OpenOutboxEntries() row drops metadata: %+v", one[0])
	}
	all, err := store.OpenOutboxEntries(ctx, pool, f.tenant, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("OpenOutboxEntries(0) = %d rows, %v, want 2", len(all), err)
	}
}
