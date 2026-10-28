package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

type problemCase struct {
	name   string
	err    error
	status int
}

func problemCases() []problemCase {
	return []problemCase{
		{"unauthenticated", errUnauthenticated, http.StatusUnauthorized},
		{"authn refusal", authn.ErrUnauthenticated, http.StatusUnauthorized},
		{"malformed JSON", errMalformedJSON, http.StatusBadRequest},
		{"missing key", errMissingKey, http.StatusBadRequest},
		{"malformed header", errMalformedHeader, http.StatusBadRequest},
		{"unauthorized principal", application.ErrUnauthorized, http.StatusForbidden},
		{"not found", application.ErrNotFound, http.StatusNotFound},
		{"lifecycle refusal", &lifecycle.TransitionError{Code: lifecycle.RejectTerminalState}, http.StatusConflict},
		{"unauthorized actor", &lifecycle.TransitionError{Code: lifecycle.RejectUnauthorizedActor}, http.StatusForbidden},
		{"stale version", fmt.Errorf("wrapped: %w", &lifecycle.TransitionError{Code: lifecycle.RejectVersionConflict}), http.StatusPreconditionFailed},
		{"missing precondition", errMissingPrecondition, http.StatusPreconditionRequired},
		{"payload mismatch", application.ErrPayloadMismatch, http.StatusUnprocessableEntity},
		{"no harness", application.ErrNoHarness, http.StatusUnprocessableEntity},
		{"invalid command", fmt.Errorf("%w: x", application.ErrInvalidCommand), http.StatusUnprocessableEntity},
		{"invalid lifecycle value", lifecycle.ErrInvalid, http.StatusUnprocessableEntity},
		{"invalid identity value", identity.ErrInvalid, http.StatusUnprocessableEntity},
		{"operational failure", errors.New("connection reset by peer: secret-dsn"), http.StatusInternalServerError},
	}
}

func TestProblemForMapsEveryErrorCategory(t *testing.T) {
	for _, tc := range problemCases() {
		t.Run(tc.name, func(t *testing.T) {
			problem := problemFor(tc.err)
			if problem.Status != tc.status {
				t.Fatalf("status = %d, want %d", problem.Status, tc.status)
			}
			if strings.Contains(problem.Detail+problem.Title, "secret-dsn") {
				t.Fatalf("problem echoes the error: %+v", problem)
			}
		})
	}
}

// fakeAuth accepts one token for one tenant.
type fakeAuth struct {
	token  string
	actor  identity.AuthorizationContext
	failed error
}

func (f fakeAuth) Authenticate(_ context.Context, token string, tenant ids.TenantID) (identity.AuthorizationContext, error) {
	if f.failed != nil {
		return identity.AuthorizationContext{}, f.failed
	}
	if token != f.token || tenant != f.actor.Tenant() {
		return identity.AuthorizationContext{}, authn.ErrUnauthenticated
	}
	return f.actor, nil
}

// newUnitAPI returns an API with no database: any request that reaches an
// application call panics, so these tests prove refusals happen first.
func newUnitAPI(t *testing.T) (*API, ids.TenantID, *bytes.Buffer) {
	t.Helper()
	principal := identity.Principal{Tenant: newID[ids.Tenant](t), ID: newID[ids.Principal](t), Kind: identity.PrincipalHuman}
	actor, err := identity.NewAuthorizationContext(principal)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	api := &API{auth: fakeAuth{token: "good.token.value", actor: actor}, logger: slog.New(slog.NewJSONHandler(logs, nil)), mux: http.NewServeMux()}
	api.options.Now = func() time.Time { return testNow }
	for _, route := range api.routes() {
		api.mux.Handle(route.pattern, route.handler)
	}
	return api, principal.Tenant, logs
}

type unitRequest struct {
	method, path, body string
	header             map[string]string
}

func (a *API) do(t *testing.T, req unitRequest) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(req.method, req.path, strings.NewReader(req.body))
	for name, value := range req.header {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	a.ServeHTTP(recorder, request)
	return recorder
}

func TestRequestsAreRefusedBeforeAnyCommandRuns(t *testing.T) {
	api, tenant, logs := newUnitAPI(t)
	base := "/v1/tenants/" + tenant.String()
	task := newID[ids.Task](t).String()
	migration := newID[ids.Migration](t).String()
	grant := newID[ids.Grant](t).String()
	auth := map[string]string{"Authorization": "Bearer good.token.value"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"Authorization": auth["Authorization"]}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	validSubmit := `{"provider_id":"` + newID[ids.Provider](t).String() + `","objective":"Update the fixture","acceptance_criteria":["The fixture is updated"],"deadline":"2026-10-24T00:00:00Z","unit":"usd_cents","ceiling":10,"amount":5}`
	validMigration := `{"from_provider_id":"` + newID[ids.Provider](t).String() + `","to_provider_id":"` + newID[ids.Provider](t).String() + `"}`
	cases := []struct {
		name   string
		req    unitRequest
		status int
	}{
		{"no bearer token", unitRequest{"GET", base + "/tasks/" + task, "", nil}, 401},
		{"wrong token", unitRequest{"GET", base + "/tasks/" + task, "", map[string]string{"Authorization": "Bearer other.token.value"}}, 401},
		{"basic scheme", unitRequest{"GET", base + "/tasks/" + task, "", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}}, 401},
		{"malformed tenant", unitRequest{"GET", "/v1/tenants/not-a-tenant/tasks/" + task, "", auth}, 401},
		{"another tenant", unitRequest{"GET", "/v1/tenants/" + newID[ids.Tenant](t).String() + "/tasks/" + task, "", auth}, 401},
		{"malformed task id", unitRequest{"GET", base + "/tasks/nope", "", auth}, 404},
		{"submit without key", unitRequest{"POST", base + "/tasks", validSubmit, auth}, 400},
		{"submit oversized key", unitRequest{"POST", base + "/tasks", validSubmit, with(map[string]string{"Idempotency-Key": strings.Repeat("k", 129)})}, 400},
		{"submit malformed JSON", unitRequest{"POST", base + "/tasks", `{"provider_id":`, with(map[string]string{"Idempotency-Key": "k1"})}, 400},
		{"submit unknown field", unitRequest{"POST", base + "/tasks", strings.Replace(validSubmit, "{", `{"unexpected":"x",`, 1), with(map[string]string{"Idempotency-Key": "k1"})}, 400},
		{"cancel null body", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `null`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"3"`})}, 400},
		{"cancel trailing bracket", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}]`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"3"`})}, 400},
		{"cancel trailing brace", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}}`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"3"`})}, 400},
		{"cancel array body", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `[{}]`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"3"`})}, 400},
		{"cancel empty body", unitRequest{"POST", base + "/tasks/" + task + "/cancel", ``, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"3"`})}, 400},
		{"submit two objects", unitRequest{"POST", base + "/tasks", validSubmit + validSubmit, with(map[string]string{"Idempotency-Key": "k1"})}, 400},
		{"submit oversized body", unitRequest{"POST", base + "/tasks", `{"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`, with(map[string]string{"Idempotency-Key": "k1"})}, 400},
		{"submit invalid body", unitRequest{"POST", base + "/tasks", strings.Replace(validSubmit, `"amount":5`, `"amount":0`, 1), with(map[string]string{"Idempotency-Key": "k1"})}, 422},
		{"cancel without If-Match", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}`, with(map[string]string{"Idempotency-Key": "k2"})}, 428},
		{"cancel weak If-Match", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `W/"3"`})}, 400},
		{"cancel wildcard If-Match", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `*`})}, 400},
		{"cancel zero version", unitRequest{"POST", base + "/tasks/" + task + "/cancel", `{}`, with(map[string]string{"Idempotency-Key": "k2", "If-Match": `"0"`})}, 400},
		{"decide invalid body", unitRequest{"POST", base + "/attention/" + newID[ids.Attention](t).String() + "/decisions", `{"kind":"answer"}`, with(map[string]string{"Idempotency-Key": "k3", "If-Match": `"2"`})}, 422},
		{"review invalid body", unitRequest{"POST", base + "/tasks/" + task + "/reviews", `{"disposition":"accept"}`, with(map[string]string{"Idempotency-Key": "k4", "If-Match": `"2"`})}, 422},
		{"malformed migration id", unitRequest{"GET", base + "/migration-passes/nope", "", auth}, 404},
		{"create migration without key", unitRequest{"POST", base + "/migration-passes", validMigration, auth}, 400},
		{"create migration malformed body", unitRequest{"POST", base + "/migration-passes", `{"from_provider_id":`, with(map[string]string{"Idempotency-Key": "m1"})}, 400},
		{"create migration unknown field", unitRequest{"POST", base + "/migration-passes", strings.Replace(validMigration, "{", `{"force":true,`, 1), with(map[string]string{"Idempotency-Key": "m1"})}, 400},
		{"migration command without If-Match", unitRequest{"POST", base + "/migration-passes/" + migration + "/preview", `{}`, with(map[string]string{"Idempotency-Key": "m2"})}, 428},
		{"migration command unknown field", unitRequest{"POST", base + "/migration-passes/" + migration + "/preview", `{"force":true}`, with(map[string]string{"Idempotency-Key": "m2", "If-Match": `"1"`})}, 400},
		{"malformed grant id", unitRequest{"POST", base + "/authority-grants/nope/revoke", `{}`, with(map[string]string{"Idempotency-Key": "r1"})}, 404},
		{"revoke without key", unitRequest{"POST", base + "/authority-grants/" + grant + "/revoke", `{}`, auth}, 400},
		{"revoke nonempty body", unitRequest{"POST", base + "/authority-grants/" + grant + "/revoke", `{"force":true}`, with(map[string]string{"Idempotency-Key": "r1"})}, 400},
		{"events bad limit", unitRequest{"GET", base + "/events?limit=0", "", auth}, 422},
		{"unknown route", unitRequest{"GET", base + "/tasks", "", auth}, 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := api.do(t, tc.req)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", response.Code, tc.status, response.Body)
			}
			if tc.status == 405 {
				return
			}
			if got := response.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("Content-Type = %q", got)
			}
			var problem Problem
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil || problem.Status != tc.status {
				t.Fatalf("problem = %+v, %v", problem, err)
			}
			if tc.status == 401 && response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("401 without WWW-Authenticate: Bearer")
			}
		})
	}
	if strings.Contains(logs.String(), "good.token.value") || strings.Contains(logs.String(), "other.token.value") {
		t.Fatalf("token logged: %s", logs)
	}
}

func TestAuthenticatorFailureIsNotReportedAsUnauthenticated(t *testing.T) {
	api, tenant, logs := newUnitAPI(t)
	api.auth = fakeAuth{failed: errors.New("database unavailable")}
	response := api.do(t, unitRequest{"GET", "/v1/tenants/" + tenant.String() + "/attention", "", map[string]string{"Authorization": "Bearer good.token.value"}})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), "database unavailable") {
		t.Fatalf("internal error echoed: %s", body)
	}
	if !strings.Contains(logs.String(), "database unavailable") {
		t.Fatal("operational failure not logged")
	}
}

func TestCursorRoundTripAndRejection(t *testing.T) {
	tenant, task := newID[ids.Tenant](t), newID[ids.Task](t)
	cursor := application.EventCursor{Generation: 3, Position: 42, Stream: application.StreamTag(tenant, task)}
	if parsed, err := parseCursor(formatCursor(cursor)); err != nil || parsed != cursor {
		t.Fatalf("round trip = %+v, %v", parsed, err)
	}
	if application.StreamTag(tenant, task) == application.StreamTag(tenant, ids.TaskID{}) ||
		application.StreamTag(tenant, task) == application.StreamTag(newID[ids.Tenant](t), task) {
		t.Fatal("stream tags collide across scope or tenant")
	}
	for _, text := range []string{"", "3", "3.4", "0.1." + cursor.Stream, "3.-1." + cursor.Stream, "a.b." + cursor.Stream,
		"3.4.5", "3.4." + strings.ToUpper(cursor.Stream), "3.04." + cursor.Stream, "3. 4." + cursor.Stream, "3.4." + cursor.Stream + ".x"} {
		if _, err := parseCursor(text); err == nil {
			t.Errorf("parseCursor(%q) accepted", text)
		}
	}
}
