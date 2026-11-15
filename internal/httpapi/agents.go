package httpapi

import (
	"net/http"
	"strconv"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// Agents are readable by every principal of the tenant, so these handlers
// need no visibility check before a command: the command itself refuses a
// principal that did not create the agent.

func (a *API) createAgent(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	body, err := decodeLimited[AgentRequest](r, maxAgentBodyBytes)
	if err != nil {
		return err
	}
	agent, err := application.CreateAgent(r.Context(), a.pool, application.CreateAgentCommand{
		Command: cmd, At: a.options.Now(), Profile: body.profile(),
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tenants/"+actor.Tenant().String()+"/agents/"+agent.ID.String())
	writeAgent(w, http.StatusCreated, agent)
	return nil
}

func (a *API) listAgents(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	agents, err := application.ListAgents(r.Context(), a.pool, actor)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, AgentListBody{Agents: nonNil(agents)})
	return nil
}

func (a *API) getAgent(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Agent](r, "agent")
	if err != nil {
		return err
	}
	agent, err := application.GetAgent(r.Context(), a.pool, actor, id)
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, agent)
	return nil
}

// reviseAgent replaces the agent's profile. If-Match carries the agent
// version.
func (a *API) reviseAgent(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Agent](r, "agent")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	body, err := decodeLimited[AgentRequest](r, maxAgentBodyBytes)
	if err != nil {
		return err
	}
	agent, err := application.ReviseAgent(r.Context(), a.pool, application.ReviseAgentCommand{
		Command: cmd, Agent: id, At: a.options.Now(), Profile: body.profile(),
	})
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, agent)
	return nil
}

// retireAgent stops new delegation to the agent. If-Match carries the agent
// version.
func (a *API) retireAgent(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Agent](r, "agent")
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
	agent, err := application.RetireAgent(r.Context(), a.pool, application.RetireAgentCommand{
		Command: cmd, Agent: id, At: a.options.Now(),
	})
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, agent)
	return nil
}

func (a *API) getAgentRevision(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	id, err := pathID[ids.Agent](r, "agent")
	if err != nil {
		return err
	}
	// A revision number that cannot exist names nothing, so it is not found.
	number, err := strconv.ParseUint(r.PathValue("revision"), 10, 32)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != r.PathValue("revision") {
		return application.ErrNotFound
	}
	revision, err := application.GetAgentRevision(r.Context(), a.pool, actor, id, uint32(number))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, revision)
	return nil
}

func writeAgent(w http.ResponseWriter, status int, agent lifecycle.Agent) {
	w.Header().Set("ETag", etag(agent.Version))
	writeJSON(w, status, agent)
}
