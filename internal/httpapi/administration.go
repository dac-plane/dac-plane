package httpapi

import (
	"errors"
	"net/http"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

func (a *API) createMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	body, err := decode[CreateMigrationRequest](r)
	if err != nil {
		return err
	}
	view, err := application.CreateMigration(r.Context(), a.pool, application.CreateMigrationPass{
		Command: cmd,
		From:    body.From,
		To:      body.To,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tenants/"+actor.Tenant().String()+"/migration-passes/"+view.Pass.ID.String())
	w.Header().Set("ETag", etag(view.Pass.Version))
	writeJSON(w, http.StatusCreated, migrationBody(view))
	return nil
}

func (a *API) getMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	migration, err := pathID[ids.Migration](r, "migration")
	if err != nil {
		return err
	}
	view, err := application.GetMigration(r.Context(), a.pool, actor, migration)
	if err != nil {
		return concealAdministrativeResource(err)
	}
	w.Header().Set("ETag", etag(view.Pass.Version))
	writeJSON(w, http.StatusOK, migrationBody(view))
	return nil
}

func (a *API) previewMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.administerMigration(w, r, actor, lifecycle.MigrationPreview, http.StatusOK)
}

func (a *API) holdMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.administerMigration(w, r, actor, lifecycle.MigrationHold, http.StatusOK)
}

func (a *API) releaseMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.administerMigration(w, r, actor, lifecycle.MigrationRelease, http.StatusOK)
}

func (a *API) executeMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.administerMigration(w, r, actor, lifecycle.MigrationExecute, http.StatusAccepted)
}

func (a *API) cancelMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.administerMigration(w, r, actor, lifecycle.MigrationCancel, http.StatusOK)
}

func (a *API) administerMigration(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext, event lifecycle.MigrationEvent, status int) error {
	migration, err := pathID[ids.Migration](r, "migration")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	if _, err := decode[EmptyCommandRequest](r); err != nil {
		return err
	}
	view, err := application.AdministerMigration(r.Context(), a.pool, application.MigrationAdministration{
		Command:   cmd,
		Migration: migration,
		Event:     event,
	})
	if err != nil {
		return concealAdministrativeResource(err)
	}
	w.Header().Set("ETag", etag(view.Pass.Version))
	writeJSON(w, status, migrationBody(view))
	return nil
}

func (a *API) revokeAuthority(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	grant, err := pathID[ids.Grant](r, "grant")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	if _, err := decode[EmptyCommandRequest](r); err != nil {
		return err
	}
	if err := application.Revoke(r.Context(), a.pool, application.RevokeAuthority{
		Command: cmd,
		Grant:   grant,
		At:      a.options.Now(),
	}); err != nil {
		return concealAdministrativeResource(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func concealAdministrativeResource(err error) error {
	if errors.Is(err, application.ErrUnauthorized) {
		return application.ErrNotFound
	}
	return err
}
