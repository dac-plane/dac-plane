package application

import (
	"testing"

	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// A task a worker moved between the position query and its read is left out of
// a filtered page rather than returned in another state.
func TestAFilteredPageKeepsOnlyTasksStillInThatState(t *testing.T) {
	running := lifecycle.Task{State: lifecycle.TaskRunning}
	succeeded := lifecycle.Task{State: lifecycle.TaskSucceeded}
	for name, tc := range map[string]struct {
		task  lifecycle.Task
		state lifecycle.TaskState
		want  bool
	}{
		"no filter keeps any task":       {succeeded, "", true},
		"still running":                  {running, lifecycle.TaskRunning, true},
		"moved on to succeeded":          {succeeded, lifecycle.TaskRunning, false},
		"moved into the requested state": {succeeded, lifecycle.TaskSucceeded, true},
	} {
		if got := matchesState(tc.task, tc.state); got != tc.want {
			t.Errorf("%s: matchesState() = %v, want %v", name, got, tc.want)
		}
	}
}
