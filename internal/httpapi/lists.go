package httpapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// TaskListBody is one page of tasks, newest first by creation (ADR-0028).
// NextCursor is sent back as cursor for the following page and is absent on the last.
type TaskListBody struct {
	Tasks      []lifecycle.Task `json:"tasks"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	query := r.URL.Query()
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", application.ErrInvalidCommand, fmt.Sprintf(format, args...))
	}
	var request application.TaskQuery
	if text := query.Get("limit"); text != "" {
		limit, err := strconv.Atoi(text)
		if err != nil || limit < 1 || limit > application.MaxTaskPage {
			return invalid("limit must be 1 to %d", application.MaxTaskPage)
		}
		request.Limit = limit
	}
	if text := query.Get("state"); text != "" {
		request.State = lifecycle.TaskState(text)
	}
	if text := query.Get("agent"); text != "" {
		agent, err := ids.Parse[ids.Agent](text)
		if err != nil {
			return invalid("agent is not an identifier")
		}
		request.Agent = agent
	}
	if text := query.Get("cursor"); text != "" {
		cursor, err := parseTaskCursor(text)
		if err != nil {
			return invalid("malformed cursor")
		}
		request.After = &cursor
	}
	page, err := application.ListTasks(r.Context(), a.pool, actor, request)
	if err != nil {
		return err
	}
	body := TaskListBody{Tasks: page.Tasks}
	if page.Next != nil {
		body.NextCursor = formatTaskCursor(*page.Next)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
	return nil
}

// A task cursor is the creation time in microseconds and the task, which is the
// position a page starts after. It is opaque to a client and names no more than
// a task the client has already been shown.
func formatTaskCursor(cursor application.TaskCursor) string {
	text := strconv.FormatInt(cursor.CreatedAt.UnixMicro(), 10) + "." + cursor.Task.String()
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

func parseTaskCursor(text string) (application.TaskCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return application.TaskCursor{}, err
	}
	micros, task, found := strings.Cut(string(raw), ".")
	if !found {
		return application.TaskCursor{}, fmt.Errorf("malformed cursor")
	}
	value, err := strconv.ParseInt(micros, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != micros {
		return application.TaskCursor{}, fmt.Errorf("malformed cursor")
	}
	id, err := ids.Parse[ids.Task](task)
	if err != nil {
		return application.TaskCursor{}, err
	}
	return application.TaskCursor{CreatedAt: time.UnixMicro(value).UTC(), Task: id}, nil
}

// ProviderBody is a provider a surface may name: what a person needs to choose
// one. HarnessMode is a runtime's only (ADR-0028).
type ProviderBody struct {
	Provider    ids.ProviderID        `json:"provider_id"`
	Role        contracts.Role        `json:"role"`
	Name        string                `json:"name,omitempty"`
	HarnessMode lifecycle.HarnessMode `json:"harness_mode,omitempty"`
}

// ProviderListBody lists the providers a surface may name.
type ProviderListBody struct {
	Providers []ProviderBody `json:"providers"`
}

func (a *API) listProviders(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	listings, err := application.ListProviders(r.Context(), a.pool, actor, contracts.Role(r.URL.Query().Get("role")))
	if err != nil {
		return err
	}
	body := ProviderListBody{Providers: make([]ProviderBody, 0, len(listings))}
	for _, l := range listings {
		body.Providers = append(body.Providers, ProviderBody{Provider: l.Provider, Role: l.Role, Name: l.Name, HarnessMode: l.HarnessMode})
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}
