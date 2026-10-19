package lifecycle

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var mediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)

// ArtifactKind is what a piece of evidence supports.
type ArtifactKind string

const (
	ArtifactCodeChange     ArtifactKind = "code_change"
	ArtifactTestResult     ArtifactKind = "test_result"
	ArtifactVisualProof    ArtifactKind = "visual_proof"
	ArtifactExternalAction ArtifactKind = "external_action"
	ArtifactAgentClaim     ArtifactKind = "agent_claim"
)

// ArtifactKinds lists every ArtifactKind.
func ArtifactKinds() []ArtifactKind {
	return []ArtifactKind{ArtifactCodeChange, ArtifactTestResult, ArtifactVisualProof, ArtifactExternalAction, ArtifactAgentClaim}
}

// ArtifactManifest is what a writer declares it has uploaded. Operation is the
// stable commit operation and the manifest's identity: a retry repeats it.
// WriterEpoch is the writer's fence; a writer from an older epoch is stale.
type ArtifactManifest struct {
	Artifact       ids.ArtifactID  `json:"artifact_id"`
	Tenant         ids.TenantID    `json:"tenant_id"`
	Task           ids.TaskID      `json:"task_id"`
	Attempt        ids.AttemptID   `json:"attempt_id"`
	Operation      ids.OperationID `json:"operation_id"`
	Producer       ids.ProviderID  `json:"producer_id"`
	Kind           ArtifactKind    `json:"kind"`
	Digest         identity.Digest `json:"digest"`
	Size           uint64          `json:"size"`
	MediaType      string          `json:"media_type"`
	SourceRevision string          `json:"source_revision,omitzero"`
	WriterEpoch    uint64          `json:"writer_epoch"`
}

// Validate reports whether the manifest is complete.
func (m ArtifactManifest) Validate() error {
	if m.Artifact.IsZero() || m.Tenant.IsZero() || m.Task.IsZero() || m.Attempt.IsZero() ||
		m.Operation.IsZero() || m.Producer.IsZero() || m.WriterEpoch == 0 {
		return fmt.Errorf("%w: manifest requires identifiers and a positive writer epoch", ErrInvalid)
	}
	return ValidateArtifactMetadata(m.Kind, m.Digest, m.Size, m.MediaType, m.SourceRevision)
}

// ValidateArtifactMetadata applies the same evidence rules to declarations
// and product-allocated manifests without inventing placeholder identities.
func ValidateArtifactMetadata(kind ArtifactKind, digest identity.Digest, size uint64, mediaType, sourceRevision string) error {
	if !slices.Contains(ArtifactKinds(), kind) {
		return fmt.Errorf("%w: unknown artifact kind", ErrInvalid)
	}
	if size > MaxAmount || !mediaTypePattern.MatchString(mediaType) || len(sourceRevision) > maxNativeRefLength {
		return fmt.Errorf("%w: manifest size, media type or source revision out of bounds", ErrInvalid)
	}
	return validDigest(digest)
}

// StoredObject is the artifact store's own observation of the uploaded bytes.
// The product compares it with the manifest; it never trusts the declaration.
type StoredObject struct {
	Present bool
	Digest  identity.Digest
	Size    uint64
}

// ArtifactCommitOutcome is the result of a manifest commit.
type ArtifactCommitOutcome string

const (
	CommitCommitted ArtifactCommitOutcome = "committed"
	CommitRetry     ArtifactCommitOutcome = "retry"
	CommitFenced    ArtifactCommitOutcome = "fenced"
)

// ArtifactCommitOutcomes lists every ArtifactCommitOutcome.
func ArtifactCommitOutcomes() []ArtifactCommitOutcome {
	return []ArtifactCommitOutcome{CommitCommitted, CommitRetry, CommitFenced}
}

// RetryReason is why a commit made no admission but may be repeated.
type RetryReason string

const (
	RetryMissingBytes   RetryReason = "missing_bytes"
	RetryDigestMismatch RetryReason = "digest_mismatch"
	RetrySizeMismatch   RetryReason = "size_mismatch"
)

// RetryReasons lists every RetryReason.
func RetryReasons() []RetryReason {
	return []RetryReason{RetryMissingBytes, RetryDigestMismatch, RetrySizeMismatch}
}

// Artifact is registered evidence: a committed manifest whose bytes the store
// verified. It records the exact digest and size, the manifest's commit
// operation, its provenance and the writer epoch that won. Artifact identity
// and integrity metadata are product records; the bytes may live elsewhere.
type Artifact struct {
	ArtifactManifest
	CommittedAt time.Time `json:"committed_at"`
}

// Validate reports whether the artifact is a complete committed record.
func (a Artifact) Validate() error {
	if a.CommittedAt.IsZero() {
		return fmt.Errorf("%w: artifact requires a commit time", ErrInvalid)
	}
	return a.ArtifactManifest.Validate()
}

// Admission is the outcome of one manifest commit. Artifact is set only when
// the outcome is committed, or when fenced by an existing winner, in which
// case it is that winner, unchanged.
type Admission struct {
	Outcome  ArtifactCommitOutcome
	Reason   RetryReason
	Artifact Artifact
	manifest ArtifactManifest
}

// Audit records the admission decision: a commit is applied, a retry or a
// fence is a refusal with its reason.
func (a Admission) Audit(id ids.AuditID, actor Actor, at time.Time) AuditEvent {
	change := Change{
		Tenant: a.manifest.Tenant, Record: RecordArtifact, RecordID: a.manifest.Artifact.Untyped(),
		Event: "commit", From: "staged", Version: 1,
	}
	switch a.Outcome {
	case CommitCommitted:
		change.To = string(CommitCommitted)
		return change.Audit(id, actor, at)
	case CommitFenced:
		return (&TransitionError{Change: change, Code: RejectStaleWriter}).Audit(id, actor, at)
	default:
		return (&TransitionError{Change: change, Code: RejectAdmissionRetry}).Audit(id, actor, at)
	}
}

// AdmitArtifact decides a manifest commit. currentEpoch is the attempt's
// current writer epoch; existing is the artifact already registered under the
// manifest's artifact id, if any. Only verified bytes are admitted:
//
//   - fenced: the writer's epoch is older than currentEpoch, or another
//     manifest already won this artifact. The winner is not changed.
//   - retry: the store does not hold matching bytes. Nothing is registered and
//     the same operation may commit again.
//   - committed: the store's digest and size equal the manifest. Committing
//     the same manifest again returns the same artifact.
func AdmitArtifact(manifest ArtifactManifest, stored StoredObject, currentEpoch uint64, existing *Artifact, at time.Time) (Admission, error) {
	if err := manifest.Validate(); err != nil {
		return Admission{}, err
	}
	if currentEpoch == 0 || at.IsZero() {
		return Admission{}, fmt.Errorf("%w: admission requires the current writer epoch and a time", ErrInvalid)
	}
	if existing != nil {
		if err := existing.Validate(); err != nil {
			return Admission{}, err
		}
		if existing.Artifact != manifest.Artifact {
			return Admission{}, fmt.Errorf("%w: existing artifact is a different record", ErrInvalid)
		}
	}
	admission := Admission{manifest: manifest}
	switch {
	case manifest.WriterEpoch < currentEpoch:
		admission.Outcome = CommitFenced
	case existing != nil && existing.ArtifactManifest == manifest:
		admission.Outcome, admission.Artifact = CommitCommitted, *existing
		return admission, nil
	case existing != nil:
		admission.Outcome = CommitFenced
	case !stored.Present:
		admission.Outcome, admission.Reason = CommitRetry, RetryMissingBytes
	case stored.Digest != manifest.Digest:
		admission.Outcome, admission.Reason = CommitRetry, RetryDigestMismatch
	case stored.Size != manifest.Size:
		admission.Outcome, admission.Reason = CommitRetry, RetrySizeMismatch
	default:
		admission.Outcome = CommitCommitted
		admission.Artifact = Artifact{ArtifactManifest: manifest, CommittedAt: at}
		return admission, nil
	}
	if admission.Outcome == CommitFenced && existing != nil {
		admission.Artifact = *existing
	}
	return admission, nil
}
