package githubadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
)

func catalogue() any {
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	text := map[string]any{"type": "string", "minLength": 1, "maxLength": 100}
	number := map[string]any{"type": "integer", "minimum": 1}
	tool := func(name, description string, schema any, readOnly bool) any {
		return map[string]any{"name": name, "description": description, "input_schema": schema, "read_only": readOnly}
	}
	return map[string]any{"tools": []any{
		tool("get_issue", "Read an issue's number, title, state and URL", object(map[string]any{"owner": text, "repo": text, "number": number}, "owner", "repo", "number"), true),
		tool("create_comment", "Add a comment and return its ID and URL", object(map[string]any{"owner": text, "repo": text, "number": number, "body": map[string]any{"type": "string", "minLength": 1, "maxLength": 10000}}, "owner", "repo", "number", "body"), false),
		tool("list_repositories", "List the first 30 accessible repositories", object(map[string]any{}), true),
	}}
}
func (a *Adapter) call(ctx context.Context, r wire.Request) (wire.Reply, error) {
	var e execution
	if strict(r.Arguments, &e) != nil || e.Request.Tenant != r.Tenant || e.Request.Attempt != r.Attempt || e.Request.Operation != r.Operation || e.Request.Provider != a.cfg.ProviderID || e.Request.Action != "connector."+e.Tool || e.Request.Scope.Class != "subject" || e.Request.Scope.Target != "connection:"+e.Connection || !uuid.MatchString(e.Grant) || !uuid.MatchString(e.ConnectionGrant) || !uuid.MatchString(e.Connection) || len(e.Input) == 0 || e.Input[0] != '{' || len(e.Input) > 32<<10 || digest(e.Input) != e.Request.InputDigest || (e.Source != "standing_grant" && e.Source != "one_time_approval") || (e.Source == "one_time_approval" && !uuid.MatchString(e.Decision)) {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	canonical, err := json.Marshal(e.Input)
	if err != nil || !bytes.Equal(canonical, e.Input) {
		return failed(r, false, wire.FailureInvalidArguments), nil
	}
	var path, method string
	var body []byte
	switch e.Tool {
	case "get_issue", "create_comment":
		var input struct {
			Owner  string `json:"owner"`
			Repo   string `json:"repo"`
			Number int64  `json:"number"`
			Body   string `json:"body,omitempty"`
		}
		if strict(e.Input, &input) != nil || !segment.MatchString(input.Owner) || !segment.MatchString(input.Repo) || input.Owner == "." || input.Owner == ".." || input.Repo == "." || input.Repo == ".." || input.Number <= 0 {
			return failed(r, false, wire.FailureInvalidArguments), nil
		}
		path = "/repos/" + url.PathEscape(input.Owner) + "/" + url.PathEscape(input.Repo) + "/issues/" + strconv.FormatInt(input.Number, 10)
		method = "GET"
		if e.Tool == "create_comment" {
			if input.Body == "" || len(input.Body) > 10000 {
				return failed(r, false, wire.FailureInvalidArguments), nil
			}
			path += "/comments"
			method = "POST"
			body, _ = json.Marshal(map[string]string{"body": input.Body})
		} else if input.Body != "" {
			return failed(r, false, wire.FailureInvalidArguments), nil
		}
	case "list_repositories":
		var input struct{}
		if strict(e.Input, &input) != nil {
			return failed(r, false, wire.FailureInvalidArguments), nil
		}
		path = "/user/repos?per_page=30&sort=full_name"
		method = "GET"
	default:
		return failed(r, false, wire.FailureUnknownOperation), nil
	}
	key := callKey(r.Tenant, r.Operation)
	a.callsMu.Lock()
	var record callRecord
	err = a.cfg.Vault.load(key, &record)
	if err == nil {
		a.callsMu.Unlock()
		if record.Hash != digest(r.Arguments) || record.Tenant != r.Tenant || record.Attempt != r.Attempt {
			return failed(r, false, wire.FailureStalePrecondition), nil
		}
		if record.Result != nil {
			return accepted(r, record.Result), nil
		}
		return failed(r, true, wire.FailureResourceBusy), nil
	}
	if !errors.Is(err, errAbsent) {
		a.callsMu.Unlock()
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	record = callRecord{Hash: digest(r.Arguments), Tenant: r.Tenant, Attempt: r.Attempt, State: "preparing"}
	if err := a.cfg.Vault.save(key, record); err != nil {
		a.callsMu.Unlock()
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	callCtx, cancel := context.WithCancel(ctx)
	a.active[r.Operation] = cancel
	a.callsMu.Unlock()
	defer func() { cancel(); a.callsMu.Lock(); delete(a.active, r.Operation); a.callsMu.Unlock() }()
	credential, err := a.credential(callCtx, r.Tenant, e.Connection)
	if err != nil {
		return a.finish(r, key, record, map[string]any{"error": "connection requires authorization"}, true)
	}
	a.callsMu.Lock()
	var cancelled bool
	tombstoneErr := a.cfg.Vault.load("cancel:"+r.Operation, &cancelled)
	if tombstoneErr != nil && !errors.Is(tombstoneErr, errAbsent) {
		a.callsMu.Unlock()
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	if cancelled || callCtx.Err() != nil {
		a.callsMu.Unlock()
		return a.finish(r, key, record, map[string]any{"cancelled": true, "external_request_sent": false}, true)
	}
	record.State = "sent"
	err = a.cfg.Vault.save(key, record)
	a.callsMu.Unlock()
	if err != nil {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	var output any
	switch e.Tool {
	case "get_issue":
		var issue struct {
			Number int64  `json:"number"`
			Title  string `json:"title"`
			State  string `json:"state"`
			URL    string `json:"html_url"`
		}
		status, err := a.api(callCtx, credential.AccessToken, method, path, body, &issue)
		if err != nil {
			if status >= 400 && status < 500 {
				return a.finish(r, key, record, map[string]any{"error": "GitHub refused the request", "status": status}, true)
			}
			return failed(r, true, wire.FailureTransportUnavailable), nil
		}
		output = issue
	case "create_comment":
		var comment struct {
			ID  int64  `json:"id"`
			URL string `json:"html_url"`
		}
		status, err := a.api(callCtx, credential.AccessToken, method, path, body, &comment)
		if err != nil {
			if status >= 400 && status < 500 {
				return a.finish(r, key, record, map[string]any{"error": "GitHub refused the request", "status": status}, true)
			}
			return failed(r, true, wire.FailureTransportUnavailable), nil
		}
		output = comment
	case "list_repositories":
		var repos []struct {
			Name    string `json:"full_name"`
			Private bool   `json:"private"`
			URL     string `json:"html_url"`
		}
		status, err := a.api(callCtx, credential.AccessToken, method, path, nil, &repos)
		if err != nil {
			if status >= 400 && status < 500 {
				return a.finish(r, key, record, map[string]any{"error": "GitHub refused the request", "status": status}, true)
			}
			return failed(r, true, wire.FailureTransportUnavailable), nil
		}
		if len(repos) > 30 {
			repos = repos[:30]
		}
		output = repos
	}
	projected, err := json.Marshal(output)
	if err != nil {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	protected := []string{credential.AccessToken, credential.RefreshToken, a.cfg.ClientSecret}
	for _, old := range credential.Previous {
		protected = append(protected, old.AccessToken, old.RefreshToken)
	}
	for _, secret := range protected {
		if secret != "" && bytes.Contains(projected, []byte(secret)) {
			return a.finish(r, key, record, map[string]any{"error": "provider response contained protected data"}, true)
		}
	}
	return a.finish(r, key, record, output, false)
}
func (a *Adapter) finish(r wire.Request, key string, record callRecord, output any, isError bool) (wire.Reply, error) {
	raw, err := json.Marshal(output)
	if err != nil {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	if len(raw) > maxOutput {
		raw = json.RawMessage(`{"error":"connector output exceeded the limit"}`)
		isError = true
	}
	result := callResult{Operation: r.Operation, Output: raw, Digest: digest(raw), IsError: isError, CompletedAt: time.Now().UTC()}
	record.State = "completed"
	record.Result = &result
	if err := a.cfg.Vault.save(key, record); err != nil {
		return failed(r, true, wire.FailureProviderFailed), nil
	}
	return accepted(r, result), nil
}
func (a *Adapter) api(ctx context.Context, token, method, path string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIBase+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("cannot prepare GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "dac-plane-github-connector")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return 0, errors.New("GitHub request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, errors.New("GitHub refused request")
	}
	limited := io.LimitReader(resp.Body, 1<<20)
	if out != nil && json.NewDecoder(limited).Decode(out) != nil {
		return resp.StatusCode, errors.New("invalid GitHub response")
	}
	return resp.StatusCode, nil
}
