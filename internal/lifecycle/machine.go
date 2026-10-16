// Package lifecycle defines the authoritative product records, their legal
// state transitions and the audit record of every transition decision.
//
// Every transition is a compare-and-set: the caller names the version it read,
// and a transition is applied only from that exact version. The package is pure;
// persistence commits a returned record together with its audit event (#20).
package lifecycle

import (
	"fmt"
	"slices"
)

// machine is a transition table: for each state and event, the states the
// event may lead to. More than one target means the caller must choose; no
// target means the event is illegal in that state.
type machine[S ~string, E ~string] struct {
	states   []S
	events   []E
	terminal []S
	table    map[S]map[E][]S
}

// newMachine validates the table at definition time. A malformed table is a
// programming error, so it panics during package initialization.
func newMachine[S ~string, E ~string](states []S, events []E, terminal []S, table map[S]map[E][]S) machine[S, E] {
	for from, transitions := range table {
		if !slices.Contains(states, from) {
			panic(fmt.Sprintf("lifecycle: unknown state %q", from))
		}
		if slices.Contains(terminal, from) && len(transitions) > 0 {
			panic(fmt.Sprintf("lifecycle: terminal state %q has transitions", from))
		}
		for event, targets := range transitions {
			if !slices.Contains(events, event) || len(targets) == 0 {
				panic(fmt.Sprintf("lifecycle: bad event %q from %q", event, from))
			}
			for _, to := range targets {
				if !slices.Contains(states, to) {
					panic(fmt.Sprintf("lifecycle: unknown target %q", to))
				}
			}
		}
	}
	for _, state := range terminal {
		if !slices.Contains(states, state) {
			panic(fmt.Sprintf("lifecycle: unknown terminal state %q", state))
		}
	}
	return machine[S, E]{states: states, events: events, terminal: terminal, table: table}
}

func (m machine[S, E]) targets(from S, event E) []S {
	return m.table[from][event]
}

func (m machine[S, E]) isTerminal(state S) bool {
	return slices.Contains(m.terminal, state)
}

// next checks one compare-and-set transition and returns the resulting state.
// An empty target is accepted only when the event has exactly one target.
func (m machine[S, E]) next(from S, version, expectedVersion uint64, event E, target S) (S, RejectionCode) {
	switch {
	case version != expectedVersion:
		return "", RejectVersionConflict
	case m.isTerminal(from):
		return "", RejectTerminalState
	}
	targets := m.targets(from, event)
	switch {
	case len(targets) == 0:
		return "", RejectInvalidTransition
	case target == "" && len(targets) == 1:
		return targets[0], ""
	case target == "":
		return "", RejectAmbiguousTarget
	case !slices.Contains(targets, target):
		return "", RejectInvalidTarget
	}
	return target, ""
}
