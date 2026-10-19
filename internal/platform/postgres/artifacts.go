// Package postgres Artifact repositories persist manifests, artifacts, revisions and reviews.
// Admission decisions stay pure in lifecycle.AdmitArtifact; the store writes
// decided outcomes. Revisions and reviews are immutable: conflicts compare
// for replay instead of overwriting.
package postgres

import (
	"context"
	"errors"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// InsertManifest persists a declared manifest. The commit operation is the
// manifest's identity: a retry repeats it and replays.
func InsertManifest(ctx context.Context, u *UnitOfWork, manifest lifecycle.ArtifactManifest) (lifecycle.ArtifactManifest, bool, error) {
	if err := manifest.Validate(); err != nil {
		return lifecycle.ArtifactManifest{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into artifact_manifest (tenant_id, operation_id, artifact_id, task_id, attempt_id, producer_id, kind, digest, size_bytes, media_type, source_revision, writer_epoch)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		manifest.Tenant.String(), manifest.Operation.String(), manifest.Artifact.String(),
		manifest.Task.String(), manifest.Attempt.String(), manifest.Producer.String(),
		string(manifest.Kind), string(manifest.Digest), int64(manifest.Size), manifest.MediaType,
		textOrNil(manifest.SourceRevision), int64(manifest.WriterEpoch),
	)
	if err != nil {
		return lifecycle.ArtifactManifest{}, false, err
	}
	if inserted {
		return manifest, false, nil
	}
	stored, loadErr := GetManifest(ctx, u, manifest.Tenant, manifest.Operation)
	if isNotFound(loadErr) {
		return lifecycle.ArtifactManifest{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.ArtifactManifest{}, false, loadErr
	}
	if stored != manifest {
		return lifecycle.ArtifactManifest{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetManifest loads a manifest by tenant and commit operation.
func GetManifest(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID) (lifecycle.ArtifactManifest, error) {
	var manifest lifecycle.ArtifactManifest
	var kind, digest, mediaType, tenantText, operationText, artifactText, taskText, attemptText, producerText string
	var sourceText *string
	var size, epoch int64
	err := q.QueryRow(ctx, `
		select tenant_id::text, operation_id::text, artifact_id::text, task_id::text, attempt_id::text, producer_id::text,
			kind, digest, size_bytes, media_type, source_revision, writer_epoch
		from artifact_manifest where tenant_id = $1 and operation_id = $2`,
		tenant.String(), operation.String(),
	).Scan(&tenantText, &operationText, &artifactText, &taskText, &attemptText, &producerText,
		&kind, &digest, &size, &mediaType, &sourceText, &epoch)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ArtifactManifest{}, ErrNotFound
		}
		return lifecycle.ArtifactManifest{}, err
	}
	var parseErr error
	if manifest.Tenant, parseErr = parseID[ids.Tenant](`artifact_manifest.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	if manifest.Operation, parseErr = parseID[ids.Operation](`artifact_manifest.operation_id`, operationText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	if manifest.Artifact, parseErr = parseID[ids.Artifact](`artifact_manifest.artifact_id`, artifactText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	if manifest.Task, parseErr = parseID[ids.Task](`artifact_manifest.task_id`, taskText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	if manifest.Attempt, parseErr = parseID[ids.Attempt](`artifact_manifest.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	if manifest.Producer, parseErr = parseID[ids.Provider](`artifact_manifest.producer_id`, producerText); parseErr != nil {
		return lifecycle.ArtifactManifest{}, parseErr
	}
	manifest.Kind = lifecycle.ArtifactKind(kind)
	manifest.Digest = identityDigest(digest)
	manifest.Size = uint64(size)
	manifest.MediaType = mediaType
	if sourceText != nil {
		manifest.SourceRevision = *sourceText
	}
	manifest.WriterEpoch = uint64(epoch)
	if err := manifest.Validate(); err != nil {
		return lifecycle.ArtifactManifest{}, err
	}
	return manifest, nil
}

// InsertArtifact persists a committed artifact with replay comparison.
func InsertArtifact(ctx context.Context, u *UnitOfWork, artifact lifecycle.Artifact) (lifecycle.Artifact, bool, error) {
	if err := artifact.Validate(); err != nil {
		return lifecycle.Artifact{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into artifact (tenant_id, artifact_id, operation_id, task_id, attempt_id, producer_id, kind, digest, size_bytes, media_type, source_revision, writer_epoch, committed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		artifact.Tenant.String(), artifact.Artifact.String(), artifact.Operation.String(),
		artifact.Task.String(), artifact.Attempt.String(), artifact.Producer.String(),
		string(artifact.Kind), string(artifact.Digest), int64(artifact.Size), artifact.MediaType,
		textOrNil(artifact.SourceRevision), int64(artifact.WriterEpoch), artifact.CommittedAt,
	)
	if err != nil {
		return lifecycle.Artifact{}, false, err
	}
	if inserted {
		return artifact, false, nil
	}
	stored, loadErr := GetArtifact(ctx, u, artifact.Tenant, artifact.Artifact)
	if isNotFound(loadErr) {
		return lifecycle.Artifact{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.Artifact{}, false, loadErr
	}
	if !artifactsEqual(stored, artifact) {
		return lifecycle.Artifact{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetArtifact loads a committed artifact by tenant and id.
func GetArtifact(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ArtifactID) (lifecycle.Artifact, error) {
	var artifact lifecycle.Artifact
	var kind, digest, mediaType, tenantText, operationText, taskText, attemptText, producerText string
	var sourceText *string
	var size, epoch int64
	err := q.QueryRow(ctx, `
		select tenant_id::text, operation_id::text, task_id::text, attempt_id::text, producer_id::text,
			kind, digest, size_bytes, media_type, source_revision, writer_epoch, committed_at
		from artifact where tenant_id = $1 and artifact_id = $2`,
		tenant.String(), id.String(),
	).Scan(&tenantText, &operationText, &taskText, &attemptText, &producerText,
		&kind, &digest, &size, &mediaType, &sourceText, &epoch, &artifact.CommittedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.Artifact{}, ErrNotFound
		}
		return lifecycle.Artifact{}, err
	}
	var parseErr error
	artifact.Artifact = id
	if artifact.Tenant, parseErr = parseID[ids.Tenant](`artifact.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.Artifact{}, parseErr
	}
	if artifact.Operation, parseErr = parseID[ids.Operation](`artifact.operation_id`, operationText); parseErr != nil {
		return lifecycle.Artifact{}, parseErr
	}
	if artifact.Task, parseErr = parseID[ids.Task](`artifact.task_id`, taskText); parseErr != nil {
		return lifecycle.Artifact{}, parseErr
	}
	if artifact.Attempt, parseErr = parseID[ids.Attempt](`artifact.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.Artifact{}, parseErr
	}
	if artifact.Producer, parseErr = parseID[ids.Provider](`artifact.producer_id`, producerText); parseErr != nil {
		return lifecycle.Artifact{}, parseErr
	}
	artifact.Kind = lifecycle.ArtifactKind(kind)
	artifact.Digest = identityDigest(digest)
	artifact.Size = uint64(size)
	artifact.MediaType = mediaType
	if sourceText != nil {
		artifact.SourceRevision = *sourceText
	}
	artifact.WriterEpoch = uint64(epoch)
	if err := artifact.Validate(); err != nil {
		return lifecycle.Artifact{}, err
	}
	return artifact, nil
}

func artifactsEqual(a, b lifecycle.Artifact) bool {
	return a.ArtifactManifest == b.ArtifactManifest && sameInstant(a.CommittedAt, b.CommittedAt)
}

// InsertRevision persists an immutable revision with its artifact links.
// The revision and every link commit together; conflicts replay on equality.
func InsertRevision(ctx context.Context, u *UnitOfWork, revision lifecycle.ResultRevision) (lifecycle.ResultRevision, bool, error) {
	if err := revision.Validate(); err != nil {
		return lifecycle.ResultRevision{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into result_revision (tenant_id, revision_id, task_id, attempt_id, number, previous_revision_id, created_at)
		values ($1, $2, $3, $4, $5, $6, $7)`,
		revision.Tenant.String(), revision.ID.String(), revision.Task.String(), revision.Attempt.String(),
		int32(revision.Number), uuidOrNil(revision.Previous), revision.CreatedAt,
	)
	if err != nil {
		return lifecycle.ResultRevision{}, false, err
	}
	if !inserted {
		stored, loadErr := GetRevision(ctx, u, revision.Tenant, revision.ID)
		if loadErr != nil {
			if isNotFound(loadErr) {
				return lifecycle.ResultRevision{}, false, ErrPayloadMismatch
			}
			return lifecycle.ResultRevision{}, false, loadErr
		}
		if !revisionsEqual(stored, revision) {
			return lifecycle.ResultRevision{}, false, ErrPayloadMismatch
		}
		return stored, true, nil
	}
	for _, artifactID := range revision.Artifacts {
		if _, err := insertReturning(ctx, u, `
			insert into revision_artifact (tenant_id, revision_id, artifact_id) values ($1, $2, $3)`,
			revision.Tenant.String(), revision.ID.String(), artifactID.String(),
		); err != nil {
			return lifecycle.ResultRevision{}, false, err
		}
	}
	for _, id := range revision.Verifications {
		if _, err := u.Exec(ctx, `insert into revision_verification(tenant_id,revision_id,verification_id) values($1,$2,$3)`, revision.Tenant.String(), revision.ID.String(), id.String()); err != nil {
			return lifecycle.ResultRevision{}, false, err
		}
	}
	return revision, false, nil
}

// GetRevision loads a revision with its artifact links in stored order.
func GetRevision(ctx context.Context, q Querier, tenant ids.TenantID, id ids.RevisionID) (lifecycle.ResultRevision, error) {
	var revision lifecycle.ResultRevision
	var idText, tenantText, taskText, attemptText string
	var number int32
	var previousText *string
	err := q.QueryRow(ctx, `
		select revision_id::text, tenant_id::text, task_id::text, attempt_id::text, number, previous_revision_id::text, created_at
		from result_revision where tenant_id = $1 and revision_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &attemptText, &number, &previousText, &revision.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ResultRevision{}, ErrNotFound
		}
		return lifecycle.ResultRevision{}, err
	}
	var parseErr error
	if revision.ID, parseErr = parseID[ids.Revision](`result_revision.revision_id`, idText); parseErr != nil {
		return lifecycle.ResultRevision{}, parseErr
	}
	if revision.Tenant, parseErr = parseID[ids.Tenant](`result_revision.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.ResultRevision{}, parseErr
	}
	if revision.Task, parseErr = parseID[ids.Task](`result_revision.task_id`, taskText); parseErr != nil {
		return lifecycle.ResultRevision{}, parseErr
	}
	if revision.Attempt, parseErr = parseID[ids.Attempt](`result_revision.attempt_id`, attemptText); parseErr != nil {
		return lifecycle.ResultRevision{}, parseErr
	}
	if revision.Previous, parseErr = parseOptionalID[ids.Revision](`result_revision.previous_revision_id`, previousText); parseErr != nil {
		return lifecycle.ResultRevision{}, parseErr
	}
	revision.Number = uint32(number)
	rows, err := q.Query(ctx, `
		select artifact_id::text from revision_artifact
		where tenant_id = $1 and revision_id = $2 order by artifact_id`,
		tenant.String(), id.String(),
	)
	if err != nil {
		return lifecycle.ResultRevision{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifactText string
		if err := rows.Scan(&artifactText); err != nil {
			return lifecycle.ResultRevision{}, err
		}
		artifactID, parseErr := parseID[ids.Artifact](`revision_artifact.artifact_id`, artifactText)
		if parseErr != nil {
			return lifecycle.ResultRevision{}, parseErr
		}
		revision.Artifacts = append(revision.Artifacts, artifactID)
	}
	if err := rows.Err(); err != nil {
		return lifecycle.ResultRevision{}, err
	}
	rows.Close()
	verified, err := q.Query(ctx, `select verification_id::text from revision_verification where tenant_id=$1 and revision_id=$2 order by verification_id`, tenant.String(), id.String())
	if err != nil {
		return lifecycle.ResultRevision{}, err
	}
	defer verified.Close()
	for verified.Next() {
		var text string
		if err = verified.Scan(&text); err != nil {
			return lifecycle.ResultRevision{}, err
		}
		id, err := ids.Parse[ids.Verification](text)
		if err != nil {
			return lifecycle.ResultRevision{}, err
		}
		revision.Verifications = append(revision.Verifications, id)
	}
	if err = verified.Err(); err != nil {
		return lifecycle.ResultRevision{}, err
	}
	if err := revision.Validate(); err != nil {
		return lifecycle.ResultRevision{}, err
	}
	return revision, nil
}

func revisionsEqual(a, b lifecycle.ResultRevision) bool {
	if a.ID != b.ID || a.Tenant != b.Tenant || a.Task != b.Task || a.Attempt != b.Attempt ||
		a.Number != b.Number || a.Previous != b.Previous || !sameInstant(a.CreatedAt, b.CreatedAt) ||
		len(a.Artifacts) != len(b.Artifacts) {
		return false
	}
	if len(a.Verifications) != len(b.Verifications) {
		return false
	}
	verification := make(map[ids.VerificationID]bool, len(a.Verifications))
	for _, id := range a.Verifications {
		verification[id] = true
	}
	for _, id := range b.Verifications {
		if !verification[id] {
			return false
		}
	}
	// Artifact order is not significant: storage returns links ordered by id
	// while callers preserve insertion order, so compare as a set.
	seen := make(map[ids.ArtifactID]int, len(a.Artifacts))
	for _, id := range a.Artifacts {
		seen[id]++
	}
	for _, id := range b.Artifacts {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	return true
}

// RecordReview applies a review to its task at the version the reviewer saw:
// the review row, the moved task, and history commit together. Only the
// first of concurrent reviews takes effect; the other is a version conflict.
// Replaying a recorded review returns it.
func RecordReview(ctx context.Context, u *UnitOfWork, current lifecycle.ResultRevision, review lifecycle.ResultReview, reviewers []ids.PrincipalID, tr Transition) (lifecycle.Task, lifecycle.ResultReview, bool, error) {
	if err := review.Validate(); err != nil {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	if stored, err := GetReview(ctx, u, review.Tenant, review.ID); err == nil {
		if !reviewsEqual(stored, review) {
			return lifecycle.Task{}, lifecycle.ResultReview{}, false, ErrPayloadMismatch
		}
		task, err := GetTask(ctx, u, review.Tenant, review.Task)
		if err != nil {
			return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
		}
		if task.Version != review.TaskVersion {
			// The review already moved the task (or something else did):
			// the recorded decision stands, report the replay.
			return task, stored, true, nil
		}
		// The review row exists but the task never moved: a crash landed
		// between the insert and the update. Complete the move now.
		next, change, applyErr := lifecycle.ApplyReview(task, current, review, reviewers)
		if applyErr != nil {
			return lifecycle.Task{}, lifecycle.ResultReview{}, false, applyErr
		}
		if err := updateTaskForReview(ctx, u, review, next); err != nil {
			return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
		}
		audit := change.Audit(tr.AuditID, tr.Actor, review.ReviewedAt)
		if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
			return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
		}
		return next, stored, true, nil
	} else if !isNotFound(err) {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	task, err := GetTaskForUpdate(ctx, u, review.Tenant, review.Task)
	if err != nil {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	next, change, applyErr := lifecycle.ApplyReview(task, current, review, reviewers)
	if applyErr != nil {
		if terr, ok := applyErr.(*lifecycle.TransitionError); ok {
			audit := terr.Audit(tr.AuditID, tr.Actor, review.ReviewedAt)
			if histErr := appendRefusal(ctx, u, audit); histErr != nil {
				return lifecycle.Task{}, lifecycle.ResultReview{}, false, histErr
			}
		}
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, applyErr
	}
	if _, _, err := InsertReviewRow(ctx, u, review); err != nil {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	if err := updateTaskForReview(ctx, u, review, next); err != nil {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	audit := change.Audit(tr.AuditID, tr.Actor, review.ReviewedAt)
	if err := appendHistory(ctx, u, change, audit, tr.Outbox); err != nil {
		return lifecycle.Task{}, lifecycle.ResultReview{}, false, err
	}
	return next, review, false, nil
}

// updateTaskForReview moves the reviewed task with a version predicate so a
// concurrent review loses exactly once.
func updateTaskForReview(ctx context.Context, u *UnitOfWork, review lifecycle.ResultReview, next lifecycle.Task) error {
	if err := execVersionedUpdate(ctx, u, `
		update task set state = $3, version = $4, updated_at = $5
		where tenant_id = $1 and task_id = $2 and version = $6`,
		review.Tenant.String(), review.Task.String(), string(next.State),
		int64(next.Version), next.UpdatedAt, int64(review.TaskVersion),
	); err != nil {
		return err
	}
	return nil
}

// InsertReviewRow persists a review row without moving its task.
func InsertReviewRow(ctx context.Context, u *UnitOfWork, review lifecycle.ResultReview) (lifecycle.ResultReview, bool, error) {
	if err := review.Validate(); err != nil {
		return lifecycle.ResultReview{}, false, err
	}
	inserted, err := insertReturning(ctx, u, `
		insert into result_review (tenant_id, review_id, task_id, revision_id, reviewer_id, disposition, task_version, reviewed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		review.Tenant.String(), review.ID.String(), review.Task.String(), review.Revision.String(),
		review.Reviewer.String(), string(review.Disposition), int64(review.TaskVersion), review.ReviewedAt,
	)
	if err != nil {
		return lifecycle.ResultReview{}, false, err
	}
	if inserted {
		return review, false, nil
	}
	stored, loadErr := GetReview(ctx, u, review.Tenant, review.ID)
	if isNotFound(loadErr) {
		return lifecycle.ResultReview{}, false, ErrPayloadMismatch
	}
	if loadErr != nil {
		return lifecycle.ResultReview{}, false, loadErr
	}
	if !reviewsEqual(stored, review) {
		return lifecycle.ResultReview{}, false, ErrPayloadMismatch
	}
	return stored, true, nil
}

// GetReview loads a review by tenant and id.
func GetReview(ctx context.Context, q Querier, tenant ids.TenantID, id ids.ReviewID) (lifecycle.ResultReview, error) {
	var review lifecycle.ResultReview
	var disposition, idText, tenantText, taskText, revisionText, reviewerText string
	err := q.QueryRow(ctx, `
		select review_id::text, tenant_id::text, task_id::text, revision_id::text, reviewer_id::text, disposition, task_version, reviewed_at
		from result_review where tenant_id = $1 and review_id = $2`,
		tenant.String(), id.String(),
	).Scan(&idText, &tenantText, &taskText, &revisionText, &reviewerText, &disposition, &review.TaskVersion, &review.ReviewedAt)
	if err != nil {
		if isNoRows(err) {
			return lifecycle.ResultReview{}, ErrNotFound
		}
		return lifecycle.ResultReview{}, err
	}
	var parseErr error
	if review.ID, parseErr = parseID[ids.Review](`result_review.review_id`, idText); parseErr != nil {
		return lifecycle.ResultReview{}, parseErr
	}
	if review.Tenant, parseErr = parseID[ids.Tenant](`result_review.tenant_id`, tenantText); parseErr != nil {
		return lifecycle.ResultReview{}, parseErr
	}
	if review.Task, parseErr = parseID[ids.Task](`result_review.task_id`, taskText); parseErr != nil {
		return lifecycle.ResultReview{}, parseErr
	}
	if review.Revision, parseErr = parseID[ids.Revision](`result_review.revision_id`, revisionText); parseErr != nil {
		return lifecycle.ResultReview{}, parseErr
	}
	if review.Reviewer, parseErr = parseID[ids.Principal](`result_review.reviewer_id`, reviewerText); parseErr != nil {
		return lifecycle.ResultReview{}, parseErr
	}
	review.Disposition = lifecycle.Disposition(disposition)
	if err := review.Validate(); err != nil {
		return lifecycle.ResultReview{}, err
	}
	return review, nil
}

func reviewsEqual(a, b lifecycle.ResultReview) bool {
	return a.ID == b.ID && a.Tenant == b.Tenant && a.Task == b.Task && a.Revision == b.Revision &&
		a.Reviewer == b.Reviewer && a.Disposition == b.Disposition && a.TaskVersion == b.TaskVersion &&
		sameInstant(a.ReviewedAt, b.ReviewedAt)
}

// isNotFound reports ErrNotFound so replays distinguish absence from failure.
func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
