package contracts

import (
	"fmt"

	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// RuntimeStart is the provider-neutral input to start_turn. Recovery is set
// only when a harness migration restarts the same product task from artifacts.
// Agent is set only for a task delegated to an agent, and only toward an
// adapter that negotiated CapabilityAgentInstructions.
type RuntimeStart struct {
	Brief       lifecycle.ExecutionBrief `json:"brief"`
	Agent       *AgentInstructions       `json:"agent,omitempty"`
	Environment *EnvironmentReference    `json:"environment,omitempty"`
	Recovery    *MigrationRequest        `json:"recovery,omitempty"`
}

// AgentInstructions is the role and standing instructions of the agent
// revision a task was delegated to. They travel beside the brief, never inside
// it, so the runtime can tell the owner's request from the agent's direction.
type AgentInstructions struct {
	Role         string `json:"role"`
	Instructions string `json:"instructions,omitempty"`
}

// Validate reports whether the role and instructions are within the agent
// profile's bounds.
func (a AgentInstructions) Validate() error {
	return lifecycle.ValidateAgentDirection(a.Role, a.Instructions)
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
	if r.Agent != nil {
		if err := r.Agent.Validate(); err != nil {
			return err
		}
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
