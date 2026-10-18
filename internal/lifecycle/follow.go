package lifecycle

import "slices"

// activeAttemptStates are the states in which an Attempt's provider work may
// be live and an unknown outcome must stop it for reconciliation.
var activeAttemptStates = []AttemptState{
	AttemptProvisioning, AttemptStarting, AttemptExecuting, AttemptWaitingInput, AttemptFinalizing,
}

// AttemptFollowsOperation returns the Attempt transition an operation's state
// implies, and false when it implies none. An uncertain operation sends a live
// Attempt to reconciliation; a reconciled operation resolves a reconciling
// Attempt: a provider that accepted or completed the work leaves it executing,
// one that failed or rejected it fails it. A definite failure or rejection of
// a live Attempt's operation fails it directly. Reconciliation never returns
// an Attempt to a pre-dispatch state.
func AttemptFollowsOperation(operation OperationState, attempt AttemptState) (AttemptEvent, AttemptState, bool) {
	failed := operation == OperationFailed || operation == OperationRejected
	switch {
	case operation == OperationUncertain && slices.Contains(activeAttemptStates, attempt):
		return AttemptOutcomeUnknown, AttemptReconciling, true
	case failed && slices.Contains(activeAttemptStates, attempt):
		return AttemptFail, AttemptFailed, true
	case attempt != AttemptReconciling:
		return "", "", false
	case operation == OperationAccepted || operation == OperationSucceeded:
		return AttemptReconciled, AttemptExecuting, true
	case failed:
		return AttemptReconciled, AttemptFailed, true
	}
	return "", "", false
}

// TaskFollowsAttempt returns the Task transition an Attempt's state implies,
// and false when it implies none: a failed Attempt fails the running Task.
func TaskFollowsAttempt(attempt AttemptState, task TaskState) (TaskEvent, bool) {
	if attempt == AttemptFailed && task == TaskRunning {
		return TaskExecutionFailed, true
	}
	return "", false
}
