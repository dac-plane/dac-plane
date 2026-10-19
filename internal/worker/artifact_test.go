package worker

import (
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
)

func TestArtifactReplyDecoderRejectsExtraFieldsAndDocuments(t *testing.T) {
	for _, raw := range []string{`{"upload":"opaque","secret":"extra"}`, `{"upload":"opaque"} {"upload":"second"}`, `null`, `{}`} {
		var value contracts.UploadAuthorized
		if err := decodeArtifact([]byte(raw), &value); err == nil && value.Validate() == nil {
			t.Fatalf("malformed authorization accepted: %s", raw)
		}
	}
	var value contracts.UploadAuthorized
	if err := decodeArtifact([]byte(`{"upload":"opaque"}`), &value); err != nil || value.Validate() != nil {
		t.Fatal("valid authorization rejected", err)
	}
}
