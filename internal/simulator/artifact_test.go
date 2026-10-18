package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// artifactManifest is a valid commit manifest for content at the writer epoch.
func artifactManifest(t *testing.T, config simulator.Config, task ids.TaskID, attempt ids.AttemptID, operation ids.OperationID, content []byte, epoch uint64) lifecycle.ArtifactManifest {
	t.Helper()
	return lifecycle.ArtifactManifest{
		Artifact:    mustID[ids.Artifact](t),
		Tenant:      config.Tenant,
		Task:        task,
		Attempt:     attempt,
		Operation:   operation,
		Producer:    config.Provider,
		Kind:        lifecycle.ArtifactCodeChange,
		Digest:      identity.DigestOf(content),
		Size:        uint64(len(content)),
		MediaType:   "text/plain",
		WriterEpoch: epoch,
	}
}

func TestArtifactUploadAndObservation(t *testing.T) {
	config := testConfig(t)
	authorize := mustID[ids.Operation](t)
	stat := mustID[ids.Operation](t)
	content := []byte("compiled evidence")
	object := lifecycle.StoredObject{Present: true, Digest: identity.DigestOf(content), Size: uint64(len(content))}
	store := newArtifactStore(t, config, map[ids.OperationID]simulator.ArtifactOp{
		authorize: {Reply: simulator.Accepted(nil), Effect: simulator.UploadAuthorized},
		stat:      {Reply: simulator.StoredReply(object)},
	})

	reply, err := store.Send(context.Background(), newRoleRequest(t, config, authorize, contracts.RoleArtifactStore, contracts.MethodAuthorizeUpload, nil))
	if err != nil {
		t.Fatalf("Send(authorize_upload) error = %v", err)
	}
	if reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		t.Fatalf("authorize reply = %+v, want an accepted success", reply)
	}
	applied := store.Ledger().Applied()
	if len(applied) != 1 || applied[0].Role != contracts.RoleArtifactStore || applied[0].Kind != simulator.UploadAuthorized {
		t.Fatalf("applied = %+v, want one artifact_store upload_authorized effect", applied)
	}

	reply, err = store.Send(context.Background(), newRoleRequest(t, config, stat, contracts.RoleArtifactStore, contracts.MethodStatObject, nil))
	if err != nil {
		t.Fatalf("Send(stat_object) error = %v", err)
	}
	var observed lifecycle.StoredObject
	if err := json.Unmarshal(reply.Result, &observed); err != nil {
		t.Fatalf("stat reply is not a StoredObject: %v", err)
	}
	if observed != object {
		t.Fatalf("observation = %+v, want %+v", observed, object)
	}
	if got := store.Ledger().Effects(stat); got != 0 {
		t.Fatalf("a stat applied %d effects, want 0", got)
	}
}

func TestArtifactCommitOutcomes(t *testing.T) {
	config := testConfig(t)
	task := mustID[ids.Task](t)
	attempt := mustID[ids.Attempt](t)
	commit := mustID[ids.Operation](t)
	content := []byte("committed evidence")
	manifest := artifactManifest(t, config, task, attempt, commit, content, 3)
	at := testNow

	winnerManifest := manifest
	winnerManifest.Digest = identity.DigestOf([]byte("the winning bytes"))
	winnerManifest.Size = uint64(len("the winning bytes"))
	winner := lifecycle.Artifact{ArtifactManifest: winnerManifest, CommittedAt: at.Add(-time.Minute)}
	if err := winner.Validate(); err != nil {
		t.Fatalf("winner.Validate() error = %v", err)
	}
	same := lifecycle.Artifact{ArtifactManifest: manifest, CommittedAt: at.Add(-time.Minute)}
	if err := same.Validate(); err != nil {
		t.Fatalf("same.Validate() error = %v", err)
	}

	storedMatching := lifecycle.StoredObject{Present: true, Digest: manifest.Digest, Size: manifest.Size}
	cases := []struct {
		name         string
		stored       lifecycle.StoredObject
		epoch        uint64
		existing     *lifecycle.Artifact
		want         lifecycle.ArtifactCommitOutcome
		wantReason   lifecycle.RetryReason
		wantArtifact lifecycle.Artifact
	}{
		{
			name: "committed", stored: storedMatching, epoch: 3,
			want:         lifecycle.CommitCommitted,
			wantArtifact: lifecycle.Artifact{ArtifactManifest: manifest, CommittedAt: at},
		},
		{
			name: "missing bytes retry", stored: lifecycle.StoredObject{}, epoch: 3,
			want: lifecycle.CommitRetry, wantReason: lifecycle.RetryMissingBytes,
		},
		{
			name:   "digest mismatch retry",
			stored: lifecycle.StoredObject{Present: true, Digest: identity.DigestOf([]byte("other bytes")), Size: manifest.Size},
			epoch:  3, want: lifecycle.CommitRetry, wantReason: lifecycle.RetryDigestMismatch,
		},
		{
			name:   "size mismatch retry",
			stored: lifecycle.StoredObject{Present: true, Digest: manifest.Digest, Size: manifest.Size + 1},
			epoch:  3, want: lifecycle.CommitRetry, wantReason: lifecycle.RetrySizeMismatch,
		},
		{
			name: "stale epoch fenced", stored: storedMatching, epoch: 4,
			want: lifecycle.CommitFenced,
		},
		{
			name: "existing winner fenced", stored: storedMatching, epoch: 3, existing: &winner,
			want: lifecycle.CommitFenced, wantArtifact: winner,
		},
		{
			name: "existing same manifest committed", stored: storedMatching, epoch: 3, existing: &same,
			want: lifecycle.CommitCommitted, wantArtifact: same,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newArtifactStore(t, config, map[ids.OperationID]simulator.ArtifactOp{
				commit: {Reply: simulator.StoredReply(tc.stored), Effect: simulator.ManifestCommitted},
			})
			reply, err := store.Send(context.Background(), newRoleRequest(t, config, commit, contracts.RoleArtifactStore, contracts.MethodCommitManifest, nil))
			if err != nil {
				t.Fatalf("Send(commit_manifest) error = %v", err)
			}
			var stored lifecycle.StoredObject
			if err := json.Unmarshal(reply.Result, &stored); err != nil {
				t.Fatalf("commit reply is not a StoredObject: %v", err)
			}

			admission, err := lifecycle.AdmitArtifact(manifest, stored, tc.epoch, tc.existing, at)
			if err != nil {
				t.Fatalf("AdmitArtifact() error = %v", err)
			}
			if admission.Outcome != tc.want {
				t.Fatalf("Outcome = %q, want %q", admission.Outcome, tc.want)
			}
			if admission.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", admission.Reason, tc.wantReason)
			}
			if admission.Artifact != tc.wantArtifact {
				t.Fatalf("Artifact = %+v, want %+v", admission.Artifact, tc.wantArtifact)
			}
			if got := store.Ledger().Effects(commit); got != 1 {
				t.Fatalf("commit effects = %d, want 1", got)
			}
		})
	}
}

func TestArtifactCommitIsIdempotentAcrossRedelivery(t *testing.T) {
	config := testConfig(t)
	task := mustID[ids.Task](t)
	attempt := mustID[ids.Attempt](t)
	commit := mustID[ids.Operation](t)
	content := []byte("redelivered evidence")
	manifest := artifactManifest(t, config, task, attempt, commit, content, 3)
	stored := lifecycle.StoredObject{Present: true, Digest: manifest.Digest, Size: manifest.Size}
	store := newArtifactStore(t, config, map[ids.OperationID]simulator.ArtifactOp{
		commit: {Reply: simulator.StoredReply(stored), Effect: simulator.ManifestCommitted},
	})

	first := newRoleRequest(t, config, commit, contracts.RoleArtifactStore, contracts.MethodCommitManifest, nil)
	firstReply, err := store.Send(context.Background(), first)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	redelivery := first
	redelivery.Delivery = mustID[ids.Delivery](t)
	secondReply, err := store.Send(context.Background(), redelivery)
	if err != nil {
		t.Fatalf("Send() redelivery error = %v", err)
	}
	if secondReply.Delivery != redelivery.Delivery || string(secondReply.Result) != string(firstReply.Result) {
		t.Fatalf("redelivered reply = %+v, want the recorded result for the new delivery", secondReply)
	}
	if got := store.Ledger().Deliveries(commit); got != 2 {
		t.Fatalf("deliveries = %d, want 2", got)
	}
	if got := store.Ledger().Effects(commit); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}

	var firstStored, secondStored lifecycle.StoredObject
	if err := json.Unmarshal(firstReply.Result, &firstStored); err != nil {
		t.Fatalf("first reply is not a StoredObject: %v", err)
	}
	if err := json.Unmarshal(secondReply.Result, &secondStored); err != nil {
		t.Fatalf("second reply is not a StoredObject: %v", err)
	}
	firstAdmission, err := lifecycle.AdmitArtifact(manifest, firstStored, 3, nil, testNow)
	if err != nil {
		t.Fatalf("AdmitArtifact() error = %v", err)
	}
	secondAdmission, err := lifecycle.AdmitArtifact(manifest, secondStored, 3, &firstAdmission.Artifact, testNow)
	if err != nil {
		t.Fatalf("AdmitArtifact() on the redelivery error = %v", err)
	}
	if secondAdmission.Outcome != lifecycle.CommitCommitted || secondAdmission.Artifact != firstAdmission.Artifact {
		t.Fatalf("redelivered admission = %+v, want the same artifact committed", secondAdmission)
	}
}

func TestArtifactCrashAtAdmissionBoundary(t *testing.T) {
	config := testConfig(t)
	task := mustID[ids.Task](t)
	attempt := mustID[ids.Attempt](t)
	commit := mustID[ids.Operation](t)
	content := []byte("evidence in flight")
	manifest := artifactManifest(t, config, task, attempt, commit, content, 3)
	stored := lifecycle.StoredObject{Present: true, Digest: manifest.Digest, Size: manifest.Size}
	store := newArtifactStore(t, config, map[ids.OperationID]simulator.ArtifactOp{
		commit: {Reply: simulator.StoredReply(stored), Effect: simulator.ManifestCommitted},
	})

	request := newRoleRequest(t, config, commit, contracts.RoleArtifactStore, contracts.MethodCommitManifest, nil)
	pause := store.Control().Pause(commit, simulator.AfterEffect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abandoned := make(chan error, 1)
	go func() {
		_, err := store.Send(ctx, request)
		abandoned <- err
	}()
	<-pause.Reached()
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error after abandon = %v, want context.Canceled", err)
	}
	if got := store.Ledger().Effects(commit); got != 1 {
		t.Fatalf("effects after the abandoned call = %d, want 1", got)
	}

	redelivery := request
	redelivery.Delivery = mustID[ids.Delivery](t)
	reply, err := store.Send(context.Background(), redelivery)
	if err != nil {
		t.Fatalf("Send() redelivery error = %v", err)
	}
	var redelivered lifecycle.StoredObject
	if err := json.Unmarshal(reply.Result, &redelivered); err != nil {
		t.Fatalf("redelivered reply is not a StoredObject: %v", err)
	}
	admission, err := lifecycle.AdmitArtifact(manifest, redelivered, 3, nil, testNow)
	if err != nil {
		t.Fatalf("AdmitArtifact() error = %v", err)
	}
	if admission.Outcome != lifecycle.CommitCommitted || admission.Artifact != (lifecycle.Artifact{ArtifactManifest: manifest, CommittedAt: testNow}) {
		t.Fatalf("admission after the crash = %+v, want one committed artifact", admission)
	}
	if got := store.Ledger().Deliveries(commit); got != 2 {
		t.Fatalf("deliveries = %d, want 2", got)
	}
	if got := store.Ledger().Effects(commit); got != 1 {
		t.Fatalf("effects after the redelivery = %d, want 1", got)
	}
}
