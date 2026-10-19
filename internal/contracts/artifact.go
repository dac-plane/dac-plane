package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// ArtifactDeclaration describes evidence, not an authoritative product record.
type ArtifactDeclaration struct {
	NativeRef string                 `json:"native_ref"`
	Kind      lifecycle.ArtifactKind `json:"kind"`
	Digest    identity.Digest        `json:"digest"`
	Size      uint64                 `json:"size"`
	MediaType string                 `json:"media_type"`
}

// ResultProposed is an explicit proposal for one bound turn's exact output.
type ResultProposed struct {
	SessionRef     string                `json:"session_ref"`
	TurnRef        string                `json:"turn_ref"`
	SourceRevision string                `json:"source_revision"`
	Artifacts      []ArtifactDeclaration `json:"artifacts"`
}

func (p ResultProposed) Validate() error {
	if !validRef(p.SessionRef) || !validRef(p.TurnRef) || !validRef(p.SourceRevision) || len(p.Artifacts) == 0 || len(p.Artifacts) > 32 {
		return fmt.Errorf("%w: proposal requires a turn, revision and 1..32 artifacts", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, d := range p.Artifacts {
		if !validRef(d.NativeRef) || seen[d.NativeRef] {
			return fmt.Errorf("%w: duplicate or invalid evidence reference", ErrInvalid)
		}
		seen[d.NativeRef] = true
		if err := lifecycle.ValidateArtifactMetadata(d.Kind, d.Digest, d.Size, d.MediaType, p.SourceRevision); err != nil {
			return err
		}
	}
	return nil
}

// ArtifactTransfer binds all stages to one immutable manifest. Upload is an
// opaque, short-lived capability; callers must not persist it.
type ArtifactTransfer struct {
	Manifest  lifecycle.ArtifactManifest `json:"manifest"`
	NativeRef string                     `json:"native_ref"`
	Upload    string                     `json:"upload,omitempty"`
}

func (t ArtifactTransfer) Validate() error {
	if err := t.Manifest.Validate(); err != nil {
		return err
	}
	if !validRef(t.NativeRef) || len(t.Upload) > 4096 {
		return fmt.Errorf("%w: invalid transfer reference", ErrInvalid)
	}
	return nil
}

// UploadAuthorized contains no admission claim.
type UploadAuthorized struct {
	Upload string `json:"upload"`
}

func (u UploadAuthorized) Validate() error {
	if u.Upload == "" || len(u.Upload) > 4096 {
		return fmt.Errorf("%w: invalid upload capability", ErrInvalid)
	}
	return nil
}

// ArtifactStored is an observation only; it is not a commit receipt.
type ArtifactStored struct {
	Present bool            `json:"present"`
	Digest  identity.Digest `json:"digest,omitempty"`
	Size    uint64          `json:"size"`
}

// UnmarshalJSON preserves required false/zero observations. Missing fields
// cannot be confused with an explicitly observed absent, zero-byte object.
func (s *ArtifactStored) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Present *bool           `json:"present"`
		Digest  json.RawMessage `json:"digest"`
		Size    *uint64         `json:"size"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return err
	}
	if wire.Present == nil || wire.Size == nil {
		return ErrInvalid
	}
	*s = ArtifactStored{Present: *wire.Present, Size: *wire.Size}
	if len(wire.Digest) > 0 {
		if !s.Present {
			return ErrInvalid
		}
		if err := json.Unmarshal(wire.Digest, &s.Digest); err != nil {
			return err
		}
	}
	return s.Validate()
}

func (s ArtifactStored) Validate() error {
	if s.Size > lifecycle.MaxAmount {
		return ErrInvalid
	}
	if s.Present {
		return s.Digest.Validate()
	}
	if s.Digest != "" || s.Size != 0 {
		return ErrInvalid
	}
	return nil
}

// ArtifactCommitted is the store's immutable, operation-bound admission
// receipt. Retry is non-final and may be queried again under the same identity.
type ArtifactCommitted struct {
	Manifest      lifecycle.ArtifactManifest      `json:"manifest"`
	Outcome       lifecycle.ArtifactCommitOutcome `json:"outcome"`
	Reason        lifecycle.RetryReason           `json:"reason,omitempty"`
	ObjectVersion string                          `json:"object_version,omitempty"`
	Observed      ArtifactStored                  `json:"observed"`
	CommittedAt   time.Time                       `json:"committed_at,omitzero"`
}

func (c ArtifactCommitted) Validate() error {
	if err := c.Manifest.Validate(); err != nil {
		return err
	}
	if err := c.Observed.Validate(); err != nil {
		return err
	}
	switch c.Outcome {
	case lifecycle.CommitCommitted:
		if !validRef(c.ObjectVersion) || c.CommittedAt.IsZero() || !c.Observed.Present || c.Observed.Digest != c.Manifest.Digest || c.Observed.Size != c.Manifest.Size || c.Reason != "" {
			return ErrInvalid
		}
	case lifecycle.CommitRetry:
		if c.Reason != lifecycle.RetryMissingBytes && c.Reason != lifecycle.RetryDigestMismatch && c.Reason != lifecycle.RetrySizeMismatch {
			return ErrInvalid
		}
		if c.ObjectVersion != "" || !c.CommittedAt.IsZero() {
			return ErrInvalid
		}
	case lifecycle.CommitFenced:
		if c.ObjectVersion != "" || !c.CommittedAt.IsZero() || c.Reason != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
