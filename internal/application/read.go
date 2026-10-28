package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Reads are owner-scoped (ADR-0007): a principal reads the tasks it owns and
// everything under them; the tenant-reader permission widens that to the whole
// tenant. A record the principal may not read is ErrNotFound, exactly like a
// record that does not exist, so a read never reveals what it hides.

// MaxEventPage bounds one page of events.
const MaxEventPage = 500

// TaskView is a task as a surface reads it: the task, its current attempt,
// the attempt's open attention requests and spend challenge, and the task's
// result revisions with their reviews.
type TaskView struct {
	Task          lifecycle.Task
	Attempt       *lifecycle.Attempt
	OpenAttention []lifecycle.AttentionRequest
	Challenge     *lifecycle.SpendChallenge
	Revisions     []RevisionView
}

// RevisionView is one result revision and the reviews it received.
type RevisionView struct {
	Revision lifecycle.ResultRevision
	Reviews  []lifecycle.ResultReview
}

// EventCursor is a resumable position in one event stream. Stream names the
// stream it belongs to, so a cursor from another task's stream or another
// tenant is reset instead of being applied as a position it never had. The
// zero cursor starts from the beginning.
type EventCursor struct {
	Generation uint64
	Position   int64
	Stream     string
}

// StreamTag identifies one stream: a tenant's task stream, or the tenant
// stream when task is zero. It is a short digest, not a secret: it only
// detects a cursor used on the wrong stream; every read is still authorized.
func StreamTag(tenant ids.TenantID, task ids.TaskID) string {
	scope := "tenant"
	if !task.IsZero() {
		scope = "task/" + task.String()
	}
	sum := sha256.Sum256([]byte(tenant.String() + "/" + scope))
	return hex.EncodeToString(sum[:6])
}

// EventPage is one page of events. Next resumes after the last event
// returned. Reset tells the client its cursor cannot be resumed: it must
// re-read the snapshot and continue from Next.
type EventPage struct {
	Events []store.StreamEvent
	Next   EventCursor
	Reset  bool
}

// reader is one read: a transaction, the authenticated principal, and
// whether it holds the tenant-reader permission for the rest of the read.
type reader struct {
	uow       *store.UnitOfWork
	principal identity.Principal
	tenantAll bool
}

// read runs work in one transaction after confirming the principal exists in
// its tenant and locking its tenant-reader permission, so a revocation waits
// for reads that already started and applies to every read after it.
func read(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, work func(*reader) error) error {
	principal := actor.Principal()
	if err := principal.Validate(); err != nil {
		return fmt.Errorf("%w: an authenticated principal is required", ErrInvalidCommand)
	}
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { _ = uow.Rollback(ctx) }()
	switch stored, err := store.GetPrincipal(ctx, uow, principal.Tenant, principal.ID); {
	case errors.Is(err, store.ErrNotFound), err == nil && stored != principal:
		return ErrUnauthorized
	case err != nil:
		return err
	}
	all, err := store.LockTenantReader(ctx, uow, principal.Tenant, principal.ID)
	if err != nil {
		return err
	}
	if err := work(&reader{uow: uow, principal: principal, tenantAll: all}); err != nil {
		return err
	}
	return uow.Commit(ctx)
}

// task loads a task the principal may read.
func (r *reader) task(ctx context.Context, id ids.TaskID) (lifecycle.Task, error) {
	task, err := store.GetTask(ctx, r.uow, r.principal.Tenant, id)
	if err != nil {
		return lifecycle.Task{}, err
	}
	if task.Owner != r.principal.ID && !r.tenantAll {
		return lifecycle.Task{}, ErrNotFound
	}
	if _, present := task.Brief(); !present {
		brief, briefErr := store.GetTaskExecutionBrief(ctx, r.uow, task.Tenant, task.ID)
		if briefErr == nil {
			task.Objective, task.AcceptanceCriteria = brief.Objective, brief.AcceptanceCriteria
		} else if !errors.Is(briefErr, store.ErrNotFound) {
			return lifecycle.Task{}, briefErr
		}
	}
	return task, nil
}

// owner is the owner filter for tenant-wide reads: zero for a tenant reader.
func (r *reader) owner() ids.PrincipalID {
	if r.tenantAll {
		return ids.PrincipalID{}
	}
	return r.principal.ID
}

// GetTask returns a task the actor may read.
func GetTask(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.TaskID) (TaskView, error) {
	var view TaskView
	err := read(ctx, pool, actor, func(r *reader) error {
		task, err := r.task(ctx, id)
		if err != nil {
			return err
		}
		view.Task = task
		switch attempt, err := store.GetAttemptByTask(ctx, r.uow, task.Tenant, task.ID); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			view.Attempt = &attempt
			if view.OpenAttention, err = store.ListOpenAttentionByAttempt(ctx, r.uow, task.Tenant, attempt.ID); err != nil {
				return err
			}
			switch challenge, err := store.GetSpendChallengeByAttempt(ctx, r.uow, task.Tenant, attempt.ID); {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return err
			default:
				view.Challenge = &challenge
			}
		}
		revisions, err := store.ListRevisionsByTask(ctx, r.uow, task.Tenant, task.ID)
		if err != nil {
			return err
		}
		for _, revision := range revisions {
			reviews, err := store.ListReviewsByRevision(ctx, r.uow, task.Tenant, revision.ID)
			if err != nil {
				return err
			}
			view.Revisions = append(view.Revisions, RevisionView{Revision: revision, Reviews: reviews})
		}
		return nil
	})
	return view, err
}

// GetAttempt returns one attempt of a task the actor may read.
func GetAttempt(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, task ids.TaskID, id ids.AttemptID) (lifecycle.Attempt, error) {
	var attempt lifecycle.Attempt
	err := read(ctx, pool, actor, func(r *reader) error {
		if _, err := r.task(ctx, task); err != nil {
			return err
		}
		loaded, err := store.GetAttempt(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		if loaded.Task != task {
			return ErrNotFound
		}
		attempt = loaded
		return nil
	})
	return attempt, err
}

// GetRevision returns one revision of a task the actor may read, with its
// reviews.
func GetRevision(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, task ids.TaskID, id ids.RevisionID) (RevisionView, error) {
	var view RevisionView
	err := read(ctx, pool, actor, func(r *reader) error {
		if _, err := r.task(ctx, task); err != nil {
			return err
		}
		revision, err := store.GetRevision(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		if revision.Task != task {
			return ErrNotFound
		}
		reviews, err := store.ListReviewsByRevision(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		view = RevisionView{Revision: revision, Reviews: reviews}
		return nil
	})
	return view, err
}

// GetAttention returns one attention request on a task the actor may read.
func GetAttention(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.AttentionID) (lifecycle.AttentionRequest, error) {
	var request lifecycle.AttentionRequest
	err := read(ctx, pool, actor, func(r *reader) error {
		loaded, err := store.GetAttention(ctx, r.uow, r.principal.Tenant, id)
		if err != nil {
			return err
		}
		if _, err := r.task(ctx, loaded.Task); err != nil {
			return err
		}
		request = loaded
		return nil
	})
	return request, err
}

// ListAttention returns the open attention requests on tasks the actor may
// read, oldest first.
func ListAttention(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext) ([]lifecycle.AttentionRequest, error) {
	var requests []lifecycle.AttentionRequest
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		requests, err = store.ListOpenAttention(ctx, r.uow, r.principal.Tenant, r.owner())
		return err
	})
	return requests, err
}

// ListEvents returns the next page of events after cursor: those of task when
// task is non-zero, otherwise the tenant stream. A principal without the
// tenant-reader permission sees only events of tasks it owns. A cursor from
// another generation, or beyond the head of the log, is reset.
func ListEvents(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, task ids.TaskID, cursor EventCursor, limit int) (EventPage, error) {
	if limit < 1 || limit > MaxEventPage {
		return EventPage{}, fmt.Errorf("%w: page size must be 1 to %d", ErrInvalidCommand, MaxEventPage)
	}
	if cursor.Position < 0 {
		return EventPage{}, fmt.Errorf("%w: malformed cursor", ErrInvalidCommand)
	}
	var page EventPage
	err := read(ctx, pool, actor, func(r *reader) error {
		if !task.IsZero() {
			if _, err := r.task(ctx, task); err != nil {
				return err
			}
		}
		generation, err := store.StreamGeneration(ctx, r.uow)
		if err != nil {
			return err
		}
		head, err := store.StreamHead(ctx, r.uow, r.principal.Tenant)
		if err != nil {
			return err
		}
		stream := StreamTag(r.principal.Tenant, task)
		fresh := cursor == EventCursor{}
		if !fresh && (cursor.Stream != stream || cursor.Generation != generation || cursor.Position > head) {
			// The client re-reads its snapshots, which reflect every event up
			// to head, and continues from there.
			page = EventPage{Next: EventCursor{Generation: generation, Position: head, Stream: stream}, Reset: true}
			return nil
		}
		events, err := store.ListStreamEvents(ctx, r.uow, r.principal.Tenant, task, r.owner(), cursor.Position, limit)
		if err != nil {
			return err
		}
		page = EventPage{Events: events, Next: EventCursor{Generation: generation, Position: cursor.Position, Stream: stream}}
		if len(events) > 0 {
			page.Next.Position = events[len(events)-1].Position
		}
		return nil
	})
	return page, err
}
