package lifecycle

import (
	"path/filepath"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/testsupport/schematest"
)

func TestContractConformanceArtifactVerification(t *testing.T) {
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
	r, err := NewResultRevision(newID[ids.Revision](t), m.Attempt, []Artifact{{ArtifactManifest: m, CommittedAt: testNow}}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	r.Verifications = []ids.VerificationID{v.ID}
	rc := schematest.GoContract[ResultRevision](filepath.Join(schemaDir, "result-revision.schema.json"))
	rr := schematest.Marshal(t, r)
	if err = rc.GoAccept(rr); err != nil {
		t.Fatal(err)
	}
	if err = schematest.Accepts(t, schematest.Compile(t, rc.Schema), rr); err != nil {
		t.Fatal(err)
	}
	r.Verifications = append(r.Verifications, v.ID)
	if r.Validate() == nil {
		t.Fatal("duplicate verification links accepted")
	}
	v.Size++
	if v.Validate() == nil {
		t.Fatal("mismatching bytes accepted")
	}
}
