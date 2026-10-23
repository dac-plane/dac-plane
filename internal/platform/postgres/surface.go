package postgres

import (
	"context"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// StreamEvent is one committed durable event as the Surface API streams it.
// Position orders the log; Version is the record's own gapless revision, so
// a consumer can discard an event older than one it already applied for the
// same record. Task is zero for records outside any task.
type StreamEvent struct {
	Position   int64
	Tenant     ids.TenantID
	Task       ids.TaskID
	Record     lifecycle.RecordKind
	RecordID   ids.AnyID
	Event      string
	From       string
	To         string
	Version    uint64
	OccurredAt time.Time
}

// StreamGeneration returns the current event stream generation.
func StreamGeneration(ctx context.Context, q Querier) (uint64, error) {
	var generation int64
	if err := q.QueryRow(ctx, `select generation from event_stream`).Scan(&generation); err != nil {
		return 0, err
	}
	return uint64(generation), nil
}

// StreamHead returns the highest committed position in tenant's stream, or
// zero when it has none. A cursor beyond it cannot come from this stream.
func StreamHead(ctx context.Context, q Querier, tenant ids.TenantID) (int64, error) {
	var head int64
	err := q.QueryRow(ctx, `select coalesce(max(position), 0) from event_position where tenant_id = $1`, tenant.String()).Scan(&head)
	return head, err
}

// ListStreamEvents returns up to limit events of tenant after position, in
// position order, restricted to task when it is non-zero and to tasks owned
// by owner when owner is non-zero (events outside any task are then
// excluded). Positions are drawn at commit under a per-tenant lock (migration
// 11), so they become visible in order and reading after a position never
// skips a later commit.
func ListStreamEvents(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID, owner ids.PrincipalID, after int64, limit int) ([]StreamEvent, error) {
	var taskArg, ownerArg any
	if !task.IsZero() {
		taskArg = task.String()
	}
	if !owner.IsZero() {
		ownerArg = owner.String()
	}
	rows, err := q.Query(ctx, `
		select p.position, coalesce(p.task_id::text, ''), e.record_kind, e.record_id::text, e.event, e.from_state, coalesce(e.to_state, ''), e.version, e.occurred_at
		from event_position p
		join durable_event e on e.tenant_id = p.tenant_id and e.event_id = p.event_id
		where p.tenant_id = $1 and ($2::uuid is null or p.task_id = $2::uuid) and p.position > $3
		  and ($5::uuid is null or p.task_id in (select task_id from task where tenant_id = $1 and owner_id = $5::uuid))
		order by p.position
		limit $4`,
		tenant.String(), taskArg, after, limit, ownerArg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []StreamEvent
	for rows.Next() {
		event := StreamEvent{Tenant: tenant}
		var taskText, kind, recordText string
		var version int64
		if err := rows.Scan(&event.Position, &taskText, &kind, &recordText, &event.Event, &event.From, &event.To, &version, &event.OccurredAt); err != nil {
			return nil, err
		}
		if taskText != "" {
			if event.Task, err = parseID[ids.Task](`event_position.task_id`, taskText); err != nil {
				return nil, err
			}
		}
		if event.RecordID, err = parseID[ids.Any](`durable_event.record_id`, recordText); err != nil {
			return nil, err
		}
		event.Record, event.Version, event.OccurredAt = lifecycle.RecordKind(kind), uint64(version), event.OccurredAt.UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}

// ListOpenAttention returns the tenant's unconsumed attention requests,
// oldest first, restricted to tasks owned by owner when owner is non-zero.
func ListOpenAttention(ctx context.Context, q Querier, tenant ids.TenantID, owner ids.PrincipalID) ([]lifecycle.AttentionRequest, error) {
	var ownerArg any
	if !owner.IsZero() {
		ownerArg = owner.String()
	}
	rows, err := q.Query(ctx, `
		select r.attention_id::text from attention_request r
		join task t on t.tenant_id = r.tenant_id and t.task_id = r.task_id
		where r.tenant_id = $1 and ($2::uuid is null or t.owner_id = $2::uuid) and r.state = any($3)
		order by r.created_at, r.attention_id`,
		tenant.String(), ownerArg, stringsAny(lifecycle.UnconsumedAttentionStates()),
	)
	if err != nil {
		return nil, err
	}
	attentionIDs, err := scanIDs[ids.Attention](rows, `attention_request.attention_id`)
	if err != nil {
		return nil, err
	}
	requests := make([]lifecycle.AttentionRequest, 0, len(attentionIDs))
	for _, id := range attentionIDs {
		request, err := GetAttention(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

// ListRevisionsByTask returns the task's result revisions, oldest first.
func ListRevisionsByTask(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID) ([]lifecycle.ResultRevision, error) {
	rows, err := q.Query(ctx, `
		select revision_id::text from result_revision where tenant_id = $1 and task_id = $2 order by number`,
		tenant.String(), task.String(),
	)
	if err != nil {
		return nil, err
	}
	revisionIDs, err := scanIDs[ids.Revision](rows, `result_revision.revision_id`)
	if err != nil {
		return nil, err
	}
	revisions := make([]lifecycle.ResultRevision, 0, len(revisionIDs))
	for _, id := range revisionIDs {
		revision, err := GetRevision(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, nil
}

// ListReviewsByRevision returns the reviews of one revision, oldest first.
func ListReviewsByRevision(ctx context.Context, q Querier, tenant ids.TenantID, revision ids.RevisionID) ([]lifecycle.ResultReview, error) {
	rows, err := q.Query(ctx, `
		select review_id::text from result_review where tenant_id = $1 and revision_id = $2 order by reviewed_at, review_id`,
		tenant.String(), revision.String(),
	)
	if err != nil {
		return nil, err
	}
	reviewIDs, err := scanIDs[ids.Review](rows, `result_review.review_id`)
	if err != nil {
		return nil, err
	}
	reviews := make([]lifecycle.ResultReview, 0, len(reviewIDs))
	for _, id := range reviewIDs {
		review, err := GetReview(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// GetSpendChallengeByAttempt loads the attempt's newest spend challenge.
func GetSpendChallengeByAttempt(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) (lifecycle.SpendChallenge, error) {
	var text string
	err := q.QueryRow(ctx, `
		select spend_id::text from spend_challenge where tenant_id = $1 and attempt_id = $2 order by created_at desc, spend_id desc limit 1`,
		tenant.String(), attempt.String(),
	).Scan(&text)
	if isNoRows(err) {
		return lifecycle.SpendChallenge{}, ErrNotFound
	}
	if err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	id, err := parseID[ids.Spend](`spend_challenge.spend_id`, text)
	if err != nil {
		return lifecycle.SpendChallenge{}, err
	}
	return GetSpendChallenge(ctx, q, tenant, id)
}

type idRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

func scanIDs[K any](rows idRows, column string) ([]ids.ID[K], error) {
	defer rows.Close()
	var out []ids.ID[K]
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		id, err := parseID[K](column, text)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
