package contracts

import (
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// RuntimeStart is the provider-neutral input to start_turn. Recovery is set
// only when a harness migration restarts the same product task from artifacts.
type RuntimeStart struct {
	Brief       lifecycle.ExecutionBrief `json:"brief"`
	Environment *EnvironmentReference    `json:"environment,omitempty"`
	Recovery    *MigrationRequest        `json:"recovery,omitempty"`
}

// EnvironmentReference identifies the environment prepared for an Attempt.
// The runtime receives only provider-neutral correlation; credentials and
// provider-specific configuration remain with the adapters.
type EnvironmentReference struct {
	Provider    ids.ProviderID `json:"provider_id"`
	ResourceRef string         `json:"resource_ref"`
}

// Validate reports whether the environment reference is complete.
func (e EnvironmentReference) Validate() error {
	if e.Provider.IsZero() || !validRef(e.ResourceRef) {
		return fmt.Errorf("%w: environment reference requires a provider and bounded resource reference", ErrInvalid)
	}
	return nil
}

// Validate checks the requested outcome and optional migration restart.
func (r RuntimeStart) Validate() error {
	if err := r.Brief.Validate(); err != nil {
		return err
	}
	if r.Environment != nil {
		if err := r.Environment.Validate(); err != nil {
			return err
		}
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
