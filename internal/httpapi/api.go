// Package httpapi serves Surface API v1 (ADR-0007): JSON over HTTP under
// /v1/tenants/{tenant}, described by contracts/openapi/surface-v1.yaml.
//
// It is a thin transport over internal/application. Each handler
// authenticates the caller for the tenant in the path, decodes and validates
// the body, calls one application function and maps its result or error. No
// handler reads or writes the store or holds a lifecycle rule. Serving the
// handler on a listener with TLS is the job of the process (ADR-0009).
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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/authn"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const (
	maxBodyBytes           = 64 << 10
	maxSubmitTaskBodyBytes = 1 << 20
	maxIdempotencyKey      = 128
	defaultPageSize        = 100
	prefix                 = "/v1/tenants/{tenant}"
)

// Authenticator establishes the principal of a request for one tenant.
// *authn.Authenticator implements it.
type Authenticator interface {
	Authenticate(ctx context.Context, token string, tenant ids.TenantID) (identity.AuthorizationContext, error)
}

// Options tune the API. Zero values take the defaults.
type Options struct {
	// Now supplies command time; defaults to time.Now.
	Now func() time.Time
	// PollInterval is how often an event stream checks for new events.
	PollInterval time.Duration
	// Heartbeat is how often an idle event stream sends a comment.
	Heartbeat time.Duration
	// StreamLifetime bounds one event stream connection; the client resumes
	// with Last-Event-ID and is authenticated again.
	StreamLifetime time.Duration
}

// API is the Surface API handler.
type API struct {
	pool    *pgxpool.Pool
	auth    Authenticator
	logger  *slog.Logger
	options Options
	mux     *http.ServeMux
}

// New returns the Surface API.
func New(pool *pgxpool.Pool, auth Authenticator, logger *slog.Logger, options Options) (*API, error) {
	if pool == nil || auth == nil || logger == nil {
		return nil, errors.New("httpapi: pool, authenticator and logger are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.Heartbeat <= 0 {
		options.Heartbeat = 15 * time.Second
	}
	if options.StreamLifetime <= 0 {
		options.StreamLifetime = 10 * time.Minute
	}
	api := &API{pool: pool, auth: auth, logger: logger, options: options, mux: http.NewServeMux()}
	for _, route := range api.routes() {
		api.mux.Handle(route.pattern, route.handler)
	}
	return api, nil
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

type route struct {
	pattern string
	handler http.Handler
}

// Routes lists every pattern the API serves, for the contract conformance
// test.
func (a *API) Routes() []string {
	var patterns []string
	for _, route := range a.routes() {
		patterns = append(patterns, route.pattern)
	}
	return patterns
}

func (a *API) routes() []route {
	return []route{
		{"POST " + prefix + "/tasks", a.authenticated(a.submitTask)},
		{"GET " + prefix + "/tasks/{task}", a.authenticated(a.getTask)},
		{"POST " + prefix + "/tasks/{task}/spend-assertions", a.authenticated(a.assertSpend)},
		{"POST " + prefix + "/tasks/{task}/cancel", a.authenticated(a.cancelTask)},
		{"POST " + prefix + "/tasks/{task}/reviews", a.authenticated(a.reviewResult)},
		{"GET " + prefix + "/tasks/{task}/attempts/{attempt}", a.authenticated(a.getAttempt)},
		{"GET " + prefix + "/tasks/{task}/revisions/{revision}", a.authenticated(a.getRevision)},
		{"GET " + prefix + "/tasks/{task}/events", a.authenticated(a.taskEvents)},
		{"GET " + prefix + "/attention", a.authenticated(a.listAttention)},
		{"GET " + prefix + "/attention/{attention}", a.authenticated(a.getAttention)},
		{"POST " + prefix + "/attention/{attention}/decisions", a.authenticated(a.decide)},
		{"GET " + prefix + "/events", a.authenticated(a.tenantEvents)},
		{"POST " + prefix + "/migration-passes", a.authenticated(a.createMigration)},
		{"GET " + prefix + "/migration-passes/{migration}", a.authenticated(a.getMigration)},
		{"POST " + prefix + "/migration-passes/{migration}/preview", a.authenticated(a.previewMigration)},
		{"POST " + prefix + "/migration-passes/{migration}/hold", a.authenticated(a.holdMigration)},
		{"POST " + prefix + "/migration-passes/{migration}/release", a.authenticated(a.releaseMigration)},
		{"POST " + prefix + "/migration-passes/{migration}/execute", a.authenticated(a.executeMigration)},
		{"POST " + prefix + "/migration-passes/{migration}/cancel", a.authenticated(a.cancelMigration)},
		{"POST " + prefix + "/authority-grants/{grant}/revoke", a.authenticated(a.revokeAuthority)},
	}
}

// handlerFunc is a handler that receives the authenticated actor and returns
// an error for the API to map, after writing nothing.
type handlerFunc func(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error

// authenticated authenticates the bearer token for the tenant in the path.
// Every authentication failure is the same 401.
func (a *API) authenticated(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := ids.Parse[ids.Tenant](r.PathValue("tenant"))
		if err != nil {
			a.problem(w, r, errUnauthenticated)
			return
		}
		token, ok := authn.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.problem(w, r, errUnauthenticated)
			return
		}
		actor, err := a.auth.Authenticate(r.Context(), token, tenant)
		if err != nil {
			if !errors.Is(err, authn.ErrUnauthenticated) {
				a.logger.ErrorContext(r.Context(), "authentication failed", "error", err)
				a.problem(w, r, err)
				return
			}
			a.problem(w, r, errUnauthenticated)
			return
		}
		if err := next(w, r, actor); err != nil {
			a.problem(w, r, err)
		}
	})
}

var (
	errUnauthenticated     = errors.New("unauthenticated")
	errMalformedJSON       = errors.New("malformed JSON body")
	errMissingKey          = errors.New("Idempotency-Key is required")
	errMissingPrecondition = errors.New("If-Match is required")
	errMalformedHeader     = errors.New("malformed request header")
)

// pathID parses a path identifier. A malformed identifier names nothing the
// caller may see, so it is not found.
func pathID[K any](r *http.Request, name string) (ids.ID[K], error) {
	id, err := ids.Parse[K](r.PathValue(name))
	if err != nil {
		return ids.ID[K]{}, application.ErrNotFound
	}
	return id, nil
}

// decode reads a JSON body strictly: one object, no unknown fields, within
// the size limit, then validates it.
func decode[T interface{ Validate() error }](r *http.Request) (T, error) {
	return decodeLimited[T](r, maxBodyBytes)
}

func decodeLimited[T interface{ Validate() error }](r *http.Request, limit int64) (T, error) {
	var body T
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return body, errMalformedJSON
	}
	// Exactly one JSON object and nothing after it: null, arrays and
	// trailing tokens are malformed, not an empty body.
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return body, errMalformedJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, errMalformedJSON
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return body, errMalformedJSON
	}
	return body, body.Validate()
}

// command builds the command envelope from the Idempotency-Key and, when
// conditional, the If-Match version.
func command(r *http.Request, actor identity.AuthorizationContext, conditional bool) (application.Command, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return application.Command{}, errMissingKey
	}
	if len(key) > maxIdempotencyKey {
		return application.Command{}, errMalformedHeader
	}
	cmd := application.Command{Actor: actor, Nonce: key}
	if !conditional {
		return cmd, nil
	}
	version, err := ifMatch(r.Header.Get("If-Match"))
	if err != nil {
		return application.Command{}, err
	}
	cmd.ExpectedVersion = version
	return cmd, nil
}

// ifMatch parses one strong entity tag carrying a record version.
func ifMatch(header string) (uint64, error) {
	if header == "" {
		return 0, errMissingPrecondition
	}
	if len(header) < 3 || header[0] != '"' || header[len(header)-1] != '"' {
		return 0, errMalformedHeader
	}
	version, err := strconv.ParseUint(header[1:len(header)-1], 10, 64)
	if err != nil || version == 0 {
		return 0, errMalformedHeader
	}
	return version, nil
}

func etag(version uint64) string {
	return `"` + strconv.FormatUint(version, 10) + `"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// problemFor maps an error to its RFC 9457 problem. Details are fixed text
// per category: nothing from the request, a token or an internal error is
// echoed.
func problemFor(err error) Problem {
	var refusal *lifecycle.TransitionError
	switch {
	case errors.Is(err, errUnauthenticated), errors.Is(err, authn.ErrUnauthenticated):
		return newProblem(http.StatusUnauthorized, "unauthenticated", "Unauthenticated", "")
	case errors.Is(err, errMalformedJSON):
		return newProblem(http.StatusBadRequest, "malformed-body", "Malformed request body", "The body must be one JSON object with only documented fields.")
	case errors.Is(err, errMissingKey):
		return newProblem(http.StatusBadRequest, "idempotency-key-required", "Idempotency-Key is required", "")
	case errors.Is(err, errMalformedHeader):
		return newProblem(http.StatusBadRequest, "malformed-header", "Malformed request header", "")
	case errors.Is(err, application.ErrUnauthorized):
		return newProblem(http.StatusForbidden, "forbidden", "Forbidden", "")
	case errors.Is(err, application.ErrNotFound):
		return newProblem(http.StatusNotFound, "not-found", "Not found", "")
	case errors.As(err, &refusal) && refusal.Code == lifecycle.RejectUnauthorizedActor:
		return newProblem(http.StatusForbidden, "forbidden", "Forbidden", "")
	case errors.As(err, &refusal) && refusal.Code == lifecycle.RejectVersionConflict:
		return newProblem(http.StatusPreconditionFailed, "stale-version", "Stale version", "Read the resource again and retry with its current ETag.")
	case errors.As(err, &refusal):
		return newProblem(http.StatusConflict, "refused", "Refused by the lifecycle", string(refusal.Code))
	case errors.Is(err, errMissingPrecondition):
		return newProblem(http.StatusPreconditionRequired, "precondition-required", "If-Match is required", "")
	case errors.Is(err, application.ErrPayloadMismatch):
		return newProblem(http.StatusUnprocessableEntity, "idempotency-key-reused", "Idempotency-Key is already used", "The key was used with a different request.")
	case errors.Is(err, application.ErrNoHarness):
		return newProblem(http.StatusUnprocessableEntity, "no-harness", "No qualified harness for the provider", "")
	case errors.Is(err, application.ErrInvalidCommand), errors.Is(err, lifecycle.ErrInvalid), errors.Is(err, identity.ErrInvalid):
		return newProblem(http.StatusUnprocessableEntity, "invalid-command", "Invalid command", "")
	}
	return newProblem(http.StatusInternalServerError, "internal", "Internal error", "")
}

func newProblem(status int, kind, title, detail string) Problem {
	return Problem{Type: "https://dac-plane.dev/problems/" + kind, Title: title, Status: status, Detail: detail}
}

func (a *API) problem(w http.ResponseWriter, r *http.Request, err error) {
	problem := problemFor(err)
	if problem.Status == http.StatusInternalServerError {
		a.logger.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	if problem.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
}

// formatCursor renders an opaque cursor: generation, position and the tag of
// the stream it belongs to. parseCursor is its inverse.
func formatCursor(c application.EventCursor) string {
	return fmt.Sprintf("%d.%d.%s", c.Generation, c.Position, c.Stream)
}

func parseCursor(text string) (application.EventCursor, error) {
	parts := strings.Split(text, ".")
	invalid := fmt.Errorf("%w: malformed cursor", application.ErrInvalidCommand)
	if len(parts) != 3 || len(parts[2]) != 12 || strings.Trim(parts[2], "0123456789abcdef") != "" {
		return application.EventCursor{}, invalid
	}
	g, gErr := strconv.ParseUint(parts[0], 10, 64)
	p, pErr := strconv.ParseInt(parts[1], 10, 64)
	if gErr != nil || pErr != nil || g == 0 || p < 0 || strconv.FormatUint(g, 10) != parts[0] || strconv.FormatInt(p, 10) != parts[1] {
		return application.EventCursor{}, invalid
	}
	return application.EventCursor{Generation: g, Position: p, Stream: parts[2]}, nil
}
