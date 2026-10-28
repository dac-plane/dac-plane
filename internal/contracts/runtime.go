package contracts

import (
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// RuntimeStart is the provider-neutral input to start_turn. Recovery is set
// only when a harness migration restarts the same product task from artifacts.
type RuntimeStart struct {
	Brief    lifecycle.ExecutionBrief `json:"brief"`
	Recovery *MigrationRequest        `json:"recovery,omitempty"`
}

// Validate checks the requested outcome and optional migration restart.
func (r RuntimeStart) Validate() error {
	if err := r.Brief.Validate(); err != nil {
		return err
	}
	if r.Recovery == nil {
		return nil
	}
	if err := r.Recovery.Validate(); err != nil {
		return err
	}
	if r.Recovery.Stage != MigrationTargetStarted {
		return fmt.Errorf("%w: runtime recovery must be a target-started migration", ErrInvalid)
	}
	return nil
}
