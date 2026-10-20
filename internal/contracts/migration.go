package contracts

import (
	"fmt"
	"regexp"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// MigrationStage names the externally evidenced steps of one source Attempt.
// Each stage has a stable provider operation and can be recovered by querying
// that operation; it is never inferred from a best-effort cancellation reply.
type MigrationStage string

const (
	MigrationSourceQuiesced         MigrationStage = "source_quiesced"
	MigrationTargetEnvironmentReady MigrationStage = "target_environment_ready"
	MigrationTargetStarted          MigrationStage = "target_started"
	MigrationSourceCleaned          MigrationStage = "source_cleaned"
)

// MigrationStages lists every migration stage.
func MigrationStages() []MigrationStage {
	return []MigrationStage{MigrationSourceQuiesced, MigrationTargetEnvironmentReady, MigrationTargetStarted, MigrationSourceCleaned}
}

// MigrationRecoveryMode declares what continuity a target stage provides.
// restart_from_artifacts is deliberately not a claim of native-session resume.
type MigrationRecoveryMode string

const MigrationRestartFromArtifacts MigrationRecoveryMode = "restart_from_artifacts"

var migrationContextRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// MigrationRequest binds a provider request to its pass, source Attempt and
// product-owned migration fence. TargetAttempt is absent until target startup.
type MigrationRequest struct {
	Migration      ids.MigrationID       `json:"migration_id"`
	Operation      ids.OperationID       `json:"operation_id"`
	SourceAttempt  ids.AttemptID         `json:"source_attempt_id"`
	TargetAttempt  ids.AttemptID         `json:"target_attempt_id,omitzero"`
	Stage          MigrationStage        `json:"stage"`
	ClaimEpoch     uint64                `json:"claim_epoch"`
	Recovery       MigrationRecoveryMode `json:"recovery,omitempty"`
	ContextRefs    []string              `json:"context_refs,omitempty"`
	EnvironmentRef string                `json:"environment_ref,omitempty"`
}

// Validate checks the stage-specific request binding.
func (r MigrationRequest) Validate() error {
	if r.Migration.IsZero() || r.Operation.IsZero() || r.SourceAttempt.IsZero() || r.ClaimEpoch == 0 || len(r.ContextRefs) > 32 {
		return fmt.Errorf("%w: migration request requires pass, operation, source and fence", ErrInvalid)
	}
	for _, ref := range r.ContextRefs {
		if !migrationContextRef.MatchString(ref) {
			return fmt.Errorf("%w: migration context reference is invalid", ErrInvalid)
		}
	}
	switch r.Stage {
	case MigrationSourceQuiesced:
		if !r.TargetAttempt.IsZero() || r.Recovery != "" || r.EnvironmentRef != "" {
			return fmt.Errorf("%w: source quiesce cannot name a target", ErrInvalid)
		}
	case MigrationTargetStarted:
		if r.TargetAttempt.IsZero() || r.TargetAttempt == r.SourceAttempt || r.Recovery != MigrationRestartFromArtifacts || !migrationContextRef.MatchString(r.EnvironmentRef) {
			return fmt.Errorf("%w: target startup requires a distinct artifact restart and environment", ErrInvalid)
		}
	case MigrationSourceCleaned:
		if r.TargetAttempt.IsZero() || r.TargetAttempt == r.SourceAttempt || r.Recovery != "" || !migrationContextRef.MatchString(r.EnvironmentRef) {
			return fmt.Errorf("%w: source cleanup requires its distinct target", ErrInvalid)
		}
	case MigrationTargetEnvironmentReady:
		if r.TargetAttempt.IsZero() || r.TargetAttempt == r.SourceAttempt || r.Recovery != MigrationRestartFromArtifacts || r.EnvironmentRef != "" {
			return fmt.Errorf("%w: target environment requires a distinct artifact restart", ErrInvalid)
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
	Migration      ids.MigrationID       `json:"migration_id"`
	Operation      ids.OperationID       `json:"operation_id"`
	SourceAttempt  ids.AttemptID         `json:"source_attempt_id"`
	TargetAttempt  ids.AttemptID         `json:"target_attempt_id,omitzero"`
	Stage          MigrationStage        `json:"stage"`
	ClaimEpoch     uint64                `json:"claim_epoch"`
	Recovery       MigrationRecoveryMode `json:"recovery,omitempty"`
	ContextRefs    []string              `json:"context_refs,omitempty"`
	EnvironmentRef string                `json:"environment_ref,omitempty"`
	SessionRef     string                `json:"session_ref,omitempty"`
	RecordedAt     time.Time             `json:"recorded_at"`
}

// Validate checks that a receipt cannot be replayed across a different stage
// or migration fence.
func (r MigrationReceipt) Validate() error {
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("%w: migration receipt requires its recorded time", ErrInvalid)
	}
	if r.Stage == MigrationTargetEnvironmentReady || r.Stage == MigrationSourceCleaned {
		if !migrationContextRef.MatchString(r.EnvironmentRef) {
			return fmt.Errorf("%w: migration environment reference is invalid", ErrInvalid)
		}
	} else if r.Stage != MigrationTargetStarted && r.EnvironmentRef != "" {
		return fmt.Errorf("%w: unexpected migration environment reference", ErrInvalid)
	}
	if r.Stage == MigrationTargetStarted {
		if !migrationContextRef.MatchString(r.SessionRef) {
			return fmt.Errorf("%w: target startup receipt requires its runtime session", ErrInvalid)
		}
	} else if r.SessionRef != "" {
		return fmt.Errorf("%w: unexpected runtime session reference", ErrInvalid)
	}
	requestEnvironmentRef := r.EnvironmentRef
	if r.Stage == MigrationTargetEnvironmentReady {
		// The request asks the environment to provision; only its receipt may
		// name the newly allocated environment resource.
		requestEnvironmentRef = ""
	}
	return MigrationRequest{Migration: r.Migration, Operation: r.Operation, SourceAttempt: r.SourceAttempt, TargetAttempt: r.TargetAttempt, Stage: r.Stage, ClaimEpoch: r.ClaimEpoch, Recovery: r.Recovery, ContextRefs: r.ContextRefs, EnvironmentRef: requestEnvironmentRef}.Validate()
}
