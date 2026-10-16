package worker

import (
	"cmp"
	"slices"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// Scheduler orders one poll batch for dispatch. Person-facing work goes
// first; background work fills up to backgroundBudget so one person-heavy
// poll cannot starve background processing forever. Ordering is stable:
// demand class, then not-before time, then outbox identity.
type Scheduler struct {
	// BackgroundBudget bounds background rows per batch. Zero means the
	// scheduler takes no background rows this tick.
	BackgroundBudget int
}

// Select returns the dispatch order for ready rows without mutating them.
func (s Scheduler) Select(ready []store.OutboxRow) []store.OutboxRow {
	ordered := slices.Clone(ready)
	slices.SortFunc(ordered, func(a, b store.OutboxRow) int {
		if c := compareDemand(a.Demand, b.Demand); c != 0 {
			return c
		}
		if c := a.NextTryAt.Compare(b.NextTryAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	var person, background []store.OutboxRow
	for _, row := range ordered {
		if row.Demand == contracts.DemandPerson {
			person = append(person, row)
		} else {
			background = append(background, row)
		}
	}
	if len(background) > s.BackgroundBudget {
		background = background[:s.BackgroundBudget]
	}
	return append(person, background...)
}

// compareDemand ranks person demand ahead of everything else. Unknown demand
// values (including the zero value on rows written before #21) sort as
// background: scheduling must never fail closed on a hint.
func compareDemand(a, b contracts.Demand) int {
	rank := func(d contracts.Demand) int {
		if d == contracts.DemandPerson {
			return 0
		}
		return 1
	}
	return cmp.Compare(rank(a), rank(b))
}
