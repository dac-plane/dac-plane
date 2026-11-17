package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrArtifactUnavailable reports that an artifact's bytes cannot be read
// now: its store is not connected to this replica, did not negotiate
// downloads, or refused. The record itself stays readable (ADR-0025).
var ErrArtifactUnavailable = errors.New("artifact unavailable")

// artifactRequestTimeout bounds one authorize_download round trip.
const artifactRequestTimeout = 15 * time.Second

// GetArtifact returns an admitted artifact of a task the actor may read. An
// artifact of another task is not found, as a task the actor may not read is.
func GetArtifact(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, task ids.TaskID, id ids.ArtifactID) (lifecycle.Artifact, error) {
	var artifact lifecycle.Artifact
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		artifact, _, err = r.artifact(ctx, task, id)
		return err
	})
	return artifact, err
}

func (r *reader) artifact(ctx context.Context, task ids.TaskID, id ids.ArtifactID) (lifecycle.Artifact, lifecycle.ArtifactVerification, error) {
	if _, err := r.task(ctx, task); err != nil {
		return lifecycle.Artifact{}, lifecycle.ArtifactVerification{}, err
	}
	artifact, err := store.GetArtifact(ctx, r.uow, r.principal.Tenant, id)
	if err != nil {
		return lifecycle.Artifact{}, lifecycle.ArtifactVerification{}, err
	}
	if artifact.Task != task {
		return lifecycle.Artifact{}, lifecycle.ArtifactVerification{}, ErrNotFound
	}
	verification, err := store.GetArtifactVerification(ctx, r.uow, r.principal.Tenant, id)
	if err != nil {
		return lifecycle.Artifact{}, lifecycle.ArtifactVerification{}, err
	}
	return artifact, verification, nil
}

// ArtifactDownload is a read of one artifact's committed bytes: the admitted
// record the bytes must match and the store's short-lived URL. The URL is a
// bearer capability; the caller uses it once and never logs or returns it.
type ArtifactDownload struct {
	Artifact  lifecycle.Artifact
	URL       string
	ExpiresAt time.Time
}

// AuthorizeArtifactDownload asks the artifact's store for a read of the
// version its verification recorded. The request names the artifact's
// attempt, is read-only and is sent directly, never as a provider operation:
// repeating it is harmless.
func AuthorizeArtifactDownload(ctx context.Context, pool *pgxpool.Pool, ports ConnectorPorts, actor identity.AuthorizationContext, task ids.TaskID, id ids.ArtifactID, at time.Time) (ArtifactDownload, error) {
	var artifact lifecycle.Artifact
	var verification lifecycle.ArtifactVerification
	err := read(ctx, pool, actor, func(r *reader) error {
		var err error
		artifact, verification, err = r.artifact(ctx, task, id)
		return err
	})
	if err != nil {
		return ArtifactDownload{}, err
	}
	if ports == nil {
		return ArtifactDownload{}, ErrArtifactUnavailable
	}
	port := ports.Port(artifact.Tenant, verification.Store, contracts.RoleArtifactStore)
	if scoped, ok := port.(contracts.CapabilityScoped); ok {
		port = scoped.Requiring(contracts.CapabilityArtifactDownload)
	} else {
		port = nil
	}
	if port == nil {
		return ArtifactDownload{}, fmt.Errorf("%w: the artifact's store offers no downloads on this replica", ErrArtifactUnavailable)
	}

	arguments, err := json.Marshal(contracts.DownloadRequest{Manifest: artifact.ArtifactManifest, ObjectVersion: verification.ObjectVersion})
	if err != nil {
		return ArtifactDownload{}, err
	}
	operation, err := ids.New[ids.Operation]()
	if err != nil {
		return ArtifactDownload{}, err
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return ArtifactDownload{}, err
	}
	at = at.UTC()
	request := contracts.Request{
		Operation: operation, Delivery: delivery, Tenant: artifact.Tenant, Attempt: artifact.Attempt,
		Role: contracts.RoleArtifactStore, Method: contracts.MethodAuthorizeDownload, Arguments: arguments,
		Demand: contracts.DemandPerson, Deadline: at.Add(artifactRequestTimeout),
	}
	if err := request.Validate(); err != nil {
		return ArtifactDownload{}, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, artifactRequestTimeout)
	defer cancel()
	reply, err := port.Send(sendCtx, request)
	if err != nil {
		return ArtifactDownload{}, fmt.Errorf("%w: %v", ErrArtifactUnavailable, err)
	}
	if reply.Validate() != nil || reply.Operation != operation || reply.Delivery != delivery || reply.Outcome != contracts.OutcomeOK || reply.Durability != lifecycle.SendAccepted {
		if reply.Failure != nil {
			return ArtifactDownload{}, fmt.Errorf("%w: the store refused: %s", ErrArtifactUnavailable, reply.Failure.Code)
		}
		return ArtifactDownload{}, ErrArtifactUnavailable
	}
	var authorized contracts.DownloadAuthorized
	if err := json.Unmarshal(reply.Result, &authorized); err != nil || authorized.Validate() != nil {
		return ArtifactDownload{}, fmt.Errorf("%w: malformed download from the store", ErrArtifactUnavailable)
	}
	if !authorized.ExpiresAt.After(at) || authorized.ExpiresAt.After(at.Add(contracts.MaxDownloadLifetime+time.Minute)) {
		return ArtifactDownload{}, fmt.Errorf("%w: the store's download expiry is outside its bounds", ErrArtifactUnavailable)
	}
	return ArtifactDownload{Artifact: artifact, URL: authorized.Download, ExpiresAt: authorized.ExpiresAt}, nil
}
