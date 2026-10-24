package adapterkit

import (
	"testing"
	"time"
)

func TestBackoffGrowsWithFullJitterUpToItsCap(t *testing.T) {
	b := newBackoff(100*time.Millisecond, time.Second)
	for attempt := range 12 {
		ceiling := min(time.Second, 100*time.Millisecond<<attempt)
		if delay := b.next(); delay < 0 || delay > ceiling {
			t.Fatalf("attempt %d delay = %v, want within [0, %v]", attempt, delay, ceiling)
		}
	}
	b.reset()
	if delay := b.next(); delay > 100*time.Millisecond {
		t.Fatalf("delay after reset = %v, want at most the base", delay)
	}
}
