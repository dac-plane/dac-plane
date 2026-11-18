//go:build integration

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/httpapi"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
)

func submitAs(t *testing.T, s *surface, subject, key string) ids.TaskID {
	t.Helper()
	return decodeInto[httpapi.SubmissionBody](t, s.do(t, call{method: "POST", path: "/tasks", as: subject, key: key, body: s.submitBody()}), http.StatusCreated).Task.ID
}

func listTasks(t *testing.T, s *surface, subject string, query url.Values) httpapi.TaskListBody {
	t.Helper()
	path := "/tasks"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return decodeInto[httpapi.TaskListBody](t, s.do(t, call{method: "GET", path: path, as: subject}), http.StatusOK)
}

func taskIDs(page httpapi.TaskListBody) []ids.TaskID {
	out := make([]ids.TaskID, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		out = append(out, task.ID)
	}
	return out
}

// A page boundary names a position that never moves, so tasks submitted between
// two pages are neither skipped nor repeated (ADR-0028).
func TestTaskListPagesAreStableUnderSubmissions(t *testing.T) {
	s := newSurface(t)
	var submitted []ids.TaskID
	for i := range 5 {
		submitted = append(submitted, submitAs(t, s, "owner", "list-"+strconv.Itoa(i)))
	}
	newestFirst := slices.Clone(submitted)
	slices.Reverse(newestFirst)

	first := listTasks(t, s, "owner", url.Values{"limit": {"2"}})
	if !slices.Equal(taskIDs(first), newestFirst[:2]) || first.NextCursor == "" {
		t.Fatalf("first page = %v next %q, want the two newest", taskIDs(first), first.NextCursor)
	}
	// A task submitted between pages sorts before the first page's start.
	late := submitAs(t, s, "owner", "list-late")
	var seen []ids.TaskID
	seen = append(seen, taskIDs(first)...)
	cursor := first.NextCursor
	for cursor != "" {
		page := listTasks(t, s, "owner", url.Values{"limit": {"2"}, "cursor": {cursor}})
		seen = append(seen, taskIDs(page)...)
		cursor = page.NextCursor
	}
	if !slices.Equal(seen, newestFirst) {
		t.Fatalf("pages = %v, want %v with no skip or repeat and without the late task %s", seen, newestFirst, late)
	}
	if fresh := listTasks(t, s, "owner", nil); len(fresh.Tasks) != 6 || fresh.Tasks[0].ID != late || fresh.NextCursor != "" {
		t.Fatalf("fresh listing = %v next %q, want six with the late task first", taskIDs(fresh), fresh.NextCursor)
	}

	// A task that changes does not move.
	view := s.do(t, call{method: "GET", path: "/tasks/" + submitted[0].String(), as: "owner"})
	request := call{method: "POST", path: "/tasks/" + submitted[0].String() + "/cancel", as: "owner", key: "list-cancel", ifMatch: view.header.Get("ETag"), body: httpapi.CancelRequest{}}
	s.do(t, request).want(t, http.StatusAccepted)
	if again := listTasks(t, s, "owner", nil); !slices.Equal(taskIDs(again), append([]ids.TaskID{late}, newestFirst...)) {
		t.Fatalf("listing after a cancellation = %v, want the same order", taskIDs(again))
	}
}

func TestTaskListShowsWhatThePrincipalMayRead(t *testing.T) {
	s := newSurface(t)
	mine := submitAs(t, s, "owner", "scope-owner")
	if other := listTasks(t, s, "other", nil); len(other.Tasks) != 0 {
		t.Fatalf("another principal lists %v, want none", taskIDs(other))
	}
	if reader := listTasks(t, s, "reader", nil); !slices.Equal(taskIDs(reader), []ids.TaskID{mine}) {
		t.Fatalf("a tenant reader lists %v, want the tenant's task", taskIDs(reader))
	}
	// A cursor taken from a reader's page shows another principal nothing more.
	submitAs(t, s, "owner", "scope-owner-2")
	page := listTasks(t, s, "reader", url.Values{"limit": {"1"}})
	if page.NextCursor == "" {
		t.Fatal("no next cursor")
	}
	if leaked := listTasks(t, s, "other", url.Values{"cursor": {page.NextCursor}}); len(leaked.Tasks) != 0 {
		t.Fatalf("another principal lists %v with a reader's cursor", taskIDs(leaked))
	}
}

func TestTaskListFilters(t *testing.T) {
	s := newSurface(t)
	cancelled := submitAs(t, s, "owner", "filter-cancel")
	kept := submitAs(t, s, "owner", "filter-keep")
	view := s.do(t, call{method: "GET", path: "/tasks/" + cancelled.String(), as: "owner"})
	s.do(t, call{method: "POST", path: "/tasks/" + cancelled.String() + "/cancel", as: "owner", key: "filter-cancel-it", ifMatch: view.header.Get("ETag"), body: httpapi.CancelRequest{}}).want(t, http.StatusAccepted)
	state := decodeInto[httpapi.TaskBody](t, s.do(t, call{method: "GET", path: "/tasks/" + kept.String(), as: "owner"}), http.StatusOK).Task.State
	byState := listTasks(t, s, "owner", url.Values{"state": {string(state)}})
	for _, task := range byState.Tasks {
		if task.State != state {
			t.Fatalf("state filter returned a %s task", task.State)
		}
	}
	if !slices.Contains(taskIDs(byState), kept) {
		t.Fatalf("state %s lists %v, want %s", state, taskIDs(byState), kept)
	}
	if byAgent := listTasks(t, s, "owner", url.Values{"agent": {mustID[ids.Agent](t).String()}}); len(byAgent.Tasks) != 0 {
		t.Fatalf("an agent with no tasks lists %v", taskIDs(byAgent))
	}
}

// A surface sees what it needs to choose a provider and nothing operator-only
// (ADR-0028).
func TestProviderListHoldsOnlyWhatAPersonNeeds(t *testing.T) {
	s := newSurface(t)
	named := mustID[ids.Provider](t)
	s.seed(t, func(u *store.UnitOfWork) error {
		if err := store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: named, Role: contracts.RoleRuntime, Name: "Codex on Camer", UpdatedAt: s.at}); err != nil {
			return err
		}
		// The fixture's runtime has a live harness; this registers it too.
		if err := store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: s.provider, Role: contracts.RoleRuntime, UpdatedAt: s.at}); err != nil {
			return err
		}
		// Neither of these is a provider a surface names.
		return store.PutProviderRegistration(s.ctx, u, contracts.ProviderRegistration{Tenant: s.tenant, Provider: mustID[ids.Provider](t), Role: contracts.RoleArtifactStore, UpdatedAt: s.at})
	})
	for _, subject := range []string{"owner", "other", "reader"} {
		response := s.do(t, call{method: "GET", path: "/providers", as: subject})
		var raw struct {
			Providers []map[string]json.RawMessage `json:"providers"`
		}
		if err := json.Unmarshal(response.body, &raw); err != nil || response.status != http.StatusOK {
			t.Fatalf("%s: %d %s %v", subject, response.status, response.body, err)
		}
		roles := map[string]int{}
		for _, provider := range raw.Providers {
			for key := range provider {
				if !slices.Contains([]string{"provider_id", "role", "name", "harness_mode"}, key) {
					t.Fatalf("%s: a provider carries %q, which is not for a surface: %s", subject, key, response.body)
				}
			}
			var role string
			_ = json.Unmarshal(provider["role"], &role)
			roles[role]++
		}
		if roles["runtime"] != 2 || roles["environment"] != 1 || len(roles) != 2 {
			t.Fatalf("%s: roles = %v, want two runtimes and one environment and no artifact store: %s", subject, roles, response.body)
		}
	}
	list := decodeInto[httpapi.ProviderListBody](t, s.do(t, call{method: "GET", path: "/providers?role=runtime", as: "owner"}), http.StatusOK)
	var found httpapi.ProviderBody
	for _, provider := range list.Providers {
		if provider.Provider == named {
			found = provider
		}
		if provider.Role != contracts.RoleRuntime {
			t.Fatalf("role filter returned a %s", provider.Role)
		}
	}
	if found.Name != "Codex on Camer" || found.HarnessMode != "" {
		t.Fatalf("named runtime = %+v, want its name and no harness yet", found)
	}
	for _, provider := range list.Providers {
		if provider.Provider == s.provider && provider.HarnessMode != lifecycle.HarnessLive {
			t.Fatalf("the fixture's runtime = %+v, want a live harness", provider)
		}
	}
	s.do(t, call{method: "GET", path: "/providers?role=artifact_store", as: "owner"}).want(t, http.StatusUnprocessableEntity)
}
