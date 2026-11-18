package postgres

import (
	"context"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// listSchemaVersion is the first schema with provider names and the task
// listing index (ADR-0028). A binary serving an earlier schema during a
// rollout lists tasks without the index, lists providers without names and
// refuses to write a name (ADR-0011).
const listSchemaVersion = 20

// TaskPosition is where a page of tasks starts after: the creation time and
// identifier of the last task of the page before. A creation time never
// changes, so a position names the same place however the data moves.
type TaskPosition struct {
	CreatedAt time.Time
	Task      ids.TaskID
}

// TaskFilter narrows a task listing. A zero field does not filter.
type TaskFilter struct {
	Owner ids.PrincipalID
	State lifecycle.TaskState
	Agent ids.AgentID
}

// ListTaskPositions returns up to limit tasks of the tenant, newest first by
// creation, that come after position when it is set. The caller reads each task.
func ListTaskPositions(ctx context.Context, q Querier, tenant ids.TenantID, filter TaskFilter, after *TaskPosition, limit int) ([]TaskPosition, error) {
	var owner, state, agent, createdAt, taskID any
	if !filter.Owner.IsZero() {
		owner = filter.Owner.String()
	}
	if filter.State != "" {
		state = string(filter.State)
	}
	if !filter.Agent.IsZero() {
		agent = filter.Agent.String()
	}
	if after != nil {
		createdAt, taskID = after.CreatedAt, after.Task.String()
	}
	rows, err := q.Query(ctx, `
		select task_id::text, created_at from task t
		where tenant_id = $1
		  and ($2::uuid is null or owner_id = $2::uuid)
		  and ($3::text is null or state = $3::text)
		  and ($4::text is null or to_jsonb(t)->>'agent_id' = $4::text)
		  and ($5::timestamptz is null or (created_at, task_id) < ($5::timestamptz, $6::uuid))
		order by created_at desc, task_id desc
		limit $7`,
		tenant.String(), owner, state, agent, createdAt, taskID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var positions []TaskPosition
	for rows.Next() {
		var text string
		var position TaskPosition
		if err := rows.Scan(&text, &position.CreatedAt); err != nil {
			return nil, err
		}
		if position.Task, err = parseID[ids.Task](`task.task_id`, text); err != nil {
			return nil, err
		}
		positions = append(positions, position)
	}
	return positions, rows.Err()
}

// ProviderListing is a registration as a surface sees it: what a person needs
// to choose a provider and nothing operator-only (ADR-0028).
type ProviderListing struct {
	Provider    ids.ProviderID
	Role        contracts.Role
	Name        string
	HarnessMode lifecycle.HarnessMode
}

// ListProviderListings returns the tenant's registrations in the given roles,
// by role, then name, then identifier. A runtime carries its harness mode. A
// database before the list schema has no names.
func ListProviderListings(ctx context.Context, q Querier, tenant ids.TenantID, roles []contracts.Role) ([]ProviderListing, error) {
	version, err := SchemaVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	nameColumn := `null::text`
	if version >= listSchemaVersion {
		nameColumn = `r.display_name`
	}
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = string(role)
	}
	rows, err := q.Query(ctx, `
		select r.provider_id::text, r.role, `+nameColumn+`, coalesce(h.mode, '')
		from provider_registration r
		left join harness_qualification h on h.tenant_id = r.tenant_id and h.provider_id = r.provider_id and r.role = 'runtime'
		where r.tenant_id = $1 and r.role = any($2)
		order by r.role, `+nameColumn+` nulls last, r.provider_id`,
		tenant.String(), names,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	listings := []ProviderListing{}
	for rows.Next() {
		var id, role, mode string
		var name *string
		if err := rows.Scan(&id, &role, &name, &mode); err != nil {
			return nil, err
		}
		listing := ProviderListing{Role: contracts.Role(role), HarnessMode: lifecycle.HarnessMode(mode)}
		if listing.Provider, err = parseID[ids.Provider](`provider_registration.provider_id`, id); err != nil {
			return nil, err
		}
		if name != nil {
			listing.Name = *name
		}
		listings = append(listings, listing)
	}
	return listings, rows.Err()
}
