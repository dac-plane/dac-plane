package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ResultRevision is one immutable proposed result of a Task: the committed
// artifacts an attempt produced. A rework produces a new revision that names
// the one it follows; no revision is ever changed.
type ResultRevision struct {
	ID        ids.RevisionID   `json:"revision_id"`
	Tenant    ids.TenantID     `json:"tenant_id"`
	Task      ids.TaskID       `json:"task_id"`
	Attempt   ids.AttemptID    `json:"attempt_id"`
	Number    uint32           `json:"number"`
	Previous  ids.RevisionID   `json:"previous_revision_id,omitzero"`
	Artifacts []ids.ArtifactID `json:"artifact_ids"`
	// Verifications is absent only on legacy revisions. New application
	// finalization supplies an immutable integrity record for every artifact.
	Verifications []ids.VerificationID `json:"verification_ids,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
}

// Validate reports whether the revision is complete and its lineage shape is
// consistent: the first revision has no predecessor, every later one has one.
func (r ResultRevision) Validate() error {
	if r.ID.IsZero() || r.Tenant.IsZero() || r.Task.IsZero() || r.Attempt.IsZero() || r.Number == 0 || r.CreatedAt.IsZero() {
		return fmt.Errorf("%w: result revision requires identifiers, a positive number and a time", ErrInvalid)
	}
	if (r.Number == 1) != r.Previous.IsZero() {
		return fmt.Errorf("%w: only the first revision has no predecessor", ErrInvalid)
	}
	if len(r.Artifacts) == 0 {
		return fmt.Errorf("%w: result revision requires committed artifacts", ErrInvalid)
	}
	seen := make(map[ids.ArtifactID]bool, len(r.Artifacts))
	for _, artifact := range r.Artifacts {
		if artifact.IsZero() || seen[artifact] {
			return fmt.Errorf("%w: result revision artifacts must be set and distinct", ErrInvalid)
		}
		seen[artifact] = true
	}
	verified := map[ids.VerificationID]bool{}
	if len(r.Verifications) > 0 && len(r.Verifications) != len(r.Artifacts) {
		return fmt.Errorf("%w: verification must cover every artifact", ErrInvalid)
	}
	for _, id := range r.Verifications {
		if id.IsZero() || verified[id] {
			return fmt.Errorf("%w: verification IDs must be set and distinct", ErrInvalid)
		}
		verified[id] = true
	}
	return nil
}

// NewResultRevision creates a revision from committed artifacts. It fails
// unless every artifact is a valid committed record of the same tenant and
// task, and previous, when given, is this task's preceding revision.
func NewResultRevision(id ids.RevisionID, attempt ids.AttemptID, artifacts []Artifact, previous *ResultRevision, at time.Time) (ResultRevision, error) {
	if len(artifacts) == 0 {
		return ResultRevision{}, fmt.Errorf("%w: result revision requires committed artifacts", ErrInvalid)
	}
	first := artifacts[0]
	revision := ResultRevision{ID: id, Tenant: first.Tenant, Task: first.Task, Attempt: attempt, Number: 1, CreatedAt: at}
	for _, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return ResultRevision{}, err
		}
		if artifact.Tenant != revision.Tenant || artifact.Task != revision.Task {
			return ResultRevision{}, fmt.Errorf("%w: every artifact must belong to the revision's task", ErrInvalid)
		}
		revision.Artifacts = append(revision.Artifacts, artifact.Artifact)
	}
	if previous != nil {
		if err := previous.Validate(); err != nil {
			return ResultRevision{}, err
		}
		if previous.Tenant != revision.Tenant || previous.Task != revision.Task {
			return ResultRevision{}, fmt.Errorf("%w: previous revision belongs to another task", ErrInvalid)
		}
		revision.Number, revision.Previous = previous.Number+1, previous.ID
	}
	return revision, revision.Validate()
}

// Disposition is a reviewer's judgement of a revision.
type Disposition string

const (
	DispositionAccept         Disposition = "accept"
	DispositionRequestChanges Disposition = "request_changes"
	DispositionReject         Disposition = "reject"
)

// Dispositions lists every Disposition.
func Dispositions() []Disposition {
	return []Disposition{DispositionAccept, DispositionRequestChanges, DispositionReject}
}

// taskEvent is the Task event a disposition causes.
func (d Disposition) taskEvent() TaskEvent {
	switch d {
	case DispositionAccept:
		return TaskAccept
	case DispositionRequestChanges:
		return TaskRequestChanges
	default:
		return TaskReject
	}
}

// ResultReview is an append-only judgement of one exact revision by one
// principal, made on the task version the reviewer saw. A review is never
// edited; a later judgement is a new review.
type ResultReview struct {
	ID          ids.ReviewID    `json:"review_id"`
	Tenant      ids.TenantID    `json:"tenant_id"`
	Task        ids.TaskID      `json:"task_id"`
	Revision    ids.RevisionID  `json:"revision_id"`
	Reviewer    ids.PrincipalID `json:"reviewer_id"`
	Disposition Disposition     `json:"disposition"`
	TaskVersion uint64          `json:"task_version"`
	ReviewedAt  time.Time       `json:"reviewed_at"`
}

// Validate reports whether the review is complete.
func (r ResultReview) Validate() error {
	if r.ID.IsZero() || r.Tenant.IsZero() || r.Task.IsZero() || r.Revision.IsZero() || r.Reviewer.IsZero() ||
		r.TaskVersion == 0 || r.ReviewedAt.IsZero() {
		return fmt.Errorf("%w: review requires identifiers, the task version and a time", ErrInvalid)
	}
	if !slices.Contains(Dispositions(), r.Disposition) {
		return fmt.Errorf("%w: unknown review disposition", ErrInvalid)
	}
	return nil
}

// ApplyReview makes review the effective disposition of the task. current is
// the task's revision under review, which the review must name exactly. The
// reviewer must be the task owner or one of reviewers. The disposition is
// applied at the task version the reviewer saw, so of two concurrent reviews
// only the first takes effect; the other is a version conflict. Requesting
// changes queues new work and leaves the reviewed revision untouched.
func ApplyReview(task Task, current ResultRevision, review ResultReview, reviewers []ids.PrincipalID) (Task, Change, error) {
	change := Change{
		Tenant: task.Tenant, Record: RecordTask, RecordID: task.ID.Untyped(),
		Event: string(review.Disposition.taskEvent()), From: string(task.State), Version: task.Version,
	}
	if err := review.Validate(); err != nil {
		return task, change, err
	}
	if err := current.Validate(); err != nil {
		return task, change, err
	}
	switch {
	case review.Tenant != task.Tenant || current.Tenant != task.Tenant:
		return task, change, reject(change, RejectCrossTenant)
	case review.Task != task.ID || current.Task != task.ID:
		return task, change, reject(change, RejectWrongRecord)
	case review.Revision != current.ID:
		return task, change, reject(change, RejectNotCurrentRevision)
	case review.Reviewer != task.Owner && !slices.Contains(reviewers, review.Reviewer):
		return task, change, reject(change, RejectUnauthorizedActor)
	}
	return task.Apply(review.Disposition.taskEvent(), review.TaskVersion, "", review.ReviewedAt)
}
