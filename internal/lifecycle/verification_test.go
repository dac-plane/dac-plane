package lifecycle

import (
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
	"path/filepath"
	"testing"
)

func TestArtifactVerificationContract(t *testing.T) {
	m := newManifest(t, newTask(t))
	v := ArtifactVerification{ID: newID[ids.Verification](t), Manifest: m, Store: newID[ids.Provider](t), Digest: m.Digest, Size: m.Size, ObjectVersion: "immutable-1", VerifiedAt: testNow}
	c := schematest.GoContract[ArtifactVerification](filepath.Join(schemaDir, "artifact-verification.schema.json"))
	raw := schematest.Marshal(t, v)
	if err := c.GoAccept(raw); err != nil {
		t.Fatal(err)
	}
	if err := schematest.Accepts(t, schematest.Compile(t, c.Schema), raw); err != nil {
		t.Fatal(err)
	}
	v.Size++
	if v.Validate() == nil {
		t.Fatal("mismatching bytes accepted")
	}
}
