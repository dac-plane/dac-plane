package application

import (
	"context"
	"fmt"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bounds of a task page (ADR-0028).
const (
	DefaultTaskPage = 50
	MaxTaskPage     = 200
)

// TaskCursor is where a task page starts after, as the client holds it.
type TaskCursor = store.TaskPosition

// TaskQuery selects a page of the tasks a principal may read.
type TaskQuery struct {
	// State and Agent narrow the listing when set.
	State lifecycle.TaskState
	Agent ids.AgentID
	// After continues the listing after the last task of the page before.
	After *TaskCursor
	// Limit is the page size, DefaultTaskPage when zero.
	Limit int
}

// TaskPage is one page of tasks, newest first by creation. Next is where the
// following page starts, nil on the last page.
type TaskPage struct {
	Tasks []lifecycle.Task
	Next  *TaskCursor
}

// ListTasks lists the tasks the actor may read: its own, or the tenant's for a
// tenant reader. The owner filter is part of the query, so a page never holds
// a task the actor cannot read and a cursor reveals nothing (ADR-0028).
func ListTasks(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, query TaskQuery) (TaskPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = DefaultTaskPage
	}
	if limit < 1 || limit > MaxTaskPage {
		return TaskPage{}, fmt.Errorf("%w: a page holds 1 to %d tasks", ErrInvalidCommand, MaxTaskPage)
	}
	if query.State != "" && !containsState(query.State) {
		return TaskPage{}, fmt.Errorf("%w: unknown task state", ErrInvalidCommand)
	}
	page := TaskPage{Tasks: []lifecycle.Task{}}
	err := read(ctx, pool, actor, func(r *reader) error {
		// One more than a page tells whether another follows.
		positions, err := store.ListTaskPositions(ctx, r.uow, r.principal.Tenant,
			store.TaskFilter{Owner: r.owner(), State: query.State, Agent: query.Agent}, query.After, limit+1)
		if err != nil {
			return err
		}
		more := len(positions) > limit
		if more {
			positions = positions[:limit]
		}
		for _, position := range positions {
			task, err := r.task(ctx, position.Task)
			if err != nil {
				return err
			}
			// The position query and this read are separate statements, and a
			// worker may have moved the task between them: it is returned only
			// if it still matches what was asked for, so a page can hold fewer
			// tasks than the limit but never one the filter excludes.
			if !matchesState(task, query.State) {
				continue
			}
			page.Tasks = append(page.Tasks, task)
		}
		if more {
			last := positions[len(positions)-1]
			page.Next = &TaskCursor{CreatedAt: last.CreatedAt, Task: last.Task}
		}
		return nil
	})
	return page, err
}

// matchesState reports whether the task is in the state asked for, any state
// when none is.
func matchesState(task lifecycle.Task, state lifecycle.TaskState) bool {
	return state == "" || task.State == state
}

func containsState(state lifecycle.TaskState) bool {
	for _, s := range lifecycle.TaskStates() {
		if s == state {
			return true
		}
	}
	return false
}

// ProviderListing is a provider a surface may name when creating an agent or
// submitting a task: what a person needs to choose one, nothing operator-only.
type ProviderListing = store.ProviderListing

// ListProviders lists the tenant's runtime and environment providers, the two
// roles a surface names, optionally only those in role. Every principal of the
// tenant may read it, as it may read the agents (ADR-0028).
func ListProviders(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, role contracts.Role) ([]ProviderListing, error) {
	roles := []contracts.Role{contracts.RoleRuntime, contracts.RoleEnvironment}
	if role != "" {
		if role != contracts.RoleRuntime && role != contracts.RoleEnvironment {
			return nil, fmt.Errorf("%w: a surface names runtime and environment providers", ErrInvalidCommand)
		}
		roles = []contracts.Role{role}
	}
	var listings []ProviderListing
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		listings, err = store.ListProviderListings(ctx, r.uow, r.principal.Tenant, roles)
		return err
	})
	return listings, err
}
