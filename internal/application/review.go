package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

const commandReviewResult = "review_result"

// ReworkTerms deliberately contains no new ceiling: further work consumes
// the same Task's budget and needs a fresh deadline and spend choice.
type ReworkTerms struct {
	Provider            ids.ProviderID   `json:"provider_id"`
	EnvironmentProvider ids.ProviderID   `json:"environment_provider_id"`
	Demand              contracts.Demand `json:"demand"`
	Deadline            time.Time        `json:"deadline"`
	Unit                lifecycle.Unit   `json:"unit"`
	Amount              uint64           `json:"amount"`
	Spend               bool             `json:"spend"`
}
type ReviewResultCommand struct {
	Command
	Task        ids.TaskID
	Revision    ids.RevisionID
	Disposition lifecycle.Disposition
	Rework      *ReworkTerms
	At          time.Time
}

// ReviewView retains immutable judgement; Rework is loaded from the same
// command's scheduling record and may have advanced since review committed.
type ReviewView struct {
	Review     lifecycle.ResultReview `json:"review"`
	Rework     *Submission            `json:"-"`
	Scheduling json.RawMessage        `json:"scheduling,omitempty"`
}

func ReviewResult(ctx context.Context, pool *pgxpool.Pool, c ReviewResultCommand) (ReviewView, error) {
	c.At = c.At.UTC().Truncate(time.Microsecond)
	if err := c.Command.validate(false); err != nil {
		return ReviewView{}, err
	}
	if c.At.IsZero() || c.Task.IsZero() || c.Revision.IsZero() {
		return ReviewView{}, ErrInvalidCommand
	}
	if (c.Disposition == lifecycle.DispositionRequestChanges) != (c.Rework != nil) {
		return ReviewView{}, fmt.Errorf("%w: rework terms are required only for request_changes", ErrInvalidCommand)
	}
	if c.Rework != nil {
		terms := *c.Rework
		terms.Deadline = terms.Deadline.UTC().Truncate(time.Microsecond)
		if terms.Demand == "" {
			terms.Demand = contracts.DemandBackground
		}
		c.Rework = &terms
	}
	payload := struct {
		Task        ids.TaskID
		Revision    ids.RevisionID
		Disposition lifecycle.Disposition
		Rework      *ReworkTerms
		Version     uint64
	}{c.Task, c.Revision, c.Disposition, c.Rework, c.ExpectedVersion}
	digest, err := fingerprint(commandReviewResult, payload)
	if err != nil {
		return ReviewView{}, err
	}
	raw, err := execute(ctx, pool, c.Command, commandReviewResult, digest, c.At, func(u *unit) (json.RawMessage, error) { return reviewResult(ctx, u, c) })
	if err != nil {
		return ReviewView{}, err
	}
	return loadReview(ctx, pool, c.Actor.Tenant(), raw)
}

func loadReview(ctx context.Context, q store.Querier, tenant ids.TenantID, raw json.RawMessage) (ReviewView, error) {
	var v ReviewView
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	if len(v.Scheduling) > 0 {
		s, err := loadSubmission(ctx, q, tenant, "", v.Scheduling)
		if err != nil {
			return v, err
		}
		v.Rework = &s
	}
	return v, nil
}

func reviewResult(ctx context.Context, u *unit, c ReviewResultCommand) (json.RawMessage, error) {
	tenant := c.Actor.Tenant()
	task, err := store.GetTask(ctx, u.uow, tenant, c.Task)
	if err != nil {
		return nil, err
	}
	// The completed predecessor Attempt is locked before Task to match the
	// result worker's scope order, even when review races cancellation.
	attempt, err := store.GetAttemptByTask(ctx, u.uow, tenant, task.ID)
	if err != nil {
		return nil, err
	}
	_, task, err = store.LockResultScope(ctx, u.uow, tenant, attempt.ID)
	if err != nil {
		return nil, err
	}
	if _, present := task.Brief(); !present {
		brief, briefErr := store.GetTaskExecutionBrief(ctx, u.uow, task)
		if briefErr == nil {
			task.Objective, task.AcceptanceCriteria = brief.Objective, brief.AcceptanceCriteria
		} else if !errors.Is(briefErr, store.ErrNotFound) {
			return nil, briefErr
		}
	}
	head, err := store.LatestRevision(ctx, u.uow, tenant, task.ID)
	if err != nil {
		return nil, err
	}
	id, err := ids.New[ids.Review]()
	if err != nil {
		return nil, err
	}
	review := lifecycle.ResultReview{ID: id, Tenant: tenant, Task: task.ID, Revision: c.Revision, Reviewer: c.Actor.Principal().ID, Disposition: c.Disposition, TaskVersion: c.ExpectedVersion, ReviewedAt: c.At}
	tr, err := u.transition(c.ExpectedVersion, nil)
	if err != nil {
		return nil, err
	}
	next, record, _, err := store.RecordReview(ctx, u.uow, head, review, nil, tr)
	if err != nil {
		return nil, err
	}
	// RecordReview reloads the persisted task. At schema 13 that row has no
	// brief columns, so restore the immutable command-result fallback before a
	// request-changes review schedules its next runtime attempt.
	if brief, present := task.Brief(); present {
		next.Objective, next.AcceptanceCriteria = brief.Objective, brief.AcceptanceCriteria
	}
	v := ReviewView{Review: record}
	if c.Rework != nil {
		if attempt.State != lifecycle.AttemptCompleted || head.Attempt != attempt.ID {
			return nil, ErrInvalidCommand
		}
		facts, err := scheduleRework(ctx, u, next, attempt, *c.Rework, c.Nonce, c.Actor)
		if err != nil {
			return nil, err
		}
		v.Scheduling, err = json.Marshal(facts)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(v)
}

func scheduleRework(ctx context.Context, u *unit, task lifecycle.Task, previous lifecycle.Attempt, t ReworkTerms, nonce string, actor identity.AuthorizationContext) (submission, error) {
	brief, present := task.Brief()
	if !present {
		return submission{}, fmt.Errorf("%w: legacy task has no execution brief", ErrInvalidCommand)
	}
	c := SubmitTask{Command: Command{Actor: actor, Nonce: nonce}, At: u.at, Provider: t.Provider, EnvironmentProvider: t.EnvironmentProvider, Brief: brief, Demand: t.Demand, Deadline: t.Deadline, Unit: t.Unit, Ceiling: 1, Amount: t.Amount, Spend: t.Spend}
	// Reuse submission validation, but authorize with the already authenticated
	// owner from the surrounding command, not a new task or budget.
	if err := c.validate(); err != nil {
		return submission{}, err
	}
	if err := c.checkDeadline(); err != nil {
		return submission{}, err
	}
	if !t.Deadline.After(time.Now()) {
		return submission{}, ErrInvalidCommand
	}
	h, err := store.GetHarness(ctx, u.uow, task.Tenant, t.Provider)
	if err != nil {
		return submission{}, err
	}
	if h.Mode != lifecycle.HarnessLive {
		return submission{}, ErrNoHarness
	}
	if _, err := store.GetProviderRegistration(ctx, u.uow, task.Tenant, t.EnvironmentProvider, contracts.RoleEnvironment); errors.Is(err, store.ErrNotFound) {
		return submission{}, ErrNoEnvironment
	} else if err != nil {
		return submission{}, err
	}
	if previous.Number >= uint32(1<<31-1) {
		return submission{}, ErrInvalidCommand
	}
	id, err := ids.New[ids.Attempt]()
	if err != nil {
		return submission{}, err
	}
	attempt, err := lifecycle.NewAttempt(id, task.Tenant, task.ID, previous.Number+1, u.at)
	if err != nil {
		return submission{}, err
	}
	environmentOperation, err := ids.New[ids.Operation]()
	if err != nil {
		return submission{}, err
	}
	attempt.EnvironmentProvider, attempt.EnvironmentOperation = t.EnvironmentProvider, environmentOperation
	if _, _, err = store.InsertAttempt(ctx, u.uow, attempt); err != nil {
		return submission{}, err
	}
	budget, err := store.GetBudgetForUpdateByTaskUnit(ctx, u.uow, task.Tenant, task.ID, t.Unit)
	if err != nil {
		return submission{}, err
	}
	return scheduleAttempt(ctx, u, task, attempt, budget, c, h, commandReviewResult)
}

// GetReviewedResult reads the owner's recorded review outcome and rework.
func GetReviewedResult(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, nonce string) (ReviewView, error) {
	if actor.Principal().Validate() != nil {
		return ReviewView{}, ErrUnauthorized
	}
	r, err := store.GetCommandRecord(ctx, pool, actor.Tenant(), actor.Principal().ID, commandReviewResult, nonce)
	if errors.Is(err, store.ErrNotFound) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}
	return loadReview(ctx, pool, actor.Tenant(), r.Result)
}
