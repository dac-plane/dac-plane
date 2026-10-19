package contracts

import (
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ActionExecution is the safe execution projection retained by the product.
// The qualified executor resolves the input by digest; unrestricted action
// payloads and credentials never enter the outbox.
type ActionExecution struct {
	Request      identity.ActionRequest   `json:"request"`
	Grant        ids.GrantID              `json:"grant_id,omitzero"`
	Decision     ids.DecisionID           `json:"decision_id,omitzero"`
	Source       identity.AuthoritySource `json:"source,omitzero"`
	Precondition *ActionPrecondition      `json:"precondition,omitempty"`
}

func (a ActionExecution) Validate() error {
	if err := a.Request.Validate(); err != nil {
		return err
	}
	if a.Grant.IsZero() || a.Decision.IsZero() || (a.Source != identity.SourceOneTimeApproval && a.Source != identity.SourceStandingGrant) {
		return fmt.Errorf("%w: execution requires authority provenance", ErrInvalid)
	}
	if a.Precondition != nil {
		if err := a.Precondition.Validate(); err != nil {
			return err
		}
		if a.Precondition.Operation != a.Request.Operation {
			return fmt.Errorf("%w: precondition operation mismatch", ErrInvalid)
		}
	}
	return nil
}

// EnforcementReceipt proves the executor completed this exact authorized action.
// Receipt identity is the operation; retries return the same evidence.
type EnforcementReceipt struct {
	Execution    ActionExecution `json:"execution"`
	OutputDigest identity.Digest `json:"output_digest"`
	CompletedAt  time.Time       `json:"completed_at"`
}

func (r EnforcementReceipt) Validate() error {
	if err := r.Execution.Validate(); err != nil {
		return err
	}
	if err := r.OutputDigest.Validate(); err != nil {
		return err
	}
	if r.CompletedAt.IsZero() {
		return fmt.Errorf("%w: receipt requires completion time", ErrInvalid)
	}
	return nil
}
