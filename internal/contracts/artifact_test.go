package contracts

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/testsupport/schematest"
)

func TestContractConformanceArtifactProtocol(t *testing.T) {
	m := lifecycle.ArtifactManifest{Artifact: newID[ids.Artifact](t), Tenant: newID[ids.Tenant](t), Task: newID[ids.Task](t), Attempt: newID[ids.Attempt](t), Operation: newID[ids.Operation](t), Producer: newID[ids.Provider](t), Kind: lifecycle.ArtifactCodeChange, Digest: identity.DigestOf([]byte("patch")), Size: 5, MediaType: "text/plain", SourceRevision: "rev1", WriterEpoch: 1}
	p := ResultProposed{SessionRef: "s", TurnRef: "t", SourceRevision: "rev1", Artifacts: []ArtifactDeclaration{{NativeRef: "patch", Kind: m.Kind, Digest: m.Digest, Size: m.Size, MediaType: m.MediaType}}}
	for _, tc := range []struct {
		c schematest.Contract
		v any
	}{
		{schematest.GoContract[ResultProposed](filepath.Join(schemaDir, "entry-result-proposed.schema.json")), p},
		{schematest.GoContract[ArtifactTransfer](filepath.Join(schemaDir, "artifact-transfer.schema.json")), ArtifactTransfer{Manifest: m, NativeRef: "patch"}},
		{schematest.GoContract[UploadAuthorized](filepath.Join(schemaDir, "upload-authorized.schema.json")), UploadAuthorized{Upload: "opaque"}},
		{schematest.GoContract[ArtifactStored](filepath.Join(schemaDir, "artifact-stored.schema.json")), ArtifactStored{Present: true, Digest: m.Digest, Size: m.Size}},
		{schematest.GoContract[ArtifactCommitted](filepath.Join(schemaDir, "artifact-committed.schema.json")), ArtifactCommitted{Manifest: m, Outcome: lifecycle.CommitCommitted, ObjectVersion: "v1", Observed: ArtifactStored{Present: true, Digest: m.Digest, Size: m.Size}, CommittedAt: testNow}},
		{schematest.GoContract[DownloadRequest](filepath.Join(schemaDir, "download-request.schema.json")), DownloadRequest{Manifest: m, ObjectVersion: "v1"}},
		{schematest.GoContract[DownloadAuthorized](filepath.Join(schemaDir, "download-authorized.schema.json")), DownloadAuthorized{Download: "https://store.example/objects/a?X-Amz-Signature=s", ExpiresAt: testNow}},
	} {
		raw := schematest.Marshal(t, tc.v)
		if err := tc.c.GoAccept(raw); err != nil {
			t.Fatal(err)
		}
		s := schematest.Compile(t, tc.c.Schema)
		if err := schematest.Accepts(t, s, raw); err != nil {
			t.Fatal(err)
		}
		bad := schematest.Mutate(t, raw, func(d map[string]any) { d["credential"] = "forbidden" })
		if tc.c.GoAccept(bad) == nil || schematest.Accepts(t, s, bad) == nil {
			t.Fatal("unknown field accepted")
		}
	}
	for _, bad := range []DownloadAuthorized{
		{Download: "http://store.example/a", ExpiresAt: testNow},
		{Download: "https://store.example/a"},
		{Download: "", ExpiresAt: testNow},
	} {
		if bad.Validate() == nil {
			t.Fatalf("download %+v accepted", bad)
		}
	}
	if (DownloadRequest{Manifest: m}).Validate() == nil {
		t.Fatal("download request without an object version accepted")
	}
	p.Artifacts = append(p.Artifacts, p.Artifacts[0])
	if p.Validate() == nil {
		t.Fatal("duplicate evidence accepted")
	}
	if ClassifyEntry(EntryResultProposed) != EntryApply {
		t.Fatal("proposal not applied")
	}
	for _, raw := range []string{`{}`, `{"present":false}`, `{"size":0}`, `{"present":null,"size":0}`, `{"present":false,"size":0,"digest":null}`} {
		var stored ArtifactStored
		if json.Unmarshal([]byte(raw), &stored) == nil {
			t.Fatalf("missing or null observation accepted: %s", raw)
		}
	}
}
