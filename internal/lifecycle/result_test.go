package lifecycle

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var otherDigest = identity.Digest("sha256:" + strings.Repeat("d", 64))

func newManifest(t *testing.T, task Task) ArtifactManifest {
	t.Helper()
	return ArtifactManifest{
		Artifact:    newID[ids.Artifact](t),
		Tenant:      task.Tenant,
		Task:        task.ID,
		Attempt:     newID[ids.Attempt](t),
		Operation:   newID[ids.Operation](t),
		Producer:    newID[ids.Provider](t),
		Kind:        ArtifactCodeChange,
		Digest:      testDigest,
		Size:        2048,
		MediaType:   "text/x-diff",
		WriterEpoch: 2,
	}
}

func stored(m ArtifactManifest) StoredObject {
	return StoredObject{Present: true, Digest: m.Digest, Size: m.Size}
}

func commit(t *testing.T, m ArtifactManifest) Artifact {
	t.Helper()
	admission, err := AdmitArtifact(m, stored(m), m.WriterEpoch, nil, testNow)
	if err != nil || admission.Outcome != CommitCommitted {
		t.Fatalf("AdmitArtifact() = %s, %v", admission.Outcome, err)
	}
	return admission.Artifact
}

func TestAdmitArtifactCommitsOnlyVerifiedBytes(t *testing.T) {
	task := newTask(t)
	m := newManifest(t, task)
	admission, err := AdmitArtifact(m, stored(m), 2, nil, testNow)
	if err != nil || admission.Outcome != CommitCommitted {
		t.Fatalf("AdmitArtifact() = %s, %v", admission.Outcome, err)
	}
	a := admission.Artifact
	if a.Digest != m.Digest || a.Size != m.Size || a.Operation != m.Operation || a.Producer != m.Producer ||
		a.WriterEpoch != m.WriterEpoch || !a.CommittedAt.Equal(testNow) {
		t.Fatalf("committed artifact %+v does not record the manifest exactly", a)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := admission.Audit(newID[ids.Audit](t), testSystem, testNow).Validate(); err != nil {
		t.Fatalf("commit audit invalid: %v", err)
	}

	again, err := AdmitArtifact(m, stored(m), 2, &a, testNow.Add(time.Minute))
	if err != nil || again.Outcome != CommitCommitted || again.Artifact != a {
		t.Fatalf("recommit = %s %+v, %v; want the same artifact", again.Outcome, again.Artifact, err)
	}
}

func TestAdmitArtifactRetryRegistersNothing(t *testing.T) {
	task := newTask(t)
	m := newManifest(t, task)
	for reason, observed := range map[RetryReason]StoredObject{
		RetryMissingBytes:   {},
		RetryDigestMismatch: {Present: true, Digest: otherDigest, Size: m.Size},
		RetrySizeMismatch:   {Present: true, Digest: m.Digest, Size: m.Size + 1},
	} {
		admission, err := AdmitArtifact(m, observed, 2, nil, testNow)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if admission.Outcome != CommitRetry || admission.Reason != reason || admission.Artifact != (Artifact{}) {
			t.Fatalf("%s: %s/%s with artifact %+v", reason, admission.Outcome, admission.Reason, admission.Artifact)
		}
		audit := admission.Audit(newID[ids.Audit](t), testSystem, testNow)
		if audit.Validate() != nil || audit.Reason != RejectAdmissionRetry {
			t.Fatalf("%s: audit %+v", reason, audit)
		}
	}
	// The same operation commits once the bytes are right.
	if admission, _ := AdmitArtifact(m, stored(m), 2, nil, testNow); admission.Outcome != CommitCommitted {
		t.Fatalf("retry of the same operation = %s", admission.Outcome)
	}
}

func TestAdmitArtifactFencesStaleWriters(t *testing.T) {
	task := newTask(t)
	m := newManifest(t, task)
	stale, err := AdmitArtifact(m, stored(m), m.WriterEpoch+1, nil, testNow)
	if err != nil || stale.Outcome != CommitFenced || stale.Artifact != (Artifact{}) {
		t.Fatalf("stale epoch = %s %+v, %v", stale.Outcome, stale.Artifact, err)
	}
	if audit := stale.Audit(newID[ids.Audit](t), testSystem, testNow); audit.Reason != RejectStaleWriter || audit.Validate() != nil {
		t.Fatalf("fenced audit %+v", audit)
	}

	winner := commit(t, m)
	loser := m
	loser.Operation, loser.Digest = newID[ids.Operation](t), otherDigest
	fenced, err := AdmitArtifact(loser, stored(loser), loser.WriterEpoch, &winner, testNow)
	if err != nil || fenced.Outcome != CommitFenced || fenced.Artifact != winner {
		t.Fatalf("competing manifest = %s %+v, %v; want fenced with the winner unchanged", fenced.Outcome, fenced.Artifact, err)
	}
}

func TestAdmitArtifactRejectsMalformedInput(t *testing.T) {
	task := newTask(t)
	m := newManifest(t, task)
	unrelated := commit(t, newManifest(t, task))
	for name, call := range map[string]func() error{
		"zero epoch": func() error { _, err := AdmitArtifact(m, stored(m), 0, nil, testNow); return err },
		"bad media type": func() error {
			x := m
			x.MediaType = "diff"
			_, err := AdmitArtifact(x, stored(x), 2, nil, testNow)
			return err
		},
		"unknown kind": func() error {
			x := m
			x.Kind = "log"
			_, err := AdmitArtifact(x, stored(x), 2, nil, testNow)
			return err
		},
		"bad digest": func() error {
			x := m
			x.Digest = "md5:1"
			_, err := AdmitArtifact(x, stored(x), 2, nil, testNow)
			return err
		},
		"unrelated existing": func() error { _, err := AdmitArtifact(m, stored(m), 2, &unrelated, testNow); return err },
	} {
		if !errors.Is(call(), ErrInvalid) {
			t.Errorf("%s: want ErrInvalid", name)
		}
	}
}

func TestResultRevisionRequiresCommittedArtifactsOfItsTask(t *testing.T) {
	task := newTask(t)
	a := commit(t, newManifest(t, task))
	b := commit(t, newManifest(t, task))
	first, err := NewResultRevision(newID[ids.Revision](t), a.Attempt, []Artifact{a, b}, nil, testNow)
	if err != nil || first.Number != 1 || !first.Previous.IsZero() || len(first.Artifacts) != 2 {
		t.Fatalf("NewResultRevision() = %+v, %v", first, err)
	}

	uncommitted := a
	uncommitted.CommittedAt = time.Time{}
	foreign := commit(t, newManifest(t, newTask(t)))
	for name, artifacts := range map[string][]Artifact{
		"none":        nil,
		"uncommitted": {uncommitted},
		"other task":  {a, foreign},
		"duplicate":   {a, a},
	} {
		if _, err := NewResultRevision(newID[ids.Revision](t), a.Attempt, artifacts, nil, testNow); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: NewResultRevision() = %v, want ErrInvalid", name, err)
		}
	}
}

func TestReworkLineageNeverMutatesTheReviewedRevision(t *testing.T) {
	task := newTask(t)
	a := commit(t, newManifest(t, task))
	first, _ := NewResultRevision(newID[ids.Revision](t), a.Attempt, []Artifact{a}, nil, testNow)
	snapshot := first
	snapshot.Artifacts = append([]ids.ArtifactID(nil), first.Artifacts...)

	b := commit(t, newManifest(t, task))
	second, err := NewResultRevision(newID[ids.Revision](t), b.Attempt, []Artifact{b}, &first, testNow)
	if err != nil || second.Number != 2 || second.Previous != first.ID {
		t.Fatalf("rework revision = %+v, %v", second, err)
	}
	if first.ID != snapshot.ID || first.Number != snapshot.Number || first.Artifacts[0] != snapshot.Artifacts[0] {
		t.Fatal("creating a rework revision changed the reviewed revision")
	}

	foreign, _ := NewResultRevision(newID[ids.Revision](t), a.Attempt, []Artifact{commit(t, newManifest(t, newTask(t)))}, nil, testNow)
	if _, err := NewResultRevision(newID[ids.Revision](t), b.Attempt, []Artifact{b}, &foreign, testNow); !errors.Is(err, ErrInvalid) {
		t.Fatalf("previous revision of another task = %v, want ErrInvalid", err)
	}
}

// reviewable returns a task ready for review and its current revision.
func reviewable(t *testing.T) (Task, ResultRevision) {
	t.Helper()
	task := driveTask(t, newTask(t),
		TaskSubmit, TaskState(""), TaskDispatch, TaskState(""), TaskProposeResult, TaskState(""),
		TaskVerificationPassed, TaskState(""))
	a := commit(t, newManifest(t, task))
	revision, err := NewResultRevision(newID[ids.Revision](t), a.Attempt, []Artifact{a}, nil, testNow)
	if err != nil {
		t.Fatalf("NewResultRevision() error = %v", err)
	}
	return task, revision
}

func newReview(t *testing.T, task Task, revision ResultRevision, d Disposition) ResultReview {
	t.Helper()
	return ResultReview{
		ID: newID[ids.Review](t), Tenant: task.Tenant, Task: task.ID, Revision: revision.ID,
		Reviewer: task.Owner, Disposition: d, TaskVersion: task.Version, ReviewedAt: testNow,
	}
}

func TestReviewDispositionsDriveTheTask(t *testing.T) {
	for d, want := range map[Disposition]TaskState{
		DispositionAccept:         TaskSucceeded,
		DispositionRequestChanges: TaskQueued,
		DispositionReject:         TaskRejected,
	} {
		task, revision := reviewable(t)
		next, change, err := ApplyReview(task, revision, newReview(t, task, revision, d), nil)
		if err != nil || next.State != want {
			t.Fatalf("%s = %s, %v; want %s", d, next.State, err, want)
		}
		if err := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: task.Owner}, testNow).Validate(); err != nil {
			t.Fatalf("%s audit invalid: %v", d, err)
		}
	}
}

func TestConcurrentReviewsConflict(t *testing.T) {
	task, revision := reviewable(t)
	accept := newReview(t, task, revision, DispositionAccept)
	reject := newReview(t, task, revision, DispositionReject)
	accepted, _, err := ApplyReview(task, revision, accept, nil)
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	_, _, err = ApplyReview(accepted, revision, reject, nil)
	testRejectCode(t, err, RejectVersionConflict)
}

func TestReviewMustBindCurrentRevisionAndAuthorizedReviewer(t *testing.T) {
	task, revision := reviewable(t)
	delegate := newID[ids.Principal](t)
	cases := map[RejectionCode]func(r *ResultReview, current *ResultRevision){
		RejectNotCurrentRevision: func(r *ResultReview, current *ResultRevision) { r.Revision = newID[ids.Revision](t) },
		RejectUnauthorizedActor:  func(r *ResultReview, current *ResultRevision) { r.Reviewer = newID[ids.Principal](t) },
		RejectCrossTenant:        func(r *ResultReview, current *ResultRevision) { r.Tenant = newID[ids.Tenant](t) },
		RejectWrongRecord:        func(r *ResultReview, current *ResultRevision) { r.Task = newID[ids.Task](t) },
		RejectVersionConflict:    func(r *ResultReview, current *ResultRevision) { r.TaskVersion-- },
	}
	for code, mutate := range cases {
		t.Run(string(code), func(t *testing.T) {
			review := newReview(t, task, revision, DispositionAccept)
			current := revision
			mutate(&review, &current)
			next, _, err := ApplyReview(task, current, review, []ids.PrincipalID{delegate})
			testRejectCode(t, err, code)
			if next.State != TaskReadyForReview {
				t.Fatalf("rejected review moved task to %s", next.State)
			}
		})
	}

	review := newReview(t, task, revision, DispositionAccept)
	review.Reviewer = delegate
	if next, _, err := ApplyReview(task, revision, review, []ids.PrincipalID{delegate}); err != nil || next.State != TaskSucceeded {
		t.Fatalf("authorized delegate review = %s, %v", next.State, err)
	}
}

func TestReviewBeforeReadyIsRejected(t *testing.T) {
	_, revision := reviewable(t)
	running := driveTask(t, newTask(t), TaskSubmit, TaskState(""), TaskDispatch, TaskState(""))
	revision.Tenant, revision.Task = running.Tenant, running.ID
	_, _, err := ApplyReview(running, revision, newReview(t, running, revision, DispositionAccept), nil)
	testRejectCode(t, err, RejectInvalidTransition)
}
