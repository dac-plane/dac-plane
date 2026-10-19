package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

// ArtifactStorePort explicitly binds a producer's store audience. Store bytes
// and credentials never become product records.
type ArtifactStorePort struct {
	Provider ids.ProviderID
	Port     contracts.Port
}

func WithArtifactStores(ports map[ids.ProviderID]ArtifactStorePort) func(*Worker) {
	return func(w *Worker) {
		w.artifacts = make(map[ids.ProviderID]ArtifactStorePort, len(ports))
		for id, p := range ports {
			w.artifacts[id] = p
		}
	}
}

type artifactClaim struct {
	work  store.ArtifactWork
	claim store.OutboxClaim
	lease store.AttemptLease
}

func (w *Worker) artifactRow(ctx context.Context, tenant ids.TenantID, row store.OutboxRow) error {
	id, err := ids.Parse[ids.Operation](row.ID.String())
	if err != nil {
		return err
	}
	work, err := store.GetArtifactWork(ctx, w.pool, tenant, id)
	if err != nil {
		return err
	}
	if work.Manifest.Artifact.Untyped() != row.RecordID || row.Attempt != work.Manifest.Attempt {
		return w.poison(ctx, tenant, row, time.Now().UTC(), reasonOperationMismatch)
	}
	binding, ok := w.artifacts[work.Manifest.Producer]
	runtime, hasRuntime := w.ports[work.Manifest.Producer]
	if !ok || !hasRuntime || binding.Port == nil || binding.Provider.IsZero() {
		w.stats.SkippedNoPort.Add(1)
		return nil
	}
	if binding.Port.Role() != contracts.RoleArtifactStore || runtime.Role() != contracts.RoleRuntime {
		return errors.New("invalid artifact port role")
	}
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(context.WithoutCancel(ctx)) }()
	now := time.Now().UTC()
	claim, err := store.ClaimOutboxRowByID(ctx, u, tenant, row.ID, w.owner, w.cfg.ClaimTTL, now)
	if errors.Is(err, store.ErrStaleVersion) {
		return nil
	}
	if err != nil {
		return err
	}
	_, task, err := store.LockResultScope(ctx, u, tenant, work.Manifest.Attempt)
	if err != nil {
		return err
	}
	current, err := store.CurrentArtifactWriter(ctx, u, work)
	if err != nil {
		return err
	}
	if !current || task.State != lifecycle.TaskVerifying {
		if err = store.AckOutboxRow(ctx, u, tenant, row.ID, claim.Epoch, time.Now().UTC()); err != nil {
			return err
		}
		return u.Commit(ctx)
	}
	harness, err := store.GetHarnessForUpdate(ctx, u, tenant, work.Manifest.Producer)
	if err != nil {
		return err
	}
	if harness.Mode != lifecycle.HarnessLive {
		return nil
	}
	now = time.Now().UTC()
	lease, err := store.ClaimAttemptLease(ctx, u, tenant, work.Manifest.Attempt, w.owner, w.cfg.ClaimTTL, now)
	if errors.Is(err, store.ErrStaleVersion) {
		if err = store.ReleaseOutboxRow(ctx, u, tenant, row.ID, claim.Epoch, now); err != nil {
			return err
		}
		return u.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if err = store.BindArtifactStore(ctx, u, work, binding.Provider); err != nil {
		return err
	}
	work.Store = binding.Provider
	p := artifactClaim{work: work, claim: claim, lease: lease}
	for _, s := range []struct {
		id       ids.OperationID
		provider ids.ProviderID
	}{{work.Authorize, work.Store}, {work.Export, work.Manifest.Producer}, {work.Stat, work.Store}, {work.Manifest.Operation, work.Store}} {
		op, e := lifecycle.NewProviderOperation(s.id, tenant, work.Manifest.Attempt, s.provider, now)
		if e != nil {
			return e
		}
		existing, e := store.GetOperation(ctx, u, tenant, s.id)
		if errors.Is(e, store.ErrNotFound) {
			if _, _, e = store.InsertOperation(ctx, u, op); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if existing.Attempt != op.Attempt || existing.Provider != op.Provider {
			return store.ErrPayloadMismatch
		}
	}
	if err = u.Commit(ctx); err != nil {
		return err
	}
	if claim.Row.Attempts >= w.cfg.Policy.MaxAttempts || !time.Now().Before(work.Deadline) {
		return w.finishArtifact(ctx, p, nil, contracts.FailureDeadlineExceeded)
	}
	transfer := contracts.ArtifactTransfer{Manifest: work.Manifest, NativeRef: work.NativeRef}
	raw, err := w.artifactCall(ctx, p, binding.Port, work.Authorize, contracts.MethodAuthorizeUpload, transfer)
	if err != nil {
		return w.finishArtifact(ctx, p, nil, artifactFailure(err))
	}
	var authorized contracts.UploadAuthorized
	if decodeArtifact(raw, &authorized) != nil || authorized.Validate() != nil {
		return w.finishArtifact(ctx, p, nil, contracts.FailureMalformedFrame)
	}
	transfer.Upload = authorized.Upload
	if _, err = w.artifactCall(ctx, p, runtime, work.Export, contracts.MethodExportArtifacts, transfer); err != nil {
		return w.finishArtifact(ctx, p, nil, artifactFailure(err))
	}
	transfer.Upload = ""
	raw, err = w.artifactCall(ctx, p, binding.Port, work.Stat, contracts.MethodStatObject, transfer)
	if err != nil {
		return w.finishArtifact(ctx, p, nil, artifactFailure(err))
	}
	var observed contracts.ArtifactStored
	if decodeArtifact(raw, &observed) != nil || observed.Validate() != nil {
		return w.finishArtifact(ctx, p, nil, contracts.FailureMalformedFrame)
	}
	// Stat is diagnostic. Admission always depends on the atomically verified
	// immutable commit receipt, even when this observation matches.
	raw, err = w.artifactCall(ctx, p, binding.Port, work.Manifest.Operation, contracts.MethodCommitManifest, transfer)
	if err != nil {
		return w.finishArtifact(ctx, p, nil, artifactFailure(err))
	}
	var receipt contracts.ArtifactCommitted
	if decodeArtifact(raw, &receipt) != nil || receipt.Validate() != nil || receipt.Manifest != work.Manifest {
		return w.finishArtifact(ctx, p, nil, contracts.FailureMalformedFrame)
	}
	return w.finishArtifact(ctx, p, &receipt, "")
}

func decodeArtifact(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return contracts.ErrInvalid
	}
	return nil
}
func artifactFailure(err error) contracts.FailureCode {
	var f *contracts.Failure
	if errors.As(err, &f) {
		return f.Code
	}
	if errors.Is(err, store.ErrStaleVersion) {
		return contracts.FailureResourceBusy
	}
	return contracts.FailureTransportUnavailable
}

// artifactScope fences every send and result transaction, sampling time only
// after locks. The Attempt writer generation is never the dispatch lease.
func (w *Worker) artifactScope(ctx context.Context, u *store.UnitOfWork, p artifactClaim) error {
	var held bool
	err := u.QueryRow(ctx, `select true from outbox where tenant_id=$1 and outbox_id=$2 and status='processing' and lease_epoch=$3 and lease_expires_at>clock_timestamp() for update`, p.work.Manifest.Tenant.String(), p.claim.Row.ID.String(), p.claim.Epoch).Scan(&held)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrStaleVersion
		}
		return err
	}
	_, task, err := store.LockResultScope(ctx, u, p.work.Manifest.Tenant, p.work.Manifest.Attempt)
	if err != nil {
		return err
	}
	current, err := store.CurrentArtifactWriter(ctx, u, p.work)
	if err != nil {
		return err
	}
	if !current || task.State != lifecycle.TaskVerifying {
		return store.ErrStaleVersion
	}
	h, err := store.GetHarnessForUpdate(ctx, u, p.work.Manifest.Tenant, p.work.Manifest.Producer)
	if err != nil {
		return err
	}
	if h.Mode != lifecycle.HarnessLive {
		return store.ErrStaleVersion
	}
	var valid bool
	err = u.QueryRow(ctx, `select true from attempt where tenant_id=$1 and attempt_id=$2 and lease_owner=$3 and lease_epoch=$4 and lease_expires_at>clock_timestamp()`, p.work.Manifest.Tenant.String(), p.work.Manifest.Attempt.String(), p.lease.Owner, p.lease.Epoch).Scan(&valid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrStaleVersion
		}
		return err
	}
	// Sample after all lock waits; expiry during a wait cannot be revived.
	now := time.Now().UTC()
	if err = store.RenewArtifactClaim(ctx, u, p.work.Manifest.Tenant, p.claim.Row.ID, p.claim.Epoch, w.cfg.ClaimTTL, now); err != nil {
		return err
	}
	if err = store.RenewAttemptLease(ctx, u, p.work.Manifest.Tenant, p.work.Manifest.Attempt, p.lease, w.cfg.ClaimTTL, now); err != nil {
		return err
	}
	return nil
}

// Every method in this artifact protocol is explicitly replay-safe. A send
// marker is persisted first; a lost reply replays that same stage identity,
// never a fresh commit and never generic runtime reconciliation.
func (w *Worker) artifactCall(ctx context.Context, p artifactClaim, port contracts.Port, id ids.OperationID, method contracts.Method, value any) (json.RawMessage, error) {
	if !time.Now().Before(p.work.Deadline) {
		return nil, &contracts.Failure{Code: contracts.FailureDeadlineExceeded}
	}
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = u.Rollback(context.WithoutCancel(ctx)) }()
	if err = w.artifactScope(ctx, u, p); err != nil {
		return nil, err
	}
	op, err := store.GetOperation(ctx, u, p.work.Manifest.Tenant, id)
	if err != nil {
		return nil, err
	}
	if op.State == lifecycle.OperationRecorded {
		tr, e := w.transition(op.Version, time.Now().UTC())
		if e != nil {
			return nil, e
		}
		op, _, err = store.ApplyOperation(ctx, u, op.Tenant, op.ID, lifecycle.OperationDispatch, "", tr)
		if err != nil {
			return nil, err
		}
	}
	if op.State == lifecycle.OperationDispatched {
		tr, e := w.transition(op.Version, time.Now().UTC())
		if e != nil {
			return nil, e
		}
		if _, _, err = store.ApplyOperation(ctx, u, op.Tenant, op.ID, lifecycle.OperationSendUnknown, "", tr); err != nil {
			return nil, err
		}
	}
	if err = w.artifactScope(ctx, u, p); err != nil {
		return nil, err
	}
	if !time.Now().Before(p.work.Deadline) {
		return nil, &contracts.Failure{Code: contracts.FailureDeadlineExceeded}
	}
	if err = u.Commit(ctx); err != nil {
		return nil, err
	}
	args, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return nil, err
	}
	request := contracts.Request{Tenant: op.Tenant, Attempt: op.Attempt, Operation: id, Delivery: delivery, Role: port.Role(), Method: method, Arguments: args, Demand: contracts.DemandBackground, Deadline: p.work.Deadline}
	if err = request.Validate(); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, min(w.cfg.SendTimeout, w.cfg.ClaimTTL/2, time.Until(p.work.Deadline)))
	defer cancel()
	reply, err := port.Send(callCtx, request)
	if err != nil {
		return nil, err
	}
	if reply.Validate() != nil || reply.Operation != id || reply.Delivery != delivery {
		return nil, &contracts.Failure{Code: contracts.FailureMalformedFrame}
	}
	if reply.Failure != nil {
		return nil, reply.Failure
	}
	if reply.Durability != lifecycle.SendAccepted {
		return nil, &contracts.Failure{Code: contracts.FailureTransportUnavailable}
	}
	if method != contracts.MethodCommitManifest {
		settle, e := store.Begin(ctx, w.pool)
		if e != nil {
			return nil, e
		}
		defer func() { _ = settle.Rollback(context.WithoutCancel(ctx)) }()
		if e = w.artifactScope(ctx, settle, p); e != nil {
			return nil, e
		}
		stage, e := store.GetOperation(ctx, settle, op.Tenant, id)
		if e != nil {
			return nil, e
		}
		if stage.State != lifecycle.OperationSucceeded {
			tr, e := w.transition(stage.Version, time.Now().UTC())
			if e != nil {
				return nil, e
			}
			if _, _, e = store.ApplyOperation(ctx, settle, op.Tenant, id, lifecycle.OperationReconcile, lifecycle.OperationSucceeded, tr); e != nil {
				return nil, e
			}
		}
		if e = settle.Commit(ctx); e != nil {
			return nil, e
		}
	}
	return reply.Result, nil
}

func (w *Worker) finishArtifact(ctx context.Context, p artifactClaim, receipt *contracts.ArtifactCommitted, code contracts.FailureCode) error {
	u, err := store.Begin(ctx, w.pool)
	if err != nil {
		return err
	}
	defer func() { _ = u.Rollback(context.WithoutCancel(ctx)) }()
	if err = w.artifactScope(ctx, u, p); err != nil {
		if !errors.Is(err, store.ErrStaleVersion) {
			return err
		}
		// Cancellation, expiry or takeover may have won while the store was
		// working. Leave its outcome unadmitted; the next owner rechecks scope.
		return nil
	}
	now := time.Now().UTC()
	tenant := p.work.Manifest.Tenant
	step := progress.Step{UOW: u, Tenant: tenant, Actor: SystemActor(), At: now}
	if receipt != nil {
		if err = store.RecordArtifactCommit(ctx, u, p.work, *receipt, now); err != nil {
			return err
		}
		if receipt.Outcome == lifecycle.CommitCommitted {
			op, e := store.GetOperation(ctx, u, tenant, p.work.Manifest.Operation)
			if e != nil {
				return e
			}
			if op.State != lifecycle.OperationSucceeded {
				tr, e := w.transition(op.Version, now)
				if e != nil {
					return e
				}
				if _, _, e = store.ApplyOperation(ctx, u, tenant, op.ID, lifecycle.OperationReconcile, lifecycle.OperationSucceeded, tr); e != nil {
					return e
				}
			}
			proposal, e := store.GetResultProposal(ctx, u, tenant, p.work.Manifest.Attempt)
			if e != nil {
				return e
			}
			if _, ready, e := store.CreateVerifiedRevision(ctx, u, proposal, now); e != nil {
				return e
			} else if ready {
				if _, _, _, e = step.Attempt(ctx, p.work.Manifest.Attempt, lifecycle.AttemptComplete, ""); e != nil {
					return e
				}
				if _, e = step.Task(ctx, p.work.Manifest.Task, lifecycle.TaskVerificationPassed); e != nil {
					return e
				}
			}
			if err = store.AckOutboxRow(ctx, u, tenant, p.claim.Row.ID, p.claim.Epoch, time.Now().UTC()); err != nil {
				return err
			}
			if err = store.ReleaseAttemptLease(ctx, u, tenant, p.work.Manifest.Attempt, p.lease, now); err != nil {
				return err
			}
			return u.Commit(ctx)
		}
		code = contracts.FailureProviderFailed
		if receipt.Outcome == lifecycle.CommitFenced {
			code = contracts.FailureStalePrecondition
		}
	}
	delay, exhaust := w.cfg.Policy.Classify(p.claim.Row.Attempts+1, code)
	if exhaust {
		if _, _, _, err = step.Attempt(ctx, p.work.Manifest.Attempt, lifecycle.AttemptFail, ""); err != nil {
			return err
		}
		err = store.ExhaustOutboxRow(ctx, u, tenant, p.claim.Row.ID, p.claim.Epoch, string(code), now)
	} else {
		err = store.ScheduleOutboxRetry(ctx, u, tenant, p.claim.Row.ID, p.claim.Epoch, now.Add(delay), string(code), now)
	}
	if err != nil {
		return err
	}
	if err = store.ReleaseAttemptLease(ctx, u, tenant, p.work.Manifest.Attempt, p.lease, now); err != nil {
		return err
	}
	return u.Commit(ctx)
}
