package httpapi

import (
	"net/http"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// Connections (ADR-0022) are readable by every principal of the tenant; the
// commands refuse a principal that does not own the connection. Requests that
// reach the connector are answered by the replica its adapter is connected
// to; another replica answers connector-unavailable and the surface retries.

const maxConnectionBodyBytes = 16 << 10

func (a *API) createConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	body, err := decodeLimited[ConnectionRequest](r, maxConnectionBodyBytes)
	if err != nil {
		return err
	}
	view, err := application.CreateConnection(r.Context(), a.pool, a.options.Connectors, application.CreateConnectionCommand{
		Command: cmd, At: a.options.Now(), Provider: body.Provider, Name: body.Name,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tenants/"+actor.Tenant().String()+"/connections/"+view.Connection.ID.String())
	w.Header().Set("ETag", etag(view.Connection.Version))
	writeJSON(w, http.StatusCreated, ConnectionCreatedBody{Connection: view.Connection, Authorization: view.Authorization})
	return nil
}

func (a *API) listConnections(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	connections, err := application.ListConnections(r.Context(), a.pool, actor)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ConnectionListBody{Connections: nonNil(connections)})
	return nil
}

func (a *API) getConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
	if err != nil {
		return err
	}
	connection, err := application.GetConnection(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	writeConnection(w, http.StatusOK, connection)
	return nil
}

// authorizeConnection returns a consent URL, replayed under its idempotency key.
func (a *API) authorizeConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	id, err := pathID[ids.Connection](r, "connection")
	if err != nil {
		return err
	}
	if _, err := decode[EmptyCommandRequest](r); err != nil {
		return err
	}
	started, err := application.AuthorizeConnection(r.Context(), a.pool, a.options.Connectors, application.AuthorizeConnectionCommand{Command: cmd, Connection: id, At: a.options.Now()})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, started)
	return nil
}

// completeConnection records a finished consent. If-Match carries the
// connection version.
func (a *API) completeConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
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
	connection, err := application.CompleteConnection(r.Context(), a.pool, a.options.Connectors, application.CompleteConnectionCommand{
		Command: cmd, At: a.options.Now(), Connection: id,
	})
	if err != nil {
		return err
	}
	writeConnection(w, http.StatusOK, connection)
	return nil
}

// removeConnection revokes the connection's grants and its credential.
// If-Match carries the connection version.
func (a *API) removeConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
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
	connection, err := application.RemoveConnection(r.Context(), a.pool, a.options.Connectors, application.RemoveConnectionCommand{
		Command: cmd, At: a.options.Now(), Connection: id,
	})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if connection.State == lifecycle.ConnectionRemoving {
		// The credential is not yet revoked at the connector.
		status = http.StatusAccepted
	}
	writeConnection(w, status, connection)
	return nil
}

func (a *API) listConnectionTools(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
	if err != nil {
		return err
	}
	catalogue, err := application.ListConnectionTools(r.Context(), a.pool, a.options.Connectors, actor, id, a.options.Now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, catalogue)
	return nil
}

func (a *API) grantConnection(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	body, err := decodeLimited[ConnectionGrantRequest](r, maxConnectionBodyBytes)
	if err != nil {
		return err
	}
	grant, err := application.GrantConnection(r.Context(), a.pool, a.options.Connectors, application.GrantConnectionCommand{
		Command: cmd, At: a.options.Now(), Connection: id, Terms: body.terms(),
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tenants/"+actor.Tenant().String()+"/connection-grants/"+grant.ID.String())
	writeGrant(w, http.StatusCreated, grant)
	return nil
}

func (a *API) listConnectionGrants(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Connection](r, "connection")
	if err != nil {
		return err
	}
	grants, err := application.ListConnectionGrants(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ConnectionGrantListBody{Grants: nonNil(grants)})
	return nil
}

func (a *API) getConnectionGrant(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.ConnGrant](r, "grant")
	if err != nil {
		return err
	}
	grant, err := application.GetConnectionGrant(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	writeGrant(w, http.StatusOK, grant)
	return nil
}

// revokeConnectionGrant stops the grant authorizing calls. If-Match carries
// the grant version.
func (a *API) revokeConnectionGrant(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.ConnGrant](r, "grant")
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
	grant, err := application.RevokeConnectionGrant(r.Context(), a.pool, application.RevokeConnectionGrantCommand{
		Command: cmd, At: a.options.Now(), Grant: id,
	})
	if err != nil {
		return err
	}
	writeGrant(w, http.StatusOK, grant)
	return nil
}

func writeConnection(w http.ResponseWriter, status int, connection lifecycle.Connection) {
	w.Header().Set("ETag", etag(connection.Version))
	writeJSON(w, status, connection)
}

func writeGrant(w http.ResponseWriter, status int, grant lifecycle.ConnectionGrant) {
	w.Header().Set("ETag", etag(grant.Version))
	writeJSON(w, status, grant)
}
