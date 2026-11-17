package simulator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	"github.com/dac-plane/dac-plane/internal/simulator"
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
	if stored := s.Stored(m.Artifact); !stored.Present || stored.Digest != m.Digest || stored.Size != m.Size {
		t.Fatalf("Stored() = %+v, want manifest bytes", stored)
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

func TestArtifactByteStoreAuthorizesDownloadsOfCommittedVersionsOnly(t *testing.T) {
	config := testConfig(t)
	s, err := simulator.NewByteArtifactStore(config)
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, config, mustID[ids.Task](t), mustID[ids.Attempt](t), mustID[ids.Operation](t), []byte("review"), 1)
	send := func(method contracts.Method, arguments any) contracts.Reply {
		t.Helper()
		raw, e := json.Marshal(arguments)
		if e != nil {
			t.Fatal(e)
		}
		r := newRoleRequest(t, config, mustID[ids.Operation](t), contracts.RoleArtifactStore, method, raw)
		if method == contracts.MethodCommitManifest {
			r.Operation = m.Operation
		}
		r.Attempt = m.Attempt
		reply, e := s.Send(context.Background(), r)
		if e != nil {
			t.Fatal(e)
		}
		return reply
	}
	download := func(version string) contracts.Reply {
		return send(contracts.MethodAuthorizeDownload, contracts.DownloadRequest{Manifest: m, ObjectVersion: version})
	}
	if reply := download("v1"); reply.Outcome != contracts.OutcomeFailed || reply.Failure.Code != contracts.FailureStalePrecondition {
		t.Fatalf("download before commit = %+v", reply)
	}

	transfer := contracts.ArtifactTransfer{Manifest: m, NativeRef: "review"}
	var auth contracts.UploadAuthorized
	if err = json.Unmarshal(send(contracts.MethodAuthorizeUpload, transfer).Result, &auth); err != nil {
		t.Fatal(err)
	}
	transfer.Upload = auth.Upload
	if err = s.Upload(transfer, []byte("review")); err != nil {
		t.Fatal(err)
	}
	transfer.Upload = ""
	var committed contracts.ArtifactCommitted
	if err = json.Unmarshal(send(contracts.MethodCommitManifest, transfer).Result, &committed); err != nil || committed.Outcome != lifecycle.CommitCommitted {
		t.Fatalf("commit = %+v %v", committed, err)
	}

	reply := download(committed.ObjectVersion)
	var authorized contracts.DownloadAuthorized
	if reply.Outcome != contracts.OutcomeOK || json.Unmarshal(reply.Result, &authorized) != nil || authorized.Validate() != nil {
		t.Fatalf("download = %+v", reply)
	}
	if authorized.Download != simulator.DownloadPrefix+m.Artifact.String() || !s.Committed(m.Artifact) {
		t.Fatalf("download names %s", authorized.Download)
	}
	if reply := download("another-version"); reply.Outcome != contracts.OutcomeFailed {
		t.Fatal("download of another version authorized")
	}
}
