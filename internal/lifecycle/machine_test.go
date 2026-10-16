package lifecycle

import (
	"slices"
	"testing"
)

// assertExhaustive checks every (state, event) pair of a machine: pairs in the
// table succeed for each listed target and reject any other, and every pair not
// in the table is rejected. Illegal transitions are proven, not sampled.
func assertExhaustive[S ~string, E ~string](t *testing.T, m machine[S, E]) {
	t.Helper()
	for _, from := range m.states {
		for _, event := range m.events {
			targets := m.targets(from, event)
			if len(targets) == 0 {
				_, code := m.next(from, 1, 1, event, "")
				want := RejectInvalidTransition
				if m.isTerminal(from) {
					want = RejectTerminalState
				}
				if code != want {
					t.Errorf("%s --%s--> rejected with %q, want %q", from, event, code, want)
				}
				continue
			}
			for _, to := range targets {
				if got, code := m.next(from, 1, 1, event, to); code != "" || got != to {
					t.Errorf("%s --%s--> %s = %s/%q, want applied", from, event, to, got, code)
				}
			}
			for _, other := range m.states {
				if slices.Contains(targets, other) {
					continue
				}
				if _, code := m.next(from, 1, 1, event, other); code != RejectInvalidTarget {
					t.Errorf("%s --%s--> %s rejected with %q, want invalid_target", from, event, other, code)
				}
			}
			_, code := m.next(from, 1, 1, event, "")
			if len(targets) > 1 && code != RejectAmbiguousTarget {
				t.Errorf("%s --%s--> without target = %q, want ambiguous_target", from, event, code)
			}
			if _, code := m.next(from, 2, 1, event, targets[0]); code != RejectVersionConflict {
				t.Errorf("%s --%s--> stale version = %q, want version_conflict", from, event, code)
			}
		}
	}
}

// reachable returns every state reachable from start through the table.
func reachable[S ~string, E ~string](m machine[S, E], start S) []S {
	seen := []S{start}
	for i := 0; i < len(seen); i++ {
		for _, targets := range m.table[seen[i]] {
			for _, to := range targets {
				if !slices.Contains(seen, to) {
					seen = append(seen, to)
				}
			}
		}
	}
	return seen
}

func TestMachinesAreExhaustivelyChecked(t *testing.T) {
	t.Run("task", func(t *testing.T) { assertExhaustive(t, taskMachine) })
	t.Run("attempt", func(t *testing.T) { assertExhaustive(t, attemptMachine) })
	t.Run("attention", func(t *testing.T) { assertExhaustive(t, attentionMachine) })
}

func TestEveryNonTerminalStateHasAnExit(t *testing.T) {
	check := func(name string, states, terminal []string, exits func(string) int) {
		for _, state := range states {
			if !slices.Contains(terminal, state) && exits(state) == 0 {
				t.Errorf("%s state %s is non-terminal with no transitions", name, state)
			}
		}
	}
	check("task", strs(taskMachine.states), strs(taskMachine.terminal), func(s string) int { return len(taskMachine.table[TaskState(s)]) })
	check("attempt", strs(attemptMachine.states), strs(attemptMachine.terminal), func(s string) int { return len(attemptMachine.table[AttemptState(s)]) })
	check("attention", strs(attentionMachine.states), strs(attentionMachine.terminal), func(s string) int { return len(attentionMachine.table[AttentionState(s)]) })
}

func TestReconciliationNeverReturnsToDispatch(t *testing.T) {
	for _, state := range reachable(attemptMachine, AttemptReconciling) {
		if slices.Contains([]AttemptState{AttemptAllocated, AttemptProvisioning, AttemptStarting}, state) {
			t.Errorf("reconciling can reach pre-dispatch state %s", state)
		}
	}
}

func TestCancellationOnlyEndsTerminated(t *testing.T) {
	got := reachable(attemptMachine, AttemptCancellationRequested)
	if !slices.Equal(got, []AttemptState{AttemptCancellationRequested, AttemptTerminated}) {
		t.Fatalf("cancellation_requested reaches %v, want only itself and terminated", got)
	}
}

func TestCancellingTaskCannotSucceed(t *testing.T) {
	got := reachable(taskMachine, TaskCancelling)
	if !slices.Equal(got, []TaskState{TaskCancelling, TaskCancelled}) {
		t.Fatalf("cancelling reaches %v, want only itself and cancelled", got)
	}
}

func TestRejectedIsDistinctFromFailed(t *testing.T) {
	for from, transitions := range taskMachine.table {
		for event, targets := range transitions {
			if slices.Contains(targets, TaskRejected) && (event != TaskReject || from != TaskReadyForReview) {
				t.Errorf("%s --%s--> rejected: only review rejection may reject", from, event)
			}
			if event == TaskReject && slices.Contains(targets, TaskFailed) {
				t.Errorf("%s --reject--> failed: rejection must not be recorded as failure", from)
			}
		}
	}
}

func TestNewMachineRejectsMalformedTables(t *testing.T) {
	states := []TaskState{TaskDraft, TaskQueued}
	events := []TaskEvent{TaskSubmit}
	for name, build := range map[string]func(){
		"unknown source": func() {
			newMachine(states, events, nil, map[TaskState]map[TaskEvent][]TaskState{TaskRunning: {TaskSubmit: {TaskQueued}}})
		},
		"unknown target": func() {
			newMachine(states, events, nil, map[TaskState]map[TaskEvent][]TaskState{TaskDraft: {TaskSubmit: {TaskRunning}}})
		},
		"unknown event": func() {
			newMachine(states, events, nil, map[TaskState]map[TaskEvent][]TaskState{TaskDraft: {TaskCancel: {TaskQueued}}})
		},
		"empty targets": func() {
			newMachine(states, events, nil, map[TaskState]map[TaskEvent][]TaskState{TaskDraft: {TaskSubmit: {}}})
		},
		"terminal with transitions": func() {
			newMachine(states, events, []TaskState{TaskDraft}, map[TaskState]map[TaskEvent][]TaskState{TaskDraft: {TaskSubmit: {TaskQueued}}})
		},
		"unknown terminal": func() {
			newMachine(states, events, []TaskState{TaskFailed}, map[TaskState]map[TaskEvent][]TaskState{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("newMachine() did not panic")
				}
			}()
			build()
		})
	}
}

func strs[S ~string](values []S) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
