package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// TaskState is the product lifecycle state of a Task.
type TaskState string

const (
	TaskDraft          TaskState = "draft"
	TaskQueued         TaskState = "queued"
	TaskRunning        TaskState = "running"
	TaskWaitingInput   TaskState = "waiting_input"
	TaskVerifying      TaskState = "verifying"
	TaskReadyForReview TaskState = "ready_for_review"
	TaskCancelling     TaskState = "cancelling"
	TaskSucceeded      TaskState = "succeeded"
	TaskFailed         TaskState = "failed"
	TaskRejected       TaskState = "rejected"
	TaskCancelled      TaskState = "cancelled"
)

// TaskStates lists every TaskState.
func TaskStates() []TaskState {
	return []TaskState{
		TaskDraft, TaskQueued, TaskRunning, TaskWaitingInput, TaskVerifying, TaskReadyForReview,
		TaskCancelling, TaskSucceeded, TaskFailed, TaskRejected, TaskCancelled,
	}
}

// TaskEvent is a command or accepted observation that may change a Task.
type TaskEvent string

const (
	TaskSubmit             TaskEvent = "submit"
	TaskDispatch           TaskEvent = "dispatch"
	TaskAwaitInput         TaskEvent = "await_input"
	TaskInputResolved      TaskEvent = "input_resolved"
	TaskInputUnresolved    TaskEvent = "input_unresolved"
	TaskProposeResult      TaskEvent = "propose_result"
	TaskExecutionFailed    TaskEvent = "execution_failed"
	TaskRetry              TaskEvent = "retry"
	TaskVerificationPassed TaskEvent = "verification_passed"
	TaskVerificationFailed TaskEvent = "verification_failed"
	TaskAccept             TaskEvent = "accept"
	TaskRequestChanges     TaskEvent = "request_changes"
	TaskReject             TaskEvent = "reject"
	TaskCancel             TaskEvent = "cancel"
	TaskStopConfirmed      TaskEvent = "stop_confirmed"
)

// TaskEvents lists every TaskEvent.
func TaskEvents() []TaskEvent {
	return []TaskEvent{
		TaskSubmit, TaskDispatch, TaskAwaitInput, TaskInputResolved, TaskInputUnresolved,
		TaskProposeResult, TaskExecutionFailed, TaskRetry, TaskVerificationPassed,
		TaskVerificationFailed, TaskAccept, TaskRequestChanges, TaskReject, TaskCancel,
		TaskStopConfirmed,
	}
}

// taskMachine follows the historical specification §11 with two changes:
// waiting for input is an explicit condition, and a review rejection ends in
// rejected, which is distinct from failed execution.
var taskMachine = newMachine(TaskStates(), TaskEvents(),
	[]TaskState{TaskSucceeded, TaskFailed, TaskRejected, TaskCancelled},
	map[TaskState]map[TaskEvent][]TaskState{
		TaskDraft: {
			TaskSubmit: {TaskQueued},
			TaskCancel: {TaskCancelled},
		},
		TaskQueued: {
			TaskDispatch: {TaskRunning},
			TaskCancel:   {TaskCancelled},
		},
		TaskRunning: {
			TaskAwaitInput:      {TaskWaitingInput},
			TaskProposeResult:   {TaskVerifying},
			TaskExecutionFailed: {TaskFailed},
			TaskRetry:           {TaskQueued},
			TaskCancel:          {TaskCancelling},
		},
		// A resolved request (answered or denied) resumes work; a denial blocks
		// only the denied action. An unresolved one resumes, fails or cancels
		// according to the request's policy.
		TaskWaitingInput: {
			TaskInputResolved:   {TaskRunning},
			TaskInputUnresolved: {TaskRunning, TaskFailed, TaskCancelling},
			TaskCancel:          {TaskCancelling},
		},
		// Passing verification reaches review, or success only under an
		// explicit auto-accept policy.
		TaskVerifying: {
			TaskVerificationPassed: {TaskReadyForReview, TaskSucceeded},
			TaskVerificationFailed: {TaskRunning, TaskFailed},
			TaskCancel:             {TaskCancelling},
		},
		TaskReadyForReview: {
			TaskAccept:         {TaskSucceeded},
			TaskRequestChanges: {TaskQueued},
			TaskReject:         {TaskRejected},
			TaskCancel:         {TaskCancelled},
		},
		// Cancellation is intent until the attempt's stop is confirmed. No
		// result or verification event is legal here, so a late result can never
		// turn a cancelling task into a success.
		TaskCancelling: {
			TaskStopConfirmed: {TaskCancelled},
		},
	},
)

// Task is the delegated unit of work a principal owns.
type Task struct {
	ID        ids.TaskID      `json:"task_id"`
	Tenant    ids.TenantID    `json:"tenant_id"`
	Owner     ids.PrincipalID `json:"owner_id"`
	State     TaskState       `json:"state"`
	Version   uint64          `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// NewTask returns a draft Task at version 1.
func NewTask(id ids.TaskID, tenant ids.TenantID, owner ids.PrincipalID, at time.Time) (Task, error) {
	task := Task{ID: id, Tenant: tenant, Owner: owner, State: TaskDraft, Version: 1, CreatedAt: at, UpdatedAt: at}
	return task, task.Validate()
}

// Validate reports whether the record is complete.
func (t Task) Validate() error {
	if t.ID.IsZero() || t.Tenant.IsZero() || t.Owner.IsZero() || t.Version == 0 {
		return fmt.Errorf("%w: task requires identifiers and a positive version", ErrInvalid)
	}
	if !slices.Contains(TaskStates(), t.State) {
		return fmt.Errorf("%w: unknown task state", ErrInvalid)
	}
	if t.CreatedAt.IsZero() || t.UpdatedAt.Before(t.CreatedAt) {
		return fmt.Errorf("%w: task timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Apply performs event if the task is still at expectedVersion. target selects
// among several legal targets and may be empty when there is only one.
func (t Task) Apply(event TaskEvent, expectedVersion uint64, target TaskState, at time.Time) (Task, Change, error) {
	change := Change{
		Tenant: t.Tenant, Record: RecordTask, RecordID: t.ID.Untyped(),
		Event: string(event), From: string(t.State), Version: t.Version,
	}
	next, code := taskMachine.next(t.State, t.Version, expectedVersion, event, target)
	if code != "" {
		return t, change, reject(change, code)
	}
	t.State = next
	t.Version++
	t.UpdatedAt = at
	change.To = string(next)
	change.Version = t.Version
	return t, change, nil
}
