package simulator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestArtifactByteStoreHashesAndFreezesExactBytes(t *testing.T) {
	config := testConfig(t)
	s, err := simulator.NewByteArtifactStore(config)
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, config, mustID[ids.Task](t), mustID[ids.Attempt](t), mustID[ids.Operation](t), []byte("correct"), 1)
	transfer := contracts.ArtifactTransfer{Manifest: m, NativeRef: "patch"}
	send := func(id ids.OperationID, method contracts.Method) contracts.Reply {
		t.Helper()
		raw, e := json.Marshal(transfer)
		if e != nil {
			t.Fatal(e)
		}
		r := newRoleRequest(t, config, id, contracts.RoleArtifactStore, method, raw)
		r.Attempt = m.Attempt
		reply, e := s.Send(context.Background(), r)
		if e != nil {
			t.Fatal(e)
		}
		return reply
	}
	var auth contracts.UploadAuthorized
	if err = json.Unmarshal(send(mustID[ids.Operation](t), contracts.MethodAuthorizeUpload).Result, &auth); err != nil {
		t.Fatal(err)
	}
	upload := transfer
	upload.Upload = auth.Upload
	if err = s.Upload(upload, []byte("wrong")); err != nil {
		t.Fatal(err)
	}
	var first contracts.ArtifactCommitted
	if err = json.Unmarshal(send(m.Operation, contracts.MethodCommitManifest).Result, &first); err != nil {
		t.Fatal(err)
	}
	if first.Outcome != lifecycle.CommitRetry || first.Reason != lifecycle.RetryDigestMismatch {
		t.Fatalf("mismatch=%+v", first)
	}
	// Retry must not be cached as a terminal response under the stable commit.
	if err = s.Upload(upload, []byte("correct")); err != nil {
		t.Fatal(err)
	}
	var committed contracts.ArtifactCommitted
	if err = json.Unmarshal(send(m.Operation, contracts.MethodCommitManifest).Result, &committed); err != nil {
		t.Fatal(err)
	}
	if err = committed.Validate(); err != nil || committed.Outcome != lifecycle.CommitCommitted {
		t.Fatalf("commit=%+v %v", committed, err)
	}
	if s.Upload(upload, []byte("changed")) == nil {
		t.Fatal("committed bytes overwritten")
	}
	var replay contracts.ArtifactCommitted
	if err = json.Unmarshal(send(m.Operation, contracts.MethodCommitManifest).Result, &replay); err != nil {
		t.Fatal(err)
	}
	if replay != committed || s.Ledger().Effects(m.Operation) != 1 {
		t.Fatal("commit identity was not immutable")
	}
	transfer.Manifest.SourceRevision = "changed"
	raw, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	request := newRoleRequest(t, config, m.Operation, contracts.RoleArtifactStore, contracts.MethodCommitManifest, raw)
	request.Attempt = m.Attempt
	if _, err = s.Send(context.Background(), request); err == nil {
		t.Fatal("same operation accepted changed manifest")
	}
}

func TestArtifactByteStoreFencesUnallocatedCommit(t *testing.T) {
	config := testConfig(t)
	s, err := simulator.NewByteArtifactStore(config)
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, config, mustID[ids.Task](t), mustID[ids.Attempt](t), mustID[ids.Operation](t), nil, 4)
	raw, err := json.Marshal(contracts.ArtifactTransfer{Manifest: m, NativeRef: "patch"})
	if err != nil {
		t.Fatal(err)
	}
	request := newRoleRequest(t, config, m.Operation, contracts.RoleArtifactStore, contracts.MethodCommitManifest, raw)
	request.Attempt = m.Attempt
	reply, err := s.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var c contracts.ArtifactCommitted
	if err = json.Unmarshal(reply.Result, &c); err != nil {
		t.Fatal(err)
	}
	if c.Outcome != lifecycle.CommitFenced || s.Ledger().Effects(m.Operation) != 0 {
		t.Fatalf("unallocated generation=%+v", c)
	}
}
