package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// ResultProposal is immutable provider evidence with product-owned identities.
type ResultProposal struct {
	ID        ids.ProposalID
	Tenant    ids.TenantID
	Task      ids.TaskID
	Attempt   ids.AttemptID
	Provider  ids.ProviderID
	Operation ids.OperationID
	Epoch     uint64
	Payload   contracts.ResultProposed
	CreatedAt time.Time
}

// LockResultScope serializes the two halves of the proposal/turn join and
// admission. Attempt precedes Task, matching progress's existing lock order.
func LockResultScope(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, attemptID ids.AttemptID) (lifecycle.Attempt, lifecycle.Task, error) {
	a, err := GetAttemptForUpdate(ctx, u, tenant, attemptID)
	if err != nil {
		return a, lifecycle.Task{}, err
	}
	t, err := GetTaskForUpdate(ctx, u, tenant, a.Task)
	return a, t, err
}

func GetResultProposal(ctx context.Context, q Querier, tenant ids.TenantID, attempt ids.AttemptID) (ResultProposal, error) {
	var p ResultProposal
	var id, task, provider, operation string
	var raw []byte
	err := q.QueryRow(ctx, `select proposal_id::text,task_id::text,provider_id::text,operation_id::text,writer_epoch,payload,created_at from result_proposal where tenant_id=$1 and attempt_id=$2`, tenant.String(), attempt.String()).Scan(&id, &task, &provider, &operation, &p.Epoch, &raw, &p.CreatedAt)
	if isNoRows(err) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Tenant, p.Attempt = tenant, attempt
	if p.ID, err = ids.Parse[ids.Proposal](id); err != nil {
		return p, err
	}
	if p.Task, err = ids.Parse[ids.Task](task); err != nil {
		return p, err
	}
	if p.Provider, err = ids.Parse[ids.Provider](provider); err != nil {
		return p, err
	}
	if p.Operation, err = ids.Parse[ids.Operation](operation); err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p.Payload); err != nil {
		return p, err
	}
	return p, p.Payload.Validate()
}

// InsertResultProposal requires LockResultScope; a retry cannot replace its
// evidence even under a different stream cursor or delivery.
func InsertResultProposal(ctx context.Context, u *UnitOfWork, p ResultProposal) (ResultProposal, bool, error) {
	if err := p.Payload.Validate(); err != nil {
		return p, false, err
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return p, false, err
	}
	old, err := GetResultProposal(ctx, u, p.Tenant, p.Attempt)
	if err == nil {
		b, e := json.Marshal(old.Payload)
		if e != nil {
			return p, false, e
		}
		if string(raw) != string(b) || old.Provider != p.Provider || old.Operation != p.Operation {
			return p, false, ErrPayloadMismatch
		}
		return old, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return p, false, err
	}
	if err = u.QueryRow(ctx, `update attempt set artifact_writer_epoch=artifact_writer_epoch+1 where tenant_id=$1 and attempt_id=$2 returning artifact_writer_epoch`, p.Tenant.String(), p.Attempt.String()).Scan(&p.Epoch); err != nil {
		return p, false, err
	}
	if p.ID, err = ids.New[ids.Proposal](); err != nil {
		return p, false, err
	}
	_, err = u.Exec(ctx, `insert into result_proposal(tenant_id,proposal_id,task_id,attempt_id,provider_id,operation_id,writer_epoch,payload,created_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.Tenant.String(), p.ID.String(), p.Task.String(), p.Attempt.String(), p.Provider.String(), p.Operation.String(), int64(p.Epoch), raw, p.CreatedAt)
	return p, false, err
}

// ArtifactWork contains only immutable references and stage identities. Store
// identity is bound once by the worker registry before the first send.
type ArtifactWork struct {
	Proposal  ids.ProposalID
	Manifest  lifecycle.ArtifactManifest
	NativeRef string
	Authorize ids.OperationID
	Export    ids.OperationID
	Stat      ids.OperationID
	Store     ids.ProviderID
	Deadline  time.Time
}

func GetArtifactWork(ctx context.Context, q Querier, tenant ids.TenantID, operation ids.OperationID) (ArtifactWork, error) {
	var w ArtifactWork
	var p, auth, export, stat string
	var provider *string
	var raw []byte
	err := q.QueryRow(ctx, `select proposal_id::text,manifest,native_ref,authorize_id::text,export_id::text,stat_id::text,store_id::text,deadline from artifact_work where tenant_id=$1 and operation_id=$2`, tenant.String(), operation.String()).Scan(&p, &raw, &w.NativeRef, &auth, &export, &stat, &provider, &w.Deadline)
	if isNoRows(err) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	if err = json.Unmarshal(raw, &w.Manifest); err != nil {
		return w, err
	}
	if err = w.Manifest.Validate(); err != nil {
		return w, err
	}
	if w.Proposal, err = ids.Parse[ids.Proposal](p); err != nil {
		return w, err
	}
	if w.Authorize, err = ids.Parse[ids.Operation](auth); err != nil {
		return w, err
	}
	if w.Export, err = ids.Parse[ids.Operation](export); err != nil {
		return w, err
	}
	if w.Stat, err = ids.Parse[ids.Operation](stat); err != nil {
		return w, err
	}
	if provider != nil {
		w.Store, err = ids.Parse[ids.Provider](*provider)
	}
	return w, err
}

// EnqueueArtifacts is called once at proposal activation under scope locks.
func EnqueueArtifacts(ctx context.Context, u *UnitOfWork, p ResultProposal, at time.Time) error {
	for _, d := range p.Payload.Artifacts {
		artifact, err := ids.New[ids.Artifact]()
		if err != nil {
			return err
		}
		commit, err := ids.New[ids.Operation]()
		if err != nil {
			return err
		}
		auth, err := ids.New[ids.Operation]()
		if err != nil {
			return err
		}
		export, err := ids.New[ids.Operation]()
		if err != nil {
			return err
		}
		stat, err := ids.New[ids.Operation]()
		if err != nil {
			return err
		}
		m := lifecycle.ArtifactManifest{Artifact: artifact, Tenant: p.Tenant, Task: p.Task, Attempt: p.Attempt, Operation: commit, Producer: p.Provider, Kind: d.Kind, Digest: d.Digest, Size: d.Size, MediaType: d.MediaType, SourceRevision: p.Payload.SourceRevision, WriterEpoch: p.Epoch}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = u.Exec(ctx, `insert into artifact_work(tenant_id,operation_id,proposal_id,attempt_id,manifest,native_ref,authorize_id,export_id,stat_id,deadline) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.Tenant.String(), commit.String(), p.ID.String(), p.Attempt.String(), raw, d.NativeRef, auth.String(), export.String(), stat.String(), at.Add(24*time.Hour))
		if err != nil {
			return err
		}
		for _, s := range []struct {
			id     ids.OperationID
			method contracts.Method
		}{{auth, contracts.MethodAuthorizeUpload}, {export, contracts.MethodExportArtifacts}, {stat, contracts.MethodStatObject}, {commit, contracts.MethodCommitManifest}} {
			if _, err = u.Exec(ctx, `insert into artifact_stage_operation(tenant_id,operation_id,work_id,method) values($1,$2,$3,$4)`, p.Tenant.String(), s.id.String(), commit.String(), string(s.method)); err != nil {
				return err
			}
		}
		body, err := json.Marshal(map[string]string{"operation": commit.String()})
		if err != nil {
			return err
		}
		aid, err := ids.New[ids.Audit]()
		if err != nil {
			return err
		}
		c := lifecycle.Change{Tenant: p.Tenant, Record: lifecycle.RecordArtifact, RecordID: artifact.Untyped(), Event: "stage", From: "declared", To: "staged", Version: 1}
		if err = appendHistory(ctx, u, c, c.Audit(aid, lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: p.Provider}, at), &OutboxPayload{ID: commit.Untyped(), Attempt: p.Attempt, Body: body}); err != nil {
			return err
		}
	}
	return nil
}

func IsArtifactOperation(ctx context.Context, q Querier, tenant ids.TenantID, id ids.OperationID) (bool, error) {
	var yes bool
	err := q.QueryRow(ctx, `select exists(select 1 from artifact_stage_operation where tenant_id=$1 and operation_id=$2)`, tenant.String(), id.String()).Scan(&yes)
	return yes, err
}

func CurrentArtifactWriter(ctx context.Context, q Querier, w ArtifactWork) (bool, error) {
	var yes bool
	err := q.QueryRow(ctx, `select exists(select 1 from attempt a join result_proposal p using(tenant_id,attempt_id) where a.tenant_id=$1 and a.attempt_id=$2 and p.proposal_id=$3 and a.artifact_writer_epoch=$4 and p.writer_epoch=$4 and a.state='finalizing' and not exists(select 1 from attempt newer where newer.tenant_id=a.tenant_id and newer.task_id=a.task_id and newer.number>a.number))`, w.Manifest.Tenant.String(), w.Manifest.Attempt.String(), w.Proposal.String(), int64(w.Manifest.WriterEpoch)).Scan(&yes)
	return yes, err
}

// LatestRevision returns the immutable head; callers serialize creation and
// reviews with the Task lock rather than trusting a caller-supplied head.
func LatestRevision(ctx context.Context, q Querier, tenant ids.TenantID, task ids.TaskID) (lifecycle.ResultRevision, error) {
	var id string
	err := q.QueryRow(ctx, `select revision_id::text from result_revision where tenant_id=$1 and task_id=$2 order by number desc limit 1`, tenant.String(), task.String()).Scan(&id)
	if isNoRows(err) {
		return lifecycle.ResultRevision{}, ErrNotFound
	}
	if err != nil {
		return lifecycle.ResultRevision{}, err
	}
	rid, err := ids.Parse[ids.Revision](id)
	if err != nil {
		return lifecycle.ResultRevision{}, err
	}
	return GetRevision(ctx, q, tenant, rid)
}

func GetArtifactVerification(ctx context.Context, q Querier, tenant ids.TenantID, artifact ids.ArtifactID) (lifecycle.ArtifactVerification, error) {
	var v lifecycle.ArtifactVerification
	var raw []byte
	err := q.QueryRow(ctx, `select evidence from artifact_verification where tenant_id=$1 and artifact_id=$2`, tenant.String(), artifact.String()).Scan(&raw)
	if isNoRows(err) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, v.Validate()
}

// BindArtifactStore freezes routing before any effect. Reconfiguration cannot
// send the same stable identities to a different store after a crash.
func BindArtifactStore(ctx context.Context, u *UnitOfWork, w ArtifactWork, provider ids.ProviderID) error {
	return execVersionedUpdate(ctx, u, `update artifact_work set store_id=$3 where tenant_id=$1 and operation_id=$2 and (store_id is null or store_id=$3)`, w.Manifest.Tenant.String(), w.Manifest.Operation.String(), provider.String())
}

// RecordArtifactCommit preserves non-final outcomes too. Only an exact
// committed receipt may enter the admitted tables.
func RecordArtifactCommit(ctx context.Context, u *UnitOfWork, w ArtifactWork, r contracts.ArtifactCommitted, at time.Time) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Manifest != w.Manifest || w.Store.IsZero() {
		return ErrPayloadMismatch
	}
	current, err := CurrentArtifactWriter(ctx, u, w)
	if err != nil {
		return err
	}
	if !current {
		return ErrStaleVersion
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	eid, err := ids.New[ids.Any]()
	if err != nil {
		return err
	}
	if _, err = u.Exec(ctx, `insert into artifact_commit_evidence(tenant_id,evidence_id,operation_id,receipt,observed_at) values($1,$2,$3,$4,$5)`, w.Manifest.Tenant.String(), eid.String(), w.Manifest.Operation.String(), raw, at); err != nil {
		return err
	}
	if r.Outcome != lifecycle.CommitCommitted {
		return nil
	}
	if _, _, err = InsertManifest(ctx, u, w.Manifest); err != nil {
		return err
	}
	artifact := lifecycle.Artifact{ArtifactManifest: w.Manifest, CommittedAt: r.CommittedAt}
	_, replay, err := InsertArtifact(ctx, u, artifact)
	if err != nil {
		return err
	}
	if replay {
		v, err := GetArtifactVerification(ctx, u, w.Manifest.Tenant, w.Manifest.Artifact)
		if err != nil {
			return err
		}
		if v.Manifest != w.Manifest || v.Store != w.Store || v.ObjectVersion != r.ObjectVersion || !sameInstant(v.VerifiedAt, r.CommittedAt) {
			return ErrPayloadMismatch
		}
		return nil
	}
	vid, err := ids.New[ids.Verification]()
	if err != nil {
		return err
	}
	v := lifecycle.ArtifactVerification{ID: vid, Manifest: w.Manifest, Store: w.Store, Digest: r.Observed.Digest, Size: r.Observed.Size, ObjectVersion: r.ObjectVersion, VerifiedAt: r.CommittedAt}
	if err = v.Validate(); err != nil {
		return err
	}
	raw, err = json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = u.Exec(ctx, `insert into artifact_verification(tenant_id,verification_id,artifact_id,evidence) values($1,$2,$3,$4)`, w.Manifest.Tenant.String(), vid.String(), w.Manifest.Artifact.String(), raw); err != nil {
		return err
	}
	aid, err := ids.New[ids.Audit]()
	if err != nil {
		return err
	}
	c := lifecycle.Change{Tenant: w.Manifest.Tenant, Record: lifecycle.RecordArtifact, RecordID: w.Manifest.Artifact.Untyped(), Event: "commit", From: "staged", To: "committed", Version: 2}
	return appendHistory(ctx, u, c, c.Audit(aid, lifecycle.Actor{Kind: lifecycle.ActorSystem}, at), nil)
}

// CreateVerifiedRevision refuses partial admission. The caller holds scope
// locks and completes lifecycle transitions in this same unit of work.
func CreateVerifiedRevision(ctx context.Context, u *UnitOfWork, p ResultProposal, at time.Time) (lifecycle.ResultRevision, bool, error) {
	rows, err := u.Query(ctx, `select operation_id::text from artifact_work where tenant_id=$1 and proposal_id=$2 order by native_ref`, p.Tenant.String(), p.ID.String())
	if err != nil {
		return lifecycle.ResultRevision{}, false, err
	}
	var operations []ids.OperationID
	for rows.Next() {
		var text string
		if err = rows.Scan(&text); err != nil {
			rows.Close()
			return lifecycle.ResultRevision{}, false, err
		}
		id, e := ids.Parse[ids.Operation](text)
		if e != nil {
			rows.Close()
			return lifecycle.ResultRevision{}, false, e
		}
		operations = append(operations, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return lifecycle.ResultRevision{}, false, err
	}
	if len(operations) != len(p.Payload.Artifacts) {
		return lifecycle.ResultRevision{}, false, nil
	}
	var artifacts []lifecycle.Artifact
	var verification []lifecycle.ArtifactVerification
	for _, op := range operations {
		w, e := GetArtifactWork(ctx, u, p.Tenant, op)
		if e != nil {
			return lifecycle.ResultRevision{}, false, e
		}
		a, e := GetArtifact(ctx, u, p.Tenant, w.Manifest.Artifact)
		if errors.Is(e, ErrNotFound) {
			return lifecycle.ResultRevision{}, false, nil
		}
		if e != nil {
			return lifecycle.ResultRevision{}, false, e
		}
		v, e := GetArtifactVerification(ctx, u, p.Tenant, a.Artifact)
		if e != nil {
			return lifecycle.ResultRevision{}, false, e
		}
		if a.ArtifactManifest != w.Manifest || v.Manifest != w.Manifest || v.Store != w.Store || a.Attempt != p.Attempt || a.Task != p.Task || a.SourceRevision != p.Payload.SourceRevision || a.WriterEpoch != p.Epoch {
			return lifecycle.ResultRevision{}, false, ErrPayloadMismatch
		}
		artifacts = append(artifacts, a)
		verification = append(verification, v)
	}
	var previous *lifecycle.ResultRevision
	head, err := LatestRevision(ctx, u, p.Tenant, p.Task)
	if err == nil {
		if head.Attempt == p.Attempt {
			return head, true, nil
		}
		previous = &head
	} else if !errors.Is(err, ErrNotFound) {
		return head, false, err
	}
	id, err := ids.New[ids.Revision]()
	if err != nil {
		return lifecycle.ResultRevision{}, false, err
	}
	revision, err := lifecycle.NewResultRevision(id, p.Attempt, artifacts, previous, at)
	if err != nil {
		return revision, false, err
	}
	for _, v := range verification {
		revision.Verifications = append(revision.Verifications, v.ID)
	}
	if _, _, err = InsertRevision(ctx, u, revision); err != nil {
		return revision, false, err
	}
	return revision, true, nil
}
