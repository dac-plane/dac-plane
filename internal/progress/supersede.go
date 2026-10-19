// Package progress SupersedeOpenAttention closes the attention requests an Attempt's now
// non-live work can no longer serve. The protocol frames the wait, not the
// work: a released attempt stops being able to honor one.
package progress

import (
	"context"
	"errors"
	"slices"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

// SupersedeOpenAttention supersedes every still-open attention request of an
// Attempt whose work stopped being live, in the caller's transaction. The
// machine keeps supersede legal from every unconsumed state, so each request
// is applied against a fresh locked read; a wait that turned consumed
// meanwhile resolved legitimately, and one a racing writer closed needs
// nothing, so neither writes anything.
func (s Step) SupersedeOpenAttention(ctx context.Context, attemptID ids.AttemptID) error {
	requests, err := store.ListOpenAttentionByAttempt(ctx, s.UOW, s.Tenant, attemptID)
	if err != nil {
		return err
	}
	open := lifecycle.UnconsumedAttentionStates()
	for _, request := range requests {
		for try := 0; try < 2; try++ {
			current, err := store.GetAttentionForUpdate(ctx, s.UOW, s.Tenant, request.ID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					break
				}
				return err
			}
			if !slices.Contains(open, current.State) {
				break
			}
			tr, err := s.transition(current.Version)
			if err != nil {
				return err
			}
			_, replayed, err := store.ApplyAttention(ctx, s.UOW, s.Tenant, request.ID, lifecycle.AttentionSupersede, tr)
			if err == nil || replayed {
				break
			}
			if errors.Is(err, store.ErrStaleVersion) {
				continue // re-read; the version moved under the read
			}
			var refusal *lifecycle.TransitionError
			if errors.As(err, &refusal) {
				break // a concurrent writer closed this wait; its evidence is recorded
			}
			return err
		}
	}
	return nil
}
