package lifecycle

import (
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ArtifactVerification records integrity admission, not semantic correctness.
// Manifest and observed bytes are both preserved, rather than inferring that
// an Artifact row implies verification ever took place.
type ArtifactVerification struct {
	ID            ids.VerificationID `json:"verification_id"`
	Manifest      ArtifactManifest   `json:"manifest"`
	Store         ids.ProviderID     `json:"store_id"`
	Digest        identity.Digest    `json:"observed_digest"`
	Size          uint64             `json:"observed_size"`
	ObjectVersion string             `json:"object_version"`
	VerifiedAt    time.Time          `json:"verified_at"`
}

func (v ArtifactVerification) Validate() error {
	if err := v.Manifest.Validate(); err != nil {
		return err
	}
	if v.ID.IsZero() || v.Store.IsZero() || v.Digest != v.Manifest.Digest || v.Size != v.Manifest.Size || v.ObjectVersion == "" || len(v.ObjectVersion) > 256 || v.VerifiedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}
