package contracts

import (
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// MigrationStage names the externally evidenced steps of one source Attempt.
// Each stage has a stable provider operation and can be recovered by querying
// that operation; it is never inferred from a best-effort cancellation reply.
type MigrationStage string

const (
	MigrationSourceQuiesced MigrationStage = "source_quiesced"
	MigrationTargetStarted  MigrationStage = "target_started"
	MigrationSourceCleaned  MigrationStage = "source_cleaned"
)

// MigrationStages lists every migration stage.
func MigrationStages() []MigrationStage {
	return []MigrationStage{MigrationSourceQuiesced, MigrationTargetStarted, MigrationSourceCleaned}
}

// MigrationRecoveryMode declares what continuity a target stage provides.
// restart_from_artifacts is deliberately not a claim of native-session resume.
type MigrationRecoveryMode string

const MigrationRestartFromArtifacts MigrationRecoveryMode = "restart_from_artifacts"

// MigrationRequest binds a provider request to its pass, source Attempt and
// product-owned migration fence. TargetAttempt is absent until target startup.
type MigrationRequest struct {
	Migration     ids.MigrationID       `json:"migration_id"`
	SourceAttempt ids.AttemptID         `json:"source_attempt_id"`
	TargetAttempt ids.AttemptID         `json:"target_attempt_id,omitempty"`
	Stage         MigrationStage        `json:"stage"`
	ClaimEpoch    uint64                `json:"claim_epoch"`
	Recovery      MigrationRecoveryMode `json:"recovery,omitempty"`
}

// Validate checks the stage-specific request binding.
func (r MigrationRequest) Validate() error {
	if r.Migration.IsZero() || r.SourceAttempt.IsZero() || r.ClaimEpoch == 0 {
		return fmt.Errorf("%w: migration request requires pass, source and fence", ErrInvalid)
	}
	switch r.Stage {
	case MigrationSourceQuiesced:
		if !r.TargetAttempt.IsZero() || r.Recovery != "" {
			return fmt.Errorf("%w: source quiesce cannot name a target", ErrInvalid)
		}
	case MigrationTargetStarted:
		if r.TargetAttempt.IsZero() || r.TargetAttempt == r.SourceAttempt || r.Recovery != MigrationRestartFromArtifacts {
			return fmt.Errorf("%w: target startup requires a distinct artifact restart", ErrInvalid)
		}
	case MigrationSourceCleaned:
		if r.TargetAttempt.IsZero() || r.TargetAttempt == r.SourceAttempt || r.Recovery != "" {
			return fmt.Errorf("%w: source cleanup requires its distinct target", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown migration stage", ErrInvalid)
	}
	return nil
}

// MigrationReceipt is a provider's stage-specific, fence-bound success
// receipt. It provides integrity evidence for product progression only; it
// does not assert semantic equivalence between source and target execution.
type MigrationReceipt struct {
	Migration     ids.MigrationID       `json:"migration_id"`
	SourceAttempt ids.AttemptID         `json:"source_attempt_id"`
	TargetAttempt ids.AttemptID         `json:"target_attempt_id,omitempty"`
	Stage         MigrationStage        `json:"stage"`
	ClaimEpoch    uint64                `json:"claim_epoch"`
	Recovery      MigrationRecoveryMode `json:"recovery,omitempty"`
	RecordedAt    time.Time             `json:"recorded_at"`
}

// Validate checks that a receipt cannot be replayed across a different stage
// or migration fence.
func (r MigrationReceipt) Validate() error {
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("%w: migration receipt requires its recorded time", ErrInvalid)
	}
	return MigrationRequest{Migration: r.Migration, SourceAttempt: r.SourceAttempt, TargetAttempt: r.TargetAttempt, Stage: r.Stage, ClaimEpoch: r.ClaimEpoch, Recovery: r.Recovery}.Validate()
}
