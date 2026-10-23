package httpapi

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Request and response bodies of Surface API v1. Each mirrors a schema under
// contracts/jsonschema/surface/; requests are decoded strictly and validated
// before any command runs. Shape checks only: the application layer still
// owns every lifecycle rule.

// SubmitTaskRequest is the body of POST /tasks.
type SubmitTaskRequest struct {
	Provider ids.ProviderID   `json:"provider_id"`
	Demand   contracts.Demand `json:"demand,omitempty"`
	Deadline time.Time        `json:"deadline"`
	Unit     lifecycle.Unit   `json:"unit"`
	Ceiling  uint64           `json:"ceiling"`
	Amount   uint64           `json:"amount"`
	Spend    bool             `json:"spend,omitempty"`
}

// Validate reports whether the body is well formed.
func (r SubmitTaskRequest) Validate() error {
	switch {
	case r.Provider.IsZero():
		return fmt.Errorf("%w: provider_id is required", application.ErrInvalidCommand)
	case r.Demand != "" && !slices.Contains(contracts.Demands(), r.Demand):
		return fmt.Errorf("%w: unknown demand", application.ErrInvalidCommand)
	case r.Deadline.IsZero():
		return fmt.Errorf("%w: deadline is required", application.ErrInvalidCommand)
	case r.Unit.Validate() != nil:
		return fmt.Errorf("%w: malformed unit", application.ErrInvalidCommand)
	case r.Ceiling == 0 || r.Ceiling > lifecycle.MaxAmount || r.Amount == 0 || r.Amount > lifecycle.MaxAmount:
		return fmt.Errorf("%w: ceiling and amount must be 1 to %d", application.ErrInvalidCommand, lifecycle.MaxAmount)
	}
	return nil
}

// SpendAssertionRequest is the body of POST /tasks/{task}/spend-assertions.
// Exactly one of SubmissionKey and ReviewKey names the governed command being
// completed: the Idempotency-Key of the submission, or of the review that
// requested changes.
type SpendAssertionRequest struct {
	SubmissionKey string                   `json:"submission_key,omitempty"`
	ReviewKey     string                   `json:"review_key,omitempty"`
	Assertion     lifecycle.SpendAssertion `json:"assertion"`
}

// Validate reports whether the body is well formed.
func (r SpendAssertionRequest) Validate() error {
	if (r.SubmissionKey == "") == (r.ReviewKey == "") {
		return fmt.Errorf("%w: exactly one of submission_key and review_key is required", application.ErrInvalidCommand)
	}
	if r.Assertion.Challenge.IsZero() || r.Assertion.AssertedAt.IsZero() || r.Assertion.Proof.Validate() != nil {
		return fmt.Errorf("%w: the assertion must name its challenge, proof and time", application.ErrInvalidCommand)
	}
	return nil
}

// DecisionRequest is the body of POST /attention/{attention}/decisions.
type DecisionRequest struct {
	Kind         lifecycle.DecisionKind `json:"kind"`
	AnswerDigest identity.Digest        `json:"answer_digest,omitempty"`
}

// Validate reports whether the body is well formed.
func (r DecisionRequest) Validate() error {
	if !slices.Contains(lifecycle.DecisionKinds(), r.Kind) {
		return fmt.Errorf("%w: unknown decision kind", application.ErrInvalidCommand)
	}
	if (r.Kind == lifecycle.DecisionAnswer) != (r.AnswerDigest != "") {
		return fmt.Errorf("%w: an answer, and only an answer, carries answer_digest", application.ErrInvalidCommand)
	}
	if r.AnswerDigest != "" && r.AnswerDigest.Validate() != nil {
		return fmt.Errorf("%w: malformed answer_digest", application.ErrInvalidCommand)
	}
	return nil
}

// CancelRequest is the body of POST /tasks/{task}/cancel. It is empty; the
// task version travels in If-Match.
type CancelRequest struct{}

// Validate reports whether the body is well formed.
func (CancelRequest) Validate() error { return nil }

// ReviewRequest is the body of POST /tasks/{task}/reviews.
type ReviewRequest struct {
	Revision    ids.RevisionID           `json:"revision_id"`
	Disposition lifecycle.Disposition    `json:"disposition"`
	Rework      *application.ReworkTerms `json:"rework,omitempty"`
}

// Validate reports whether the body is well formed.
func (r ReviewRequest) Validate() error {
	if r.Revision.IsZero() {
		return fmt.Errorf("%w: revision_id is required", application.ErrInvalidCommand)
	}
	if !slices.Contains(lifecycle.Dispositions(), r.Disposition) {
		return fmt.Errorf("%w: unknown disposition", application.ErrInvalidCommand)
	}
	if (r.Disposition == lifecycle.DispositionRequestChanges) != (r.Rework != nil) {
		return fmt.Errorf("%w: rework terms accompany request_changes and nothing else", application.ErrInvalidCommand)
	}
	return nil
}

// SubmissionBody is a submitted task as it stands now.
type SubmissionBody struct {
	Task        lifecycle.Task              `json:"task"`
	Attempt     lifecycle.Attempt           `json:"attempt"`
	Operation   lifecycle.ProviderOperation `json:"operation"`
	Budget      lifecycle.Budget            `json:"budget"`
	Reservation lifecycle.Reservation       `json:"reservation"`
	Challenge   *lifecycle.SpendChallenge   `json:"spend_challenge,omitempty"`
	Receipt     *lifecycle.SpendReceipt     `json:"spend_receipt,omitempty"`
	Provider    ids.ProviderID              `json:"provider_id"`
	Mode        lifecycle.HarnessMode       `json:"harness_mode"`
	Demand      contracts.Demand            `json:"demand"`
	Deadline    time.Time                   `json:"deadline"`
	Unit        lifecycle.Unit              `json:"unit"`
	Amount      uint64                      `json:"amount"`
	Dispatch    *DispatchBody               `json:"dispatch,omitempty"`
}

// DispatchBody is the state of the operation's durable dispatch. Provider
// error text is not exposed.
type DispatchBody struct {
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
}

func submissionBody(s application.Submission) SubmissionBody {
	body := SubmissionBody{
		Task: s.Task, Attempt: s.Attempt, Operation: s.Operation, Budget: s.Budget, Reservation: s.Reservation,
		Challenge: s.Challenge, Receipt: s.Receipt, Provider: s.Provider, Mode: s.Mode, Demand: s.Demand,
		Deadline: s.Deadline.UTC(), Unit: s.Unit, Amount: s.Amount,
	}
	if s.Dispatch != nil {
		body.Dispatch = &DispatchBody{Status: s.Dispatch.Status, Attempts: s.Dispatch.Attempts}
	}
	return body
}

// TaskBody is a task as a surface reads it.
type TaskBody struct {
	Task          lifecycle.Task               `json:"task"`
	Attempt       *lifecycle.Attempt           `json:"attempt,omitempty"`
	OpenAttention []lifecycle.AttentionRequest `json:"open_attention"`
	Challenge     *lifecycle.SpendChallenge    `json:"spend_challenge,omitempty"`
	Revisions     []RevisionBody               `json:"revisions"`
}

// RevisionBody is one result revision and its reviews.
type RevisionBody struct {
	Revision lifecycle.ResultRevision `json:"revision"`
	Reviews  []lifecycle.ResultReview `json:"reviews"`
}

func revisionBody(v application.RevisionView) RevisionBody {
	return RevisionBody{Revision: v.Revision, Reviews: nonNil(v.Reviews)}
}

func taskBody(v application.TaskView) TaskBody {
	body := TaskBody{Task: v.Task, Attempt: v.Attempt, OpenAttention: nonNil(v.OpenAttention), Challenge: v.Challenge, Revisions: []RevisionBody{}}
	for _, revision := range v.Revisions {
		body.Revisions = append(body.Revisions, revisionBody(revision))
	}
	return body
}

// AttentionListBody is the open attention requests a principal may read.
type AttentionListBody struct {
	Attention []lifecycle.AttentionRequest `json:"attention"`
}

// CancelBody is the result of a cancellation request.
type CancelBody struct {
	Task         string `json:"task_id"`
	State        string `json:"state"`
	Attempt      string `json:"attempt_id,omitempty"`
	AttemptState string `json:"attempt_state,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// DecisionBody is the result of a decision.
type DecisionBody struct {
	Attention string `json:"attention_id"`
	Decision  string `json:"decision_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Grant     string `json:"grant_id,omitempty"`
}

// ReviewBody is the result of a review; Rework is set when changes were
// requested and new work was scheduled.
type ReviewBody struct {
	Review lifecycle.ResultReview `json:"review"`
	Rework *SubmissionBody        `json:"rework,omitempty"`
}

// EventBody is one committed lifecycle event. RecordVersion is the record's
// own gapless revision: a consumer may discard an event whose version is not
// newer than one it already applied for the same record.
type EventBody struct {
	Cursor        string               `json:"cursor"`
	Task          *ids.TaskID          `json:"task_id,omitempty"`
	Record        lifecycle.RecordKind `json:"record_kind"`
	RecordID      ids.AnyID            `json:"record_id"`
	RecordVersion uint64               `json:"record_version"`
	Event         string               `json:"event"`
	From          string               `json:"from_state"`
	To            string               `json:"to_state,omitempty"`
	OccurredAt    time.Time            `json:"occurred_at"`
}

// eventBody renders event in the stream next belongs to.
func eventBody(next application.EventCursor, event store.StreamEvent) EventBody {
	body := EventBody{
		Cursor: formatCursor(application.EventCursor{Generation: next.Generation, Position: event.Position, Stream: next.Stream}),
		Record: event.Record, RecordID: event.RecordID, RecordVersion: event.Version,
		Event: event.Event, From: event.From, To: event.To, OccurredAt: event.OccurredAt.UTC(),
	}
	if !event.Task.IsZero() {
		task := event.Task
		body.Task = &task
	}
	return body
}

// EventPageBody is one page of events. When Reset is true the client's
// cursor could not be resumed: it re-reads its snapshots and continues from
// NextCursor.
type EventPageBody struct {
	Events     []EventBody `json:"events"`
	NextCursor string      `json:"next_cursor"`
	Reset      bool        `json:"reset"`
}

func eventPageBody(page application.EventPage) EventPageBody {
	body := EventPageBody{Events: []EventBody{}, NextCursor: formatCursor(page.Next), Reset: page.Reset}
	for _, event := range page.Events {
		body.Events = append(body.Events, eventBody(page.Next, event))
	}
	return body
}

// Problem is an RFC 9457 problem document.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
