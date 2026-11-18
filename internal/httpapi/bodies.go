package httpapi

import (
	"fmt"
	"slices"
	"time"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

// Request and response bodies of Surface API v1. Each mirrors a schema under
// contracts/jsonschema/surface/; requests are decoded strictly and validated
// before any command runs. Shape checks only: the application layer still
// owns every lifecycle rule.

// SubmitTaskRequest is the body of POST /tasks.
type SubmitTaskRequest struct {
	// Agent, when set, delegates the task to that agent; the providers then
	// default to its current revision's.
	Agent               ids.AgentID      `json:"agent_id,omitzero"`
	Provider            ids.ProviderID   `json:"provider_id,omitzero"`
	EnvironmentProvider ids.ProviderID   `json:"environment_provider_id,omitzero"`
	Objective           string           `json:"objective"`
	AcceptanceCriteria  []string         `json:"acceptance_criteria"`
	Demand              contracts.Demand `json:"demand,omitempty"`
	Deadline            time.Time        `json:"deadline"`
	Unit                lifecycle.Unit   `json:"unit"`
	Ceiling             uint64           `json:"ceiling"`
	Amount              uint64           `json:"amount"`
	Spend               bool             `json:"spend,omitempty"`
}

// Validate reports whether the body is well formed.
func (r SubmitTaskRequest) Validate() error {
	brief := lifecycle.ExecutionBrief{Objective: r.Objective, AcceptanceCriteria: r.AcceptanceCriteria}
	switch {
	case r.Provider.IsZero() && r.Agent.IsZero():
		return fmt.Errorf("%w: provider_id is required without agent_id", application.ErrInvalidCommand)
	case r.EnvironmentProvider.IsZero() && r.Agent.IsZero():
		return fmt.Errorf("%w: environment_provider_id is required without agent_id", application.ErrInvalidCommand)
	case brief.Validate() != nil:
		return fmt.Errorf("%w: a valid objective and acceptance criteria are required", application.ErrInvalidCommand)
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
	// Answer is the text AnswerDigest is the digest of (ADR-0026).
	Answer lifecycle.Answer `json:"answer,omitempty"`
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
	if r.Answer != nil {
		if r.Kind != lifecycle.DecisionAnswer {
			return fmt.Errorf("%w: only an answer carries answer", application.ErrInvalidCommand)
		}
		if err := r.Answer.VerifyDigest(r.AnswerDigest); err != nil {
			return fmt.Errorf("%w: answer: %w", application.ErrInvalidCommand, err)
		}
	}
	return nil
}

// CancelRequest is the body of POST /tasks/{task}/cancel. It is empty; the
// task version travels in If-Match.
type CancelRequest struct{}

// Validate reports whether the body is well formed.
func (CancelRequest) Validate() error { return nil }

// EmptyCommandRequest is the body of an administrative command whose payload
// is carried entirely by its path and headers.
type EmptyCommandRequest struct{}

// Validate reports whether the body is well formed.
func (EmptyCommandRequest) Validate() error { return nil }

// CreateMigrationRequest is the body of POST /migration-passes.
type CreateMigrationRequest struct {
	From ids.ProviderID `json:"from_provider_id"`
	To   ids.ProviderID `json:"to_provider_id"`
}

// Validate reports whether the body is well formed.
func (r CreateMigrationRequest) Validate() error {
	if r.From.IsZero() || r.To.IsZero() || r.From == r.To {
		return fmt.Errorf("%w: migration requires distinct source and target providers", application.ErrInvalidCommand)
	}
	return nil
}

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
	Task                lifecycle.Task              `json:"task"`
	Attempt             lifecycle.Attempt           `json:"attempt"`
	Operation           lifecycle.ProviderOperation `json:"operation"`
	Budget              lifecycle.Budget            `json:"budget"`
	Reservation         lifecycle.Reservation       `json:"reservation"`
	Challenge           *lifecycle.SpendChallenge   `json:"spend_challenge,omitempty"`
	Receipt             *lifecycle.SpendReceipt     `json:"spend_receipt,omitempty"`
	Provider            ids.ProviderID              `json:"provider_id"`
	EnvironmentProvider *ids.ProviderID             `json:"environment_provider_id,omitempty"`
	Mode                lifecycle.HarnessMode       `json:"harness_mode"`
	Demand              contracts.Demand            `json:"demand"`
	Deadline            time.Time                   `json:"deadline"`
	Unit                lifecycle.Unit              `json:"unit"`
	Amount              uint64                      `json:"amount"`
	Dispatch            *DispatchBody               `json:"dispatch,omitempty"`
}

// DispatchBody is the state of the operation's durable dispatch. Provider
// error text is not exposed.
type DispatchBody struct {
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
}

func submissionBody(s application.Submission) SubmissionBody {
	var environmentProvider *ids.ProviderID
	if !s.EnvironmentProvider.IsZero() {
		environmentProvider = &s.EnvironmentProvider
	}
	body := SubmissionBody{
		Task: s.Task, Attempt: s.Attempt, Operation: s.Operation, Budget: s.Budget, Reservation: s.Reservation,
		Challenge: s.Challenge, Receipt: s.Receipt, Provider: s.Provider, EnvironmentProvider: environmentProvider, Mode: s.Mode, Demand: s.Demand,
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

// MigrationPreviewBody is the qualification snapshot required by execute.
type MigrationPreviewBody struct {
	FromVersion uint64    `json:"from_version"`
	ToVersion   uint64    `json:"to_version"`
	PreviewedAt time.Time `json:"previewed_at"`
}

// MigrationBody is the public projection of migration administration. The
// worker's maintenance claim is deliberately not part of the Surface API.
type MigrationBody struct {
	Migration lifecycle.MigrationPass `json:"migration"`
	Preview   *MigrationPreviewBody   `json:"preview,omitempty"`
}

func migrationBody(view application.MigrationView) MigrationBody {
	body := MigrationBody{Migration: view.Pass}
	if !view.Preview.At.IsZero() {
		body.Preview = &MigrationPreviewBody{
			FromVersion: view.Preview.FromVersion,
			ToVersion:   view.Preview.ToVersion,
			PreviewedAt: view.Preview.At.UTC(),
		}
	}
	return body
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

// AgentRequest is the body of POST /agents and PUT /agents/{agent}: the whole
// profile, since a revision replaces it as a whole (ADR-0020).
type AgentRequest struct {
	Name                string         `json:"name"`
	Role                string         `json:"role"`
	Instructions        string         `json:"instructions,omitempty"`
	Provider            ids.ProviderID `json:"provider_id"`
	EnvironmentProvider ids.ProviderID `json:"environment_provider_id"`
}

// Validate reports whether the body is a profile within its bounds.
func (r AgentRequest) Validate() error {
	if err := r.profile().Validate(); err != nil {
		return fmt.Errorf("%w: %w", application.ErrInvalidCommand, err)
	}
	return nil
}

func (r AgentRequest) profile() lifecycle.AgentProfile {
	return lifecycle.AgentProfile{
		Name: r.Name, Role: r.Role, Instructions: r.Instructions,
		Provider: r.Provider, EnvironmentProvider: r.EnvironmentProvider,
	}
}

// AgentListBody is a tenant's agents.
type AgentListBody struct {
	Agents []lifecycle.Agent `json:"agents"`
}

// ConnectionRequest is the body of POST /connections: the connector provider
// that serves the connection and the owner's name for it. Nothing in it could
// carry a credential; consent happens at the connector.
type ConnectionRequest struct {
	Provider ids.ProviderID `json:"provider_id"`
	Name     string         `json:"name"`
}

// Validate reports whether the body names a provider and a valid name.
func (r ConnectionRequest) Validate() error {
	if r.Provider.IsZero() {
		return fmt.Errorf("%w: a connector provider is required", application.ErrInvalidCommand)
	}
	if err := lifecycle.ValidateConnectionName(r.Name); err != nil {
		return fmt.Errorf("%w: %w", application.ErrInvalidCommand, err)
	}
	return nil
}

// ConnectionCreatedBody answers POST /connections: the connection and, when
// the connector answered, the URL its owner opens to consent.
type ConnectionCreatedBody struct {
	Connection    lifecycle.Connection            `json:"connection"`
	Authorization *contracts.AuthorizationStarted `json:"authorization,omitempty"`
}

// ConnectionListBody is a tenant's connections.
type ConnectionListBody struct {
	Connections []lifecycle.Connection `json:"connections"`
}

// ConnectionGrantRequest is the body of POST /connections/{connection}/grants.
// It names exactly one of an agent and a task.
type ConnectionGrantRequest struct {
	Agent      ids.AgentID         `json:"agent_id,omitzero"`
	Task       ids.TaskID          `json:"task_id,omitzero"`
	Tools      []string            `json:"tools"`
	Permission identity.Permission `json:"permission"`
	CallLimit  uint32              `json:"call_limit"`
	ExpiresAt  time.Time           `json:"expires_at"`
}

// Validate reports whether the terms are well formed. Whether the agent or
// task and the tools exist is the command's to check.
func (r ConnectionGrantRequest) Validate() error {
	if r.Agent.IsZero() == r.Task.IsZero() {
		return fmt.Errorf("%w: a grant names exactly one agent or task", application.ErrInvalidCommand)
	}
	if len(r.Tools) == 0 || len(r.Tools) > lifecycle.MaxGrantTools {
		return fmt.Errorf("%w: a grant names 1 to %d tools", application.ErrInvalidCommand, lifecycle.MaxGrantTools)
	}
	seen := map[string]bool{}
	for _, tool := range r.Tools {
		if err := lifecycle.ValidateToolName(tool); err != nil || seen[tool] {
			return fmt.Errorf("%w: grant tools are distinct tool names", application.ErrInvalidCommand)
		}
		seen[tool] = true
	}
	if r.Permission != identity.PermissionAsk && r.Permission != identity.PermissionAlways {
		return fmt.Errorf("%w: a grant's permission is ask or always", application.ErrInvalidCommand)
	}
	if r.CallLimit == 0 || r.CallLimit > lifecycle.MaxGrantCallLimit {
		return fmt.Errorf("%w: a grant's call limit is 1 to %d", application.ErrInvalidCommand, lifecycle.MaxGrantCallLimit)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: a grant expires", application.ErrInvalidCommand)
	}
	return nil
}

func (r ConnectionGrantRequest) terms() lifecycle.ConnectionGrantTerms {
	return lifecycle.ConnectionGrantTerms{Agent: r.Agent, Task: r.Task, Tools: r.Tools, Permission: r.Permission, CallLimit: r.CallLimit, ExpiresAt: r.ExpiresAt}
}

// ConnectionGrantListBody is a connection's grants.
type ConnectionGrantListBody struct {
	Grants []lifecycle.ConnectionGrant `json:"grants"`
}

// CompleteConnectionRequest carries the proof returned only to the consent browser.
type CompleteConnectionRequest struct {
	Confirmation string `json:"confirmation"`
}

func (r CompleteConnectionRequest) Validate() error {
	if err := contracts.ValidateConfirmation(r.Confirmation); err != nil {
		return fmt.Errorf("%w: %v", application.ErrInvalidCommand, err)
	}
	return nil
}
