package e2badapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/e2b/journal"
)

const (
	metadataOperation = "dac_operation_id"
	metadataTenant    = "dac_tenant_id"
	metadataAttempt   = "dac_attempt_id"
)

type Config struct {
	BaseURL, APIKey, TemplateID, ProviderID string
	HTTPClient                              *http.Client
	Journal                                 *journal.Store
	Now                                     func() time.Time
}

type Adapter struct {
	cfg Config
	mu  sync.Mutex
}

type apiError struct{ status int }

func (e apiError) Error() string { return fmt.Sprintf("E2B API returned HTTP %d", e.status) }

type migrationRequest struct {
	Migration string   `json:"migration_id"`
	Operation string   `json:"operation_id"`
	Source    string   `json:"source_attempt_id"`
	Target    string   `json:"target_attempt_id"`
	Stage     string   `json:"stage"`
	Epoch     uint64   `json:"claim_epoch"`
	Recovery  string   `json:"recovery,omitempty"`
	Context   []string `json:"context_refs,omitempty"`
	EnvRef    string   `json:"environment_ref,omitempty"`
}

type sandbox struct {
	SandboxID   string            `json:"sandboxID"`
	SandboxId   string            `json:"sandboxId,omitempty"`
	Template    string            `json:"templateID"`
	State       string            `json:"state"`
	EnvdVersion string            `json:"envdVersion"`
	Metadata    map[string]string `json:"metadata"`
	Network     struct {
		DenyOut []string `json:"denyOut"`
	} `json:"network"`
	AllowInternetAccess *bool `json:"allowInternetAccess"`
}

type migrationReceipt struct {
	Migration string    `json:"migration_id"`
	Operation string    `json:"operation_id"`
	Source    string    `json:"source_attempt_id"`
	Target    string    `json:"target_attempt_id,omitzero"`
	Stage     string    `json:"stage"`
	Epoch     uint64    `json:"claim_epoch"`
	Recovery  string    `json:"recovery,omitempty"`
	Context   []string  `json:"context_refs,omitempty"`
	EnvRef    string    `json:"environment_ref,omitempty"`
	At        time.Time `json:"recorded_at"`
}

func New(cfg Config) (*Adapter, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("E2B base URL must be an http(s) origin without user info or query")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("plain HTTP E2B API URLs are allowed only on loopback")
		}
	}
	if cfg.APIKey == "" || cfg.TemplateID == "" || cfg.ProviderID == "" || cfg.Journal == nil {
		return nil, errors.New("E2B adapter configuration is incomplete")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	}
	client := *cfg.HTTPClient
	if client.CheckRedirect == nil {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	cfg.HTTPClient = &client
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Adapter{cfg: cfg}, nil
}

func (a *Adapter) Send(ctx context.Context, req wire.Request) (wire.Reply, error) {
	if req.Role != wire.RoleEnvironment {
		return rejected(req, wire.FailureUnauthorized, "wrong adapter role"), nil
	}
	switch req.Method {
	case "provision":
		return a.provision(ctx, req)
	case "stop":
		return a.stop(ctx, req)
	case "observe":
		return a.observe(ctx, req)
	default:
		return rejected(req, wire.FailureUnknownOperation, "unsupported environment method"), nil
	}
}

func (a *Adapter) Cancel(ctx context.Context, cancel wire.Cancel) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	op, err := a.cfg.Journal.Operation(cancel.Operation)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.cfg.Journal.CancelBeforeStart(cancel.Operation)
		}
		return err
	}
	if op.Method != "provision" {
		return nil
	}
	if op.ResourceRef == "" && op.State == "prepared" {
		return a.cfg.Journal.Update(cancel.Operation, func(current *journal.Operation) error {
			current.State = "cancelled"
			return nil
		})
	}
	ref := op.ResourceRef
	if ref == "" && op.State == "create_sent" {
		sbx, findErr := a.findOperation(ctx, cancel.Operation)
		if findErr != nil {
			return findErr
		}
		if sbx == nil {
			return errors.New("E2B create outcome remains unknown during cancellation")
		}
		ref = "sandbox:" + sbx.SandboxID
	}
	id, ok := sandboxID(ref)
	if !ok {
		return nil
	}
	err = a.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, "", nil)
	var status apiError
	if err != nil && (!errors.As(err, &status) || status.status != http.StatusNotFound) {
		return err
	}
	return a.cfg.Journal.Update(cancel.Operation, func(current *journal.Operation) error {
		current.State = "cancelled"
		current.ResourceRef = ref
		return nil
	})
}

func (a *Adapter) Subscribe(_ context.Context, cursor wire.Cursor) (adapterkit.Stream, error) {
	return &observationStream{store: a.cfg.Journal, operation: cursor.Operation, after: cursor.After, connected: true}, nil
}

func (a *Adapter) provision(ctx context.Context, req wire.Request) (wire.Reply, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(req.Arguments, &fields); err != nil {
		return rejected(req, wire.FailureInvalidArguments, "invalid environment provision request"), nil
	}
	if len(fields) == 0 {
		return a.provisionTask(ctx, req)
	}
	var m migrationRequest
	if err := strictDecode(req.Arguments, &m); err != nil || m.Operation != req.Operation || m.Stage != "target_environment_ready" || m.Migration == "" || m.Source == "" || m.Target == "" || m.Target != req.Attempt || m.Source == m.Target || m.Epoch == 0 || m.Recovery != "restart_from_artifacts" {
		return rejected(req, wire.FailureInvalidArguments, "invalid migration environment provision request"), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cancelled, err := a.cfg.Journal.WasCancelledBeforeStart(req.Operation)
	if err != nil {
		return wire.Reply{}, err
	}
	if cancelled {
		return rejected(req, wire.FailureCancelled, "provision was cancelled before it started"), nil
	}
	hash := digest(req.Arguments)
	op, created, err := a.cfg.Journal.Begin(journal.Operation{ID: req.Operation, Hash: hash, Tenant: req.Tenant, Attempt: req.Attempt, Method: req.Method})
	if err != nil {
		return rejected(req, wire.FailureInvalidArguments, "operation identity or payload mismatch"), nil
	}
	if op.State == "cancelled" {
		return rejected(req, wire.FailureCancelled, "provision was cancelled before it started"), nil
	}
	if op.ResourceRef != "" && len(op.Receipt) != 0 {
		return accepted(req, op.Receipt), nil
	}
	if !created && op.State != "prepared" {
		// A sent create can have succeeded even if the response was lost. Look it
		// up by immutable metadata; never issue a second create on uncertainty.
		found, lookupErr := a.findOperation(ctx, req.Operation)
		if lookupErr != nil {
			return wire.Reply{}, lookupErr
		}
		if found != nil {
			return a.finishProvision(req, m, *found)
		}
		return wire.Reply{}, errors.New("E2B create outcome remains unknown; no retry was issued")
	}
	if !created {
		found, lookupErr := a.findOperation(ctx, req.Operation)
		if lookupErr != nil {
			return wire.Reply{}, lookupErr
		}
		if found != nil {
			return a.finishProvision(req, m, *found)
		}
	}
	if err := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error { o.State = "create_sent"; return nil }); err != nil {
		return wire.Reply{}, err
	}
	allowInternet := false
	body := map[string]any{
		"templateID":            a.cfg.TemplateID,
		"allow_internet_access": allowInternet,
		"network": map[string]any{
			"denyOut": []string{"0.0.0.0/0"},
		},
		"metadata": map[string]string{metadataOperation: req.Operation, metadataTenant: req.Tenant, metadataAttempt: req.Attempt},
	}
	var createdSandbox sandbox
	if err := a.doJSON(ctx, http.MethodPost, "/v2/sandboxes", body, "", &createdSandbox); err != nil {
		var status apiError
		if errors.As(err, &status) && status.status >= 400 && status.status < 500 && status.status != http.StatusRequestTimeout && status.status != http.StatusConflict && status.status != http.StatusTooManyRequests {
			if resetErr := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error { o.State = "prepared"; return nil }); resetErr != nil {
				return wire.Reply{}, resetErr
			}
			return wire.Reply{}, fmt.Errorf("E2B rejected sandbox create: %w", err)
		}
		found, lookupErr := a.findOperation(ctx, req.Operation)
		if lookupErr == nil && found != nil {
			return a.finishProvision(req, m, *found)
		}
		return wire.Reply{}, fmt.Errorf("E2B create reply is uncertain: %w", err)
	}
	if createdSandbox.SandboxID == "" {
		createdSandbox.SandboxID = createdSandbox.SandboxId
	}
	if createdSandbox.SandboxID == "" {
		return wire.Reply{}, errors.New("E2B create response omitted sandbox identity")
	}
	return a.finishProvision(req, m, createdSandbox)
}

type taskProvisionResult struct {
	ResourceRef string `json:"resource_ref"`
}

func (a *Adapter) provisionTask(ctx context.Context, req wire.Request) (wire.Reply, error) {
	var empty struct{}
	if err := strictDecode(req.Arguments, &empty); err != nil {
		return rejected(req, wire.FailureInvalidArguments, "invalid environment provision request"), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cancelled, err := a.cfg.Journal.WasCancelledBeforeStart(req.Operation)
	if err != nil {
		return wire.Reply{}, err
	}
	if cancelled {
		return rejected(req, wire.FailureCancelled, "provision was cancelled before it started"), nil
	}
	op, created, err := a.cfg.Journal.Begin(journal.Operation{ID: req.Operation, Hash: digest(req.Arguments), Tenant: req.Tenant, Attempt: req.Attempt, Method: req.Method})
	if err != nil {
		return rejected(req, wire.FailureInvalidArguments, "operation identity or payload mismatch"), nil
	}
	if op.ResourceRef != "" && op.State == "provisioned" {
		// A crash after saving the mapping left its readiness unrecorded.
		return a.recordTaskReadiness(req, op.ResourceRef)
	}
	if op.ResourceRef != "" && op.State != "create_sent" {
		raw, _ := json.Marshal(taskProvisionResult{ResourceRef: op.ResourceRef})
		return accepted(req, raw), nil
	}
	if !created && op.State != "prepared" {
		found, lookupErr := a.findOperation(ctx, req.Operation)
		if lookupErr != nil {
			return wire.Reply{}, lookupErr
		}
		if found == nil {
			return wire.Reply{}, errors.New("E2B create outcome remains unknown; no retry was issued")
		}
		return a.finishTaskProvision(req, *found)
	}
	if err := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error { o.State = "create_sent"; return nil }); err != nil {
		return wire.Reply{}, err
	}
	createdSandbox, err := a.createSandbox(ctx, req)
	if err != nil {
		found, lookupErr := a.findOperation(ctx, req.Operation)
		if lookupErr == nil && found != nil {
			return a.finishTaskProvision(req, *found)
		}
		return wire.Reply{}, fmt.Errorf("E2B create reply is uncertain: %w", err)
	}
	return a.finishTaskProvision(req, createdSandbox)
}

func (a *Adapter) createSandbox(ctx context.Context, req wire.Request) (sandbox, error) {
	body := map[string]any{"templateID": a.cfg.TemplateID, "allow_internet_access": false,
		"network":  map[string]any{"denyOut": []string{"0.0.0.0/0"}},
		"metadata": map[string]string{metadataOperation: req.Operation, metadataTenant: req.Tenant, metadataAttempt: req.Attempt}}
	var created sandbox
	if err := a.doJSON(ctx, http.MethodPost, "/v2/sandboxes", body, "", &created); err != nil {
		return sandbox{}, err
	}
	if created.SandboxID == "" {
		created.SandboxID = created.SandboxId
	}
	if created.SandboxID == "" {
		return sandbox{}, errors.New("E2B create response omitted sandbox identity")
	}
	return created, nil
}

func (a *Adapter) finishTaskProvision(req wire.Request, sbx sandbox) (wire.Reply, error) {
	ref := "sandbox:" + sbx.SandboxID
	if err := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error { o.ResourceRef, o.State = ref, "provisioned"; return nil }); err != nil {
		return wire.Reply{}, err
	}
	return a.recordTaskReadiness(req, ref)
}

// recordTaskReadiness records the provisioned sandbox's running observation
// and accepts the provision. The journal records it once, so a redelivery
// after a crash between the mapping and this write repairs it.
func (a *Adapter) recordTaskReadiness(req wire.Request, ref string) (wire.Reply, error) {
	_, _, err := a.cfg.Journal.RecordObservation(req.Operation, ref, func(seq uint64) (json.RawMessage, error) {
		return json.Marshal(observation{ResourceRef: ref, State: "running", Sequence: seq, ObservedAt: a.cfg.Now()})
	})
	if err != nil {
		return wire.Reply{}, err
	}
	raw, err := json.Marshal(taskProvisionResult{ResourceRef: ref})
	if err != nil {
		return wire.Reply{}, err
	}
	return accepted(req, raw), nil
}

func (a *Adapter) finishProvision(req wire.Request, m migrationRequest, sbx sandbox) (wire.Reply, error) {
	ref := "sandbox:" + sbx.SandboxID
	receipt := migrationReceipt{Migration: m.Migration, Operation: m.Operation, Source: m.Source, Target: m.Target, Stage: m.Stage, Epoch: m.Epoch, Recovery: m.Recovery, Context: m.Context, EnvRef: ref, At: a.cfg.Now()}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return wire.Reply{}, err
	}
	if err := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error {
		o.ResourceRef, o.State, o.Receipt = ref, "provisioned", raw
		return nil
	}); err != nil {
		return wire.Reply{}, err
	}
	return accepted(req, raw), nil
}

func (a *Adapter) stop(ctx context.Context, req wire.Request) (wire.Reply, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(req.Arguments, &fields); err != nil {
		return rejected(req, wire.FailureInvalidArguments, "invalid environment stop request"), nil
	}
	if _, taskEnvironmentStop := fields["provision_operation_id"]; taskEnvironmentStop {
		return a.stopTaskEnvironment(ctx, req)
	}
	var m migrationRequest
	if err := strictDecode(req.Arguments, &m); err != nil || m.Operation != req.Operation || m.Migration == "" || m.Source == "" || m.Epoch == 0 || m.EnvRef == "" || (m.Stage != "source_cleaned" && m.Stage != "target_environment_cleaned") || (m.Target != "" && m.Target == m.Source) || (m.Stage == "target_environment_cleaned" && m.Target == "") {
		return rejected(req, wire.FailureInvalidArguments, "invalid migration environment stop request"), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	hash := digest(req.Arguments)
	op, _, err := a.cfg.Journal.Begin(journal.Operation{ID: req.Operation, Hash: hash, Tenant: req.Tenant, Attempt: req.Attempt, Method: req.Method})
	if err != nil {
		return rejected(req, wire.FailureInvalidArguments, "operation identity or payload mismatch"), nil
	}
	if op.State == "stopped" && len(op.Receipt) != 0 {
		return accepted(req, op.Receipt), nil
	}
	id, ok := sandboxID(m.EnvRef)
	if !ok {
		return rejected(req, wire.FailureInvalidArguments, "invalid E2B environment reference"), nil
	}
	if err := a.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, "", nil); err != nil {
		var deleteStatus apiError
		if !errors.As(err, &deleteStatus) || deleteStatus.status != http.StatusNotFound {
			// If the reply was lost or the sandbox was already removed, verify the
			// resource before deciding whether cleanup converged.
			var current sandbox
			getErr := a.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, "", &current)
			var getStatus apiError
			if getErr == nil || !errors.As(getErr, &getStatus) || getStatus.status != http.StatusNotFound {
				return wire.Reply{}, fmt.Errorf("E2B stop outcome is uncertain: %w", err)
			}
		}
	}
	receipt := migrationReceipt{Migration: m.Migration, Operation: m.Operation, Source: m.Source, Target: m.Target, Stage: m.Stage, Epoch: m.Epoch, Context: m.Context, EnvRef: m.EnvRef, At: a.cfg.Now()}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return wire.Reply{}, err
	}
	if err := a.cfg.Journal.Update(req.Operation, func(o *journal.Operation) error {
		o.ResourceRef, o.State, o.Receipt = m.EnvRef, "stopped", raw
		return nil
	}); err != nil {
		return wire.Reply{}, err
	}
	return accepted(req, raw), nil
}

func (a *Adapter) stopTaskEnvironment(ctx context.Context, req wire.Request) (wire.Reply, error) {
	var input struct {
		ProvisionOperation string `json:"provision_operation_id"`
	}
	if err := strictDecode(req.Arguments, &input); err != nil || input.ProvisionOperation == "" {
		return rejected(req, wire.FailureInvalidArguments, "invalid environment stop request"), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	provision, err := a.cfg.Journal.Operation(input.ProvisionOperation)
	resourceRef := "operation:" + input.ProvisionOperation
	if errors.Is(err, os.ErrNotExist) {
		if err := a.cfg.Journal.CancelBeforeStart(input.ProvisionOperation); err != nil {
			return wire.Reply{}, err
		}
	} else if err != nil {
		return wire.Reply{}, err
	} else {
		if provision.Tenant != req.Tenant || provision.Attempt != req.Attempt {
			return rejected(req, wire.FailureInvalidArguments, "environment assignment is unavailable"), nil
		}
		if provision.ResourceRef == "" && provision.State == "prepared" {
			if err := a.cfg.Journal.CancelBeforeStart(input.ProvisionOperation); err != nil {
				return wire.Reply{}, err
			}
		} else if provision.ResourceRef == "" {
			found, findErr := a.findOperation(ctx, input.ProvisionOperation)
			if findErr != nil {
				return wire.Reply{}, findErr
			}
			if found == nil {
				return wire.Reply{}, errors.New("E2B create outcome remains unknown during stop")
			}
			provision.ResourceRef = "sandbox:" + found.SandboxID
		}
		if provision.ResourceRef != "" {
			resourceRef = provision.ResourceRef
		}
	}
	op, _, err := a.cfg.Journal.Begin(journal.Operation{ID: req.Operation, Hash: digest(req.Arguments), Tenant: req.Tenant, Attempt: req.Attempt, Method: req.Method, ResourceRef: resourceRef})
	if err != nil {
		return rejected(req, wire.FailureInvalidArguments, "operation identity or payload mismatch"), nil
	}
	if op.State == "observed" && len(op.Receipt) > 0 {
		return accepted(req, op.Receipt), nil
	}
	if id, ok := sandboxID(resourceRef); ok {
		if err := a.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, "", nil); err != nil {
			var status apiError
			if !errors.As(err, &status) || status.status != http.StatusNotFound {
				return wire.Reply{}, err
			}
		}
	}
	raw, _, err := a.cfg.Journal.RecordObservation(req.Operation, resourceRef, func(seq uint64) (json.RawMessage, error) {
		return json.Marshal(observation{ResourceRef: resourceRef, State: "absent", Sequence: seq, ObservedAt: a.cfg.Now()})
	})
	if err != nil {
		return wire.Reply{}, err
	}
	return accepted(req, raw), nil
}

type observationRequest struct {
	EnvironmentRef string `json:"environment_ref"`
}
type observation struct {
	ResourceRef string    `json:"resource_ref"`
	State       string    `json:"state"`
	Sequence    uint64    `json:"sequence"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (a *Adapter) observe(ctx context.Context, req wire.Request) (wire.Reply, error) {
	var input observationRequest
	if err := strictDecode(req.Arguments, &input); err != nil {
		return rejected(req, wire.FailureInvalidArguments, "invalid environment observation request"), nil
	}
	id, ok := sandboxID(input.EnvironmentRef)
	if !ok {
		return rejected(req, wire.FailureInvalidArguments, "invalid E2B environment reference"), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	op, _, err := a.cfg.Journal.Begin(journal.Operation{ID: req.Operation, Hash: digest(req.Arguments), Tenant: req.Tenant, Attempt: req.Attempt, Method: req.Method, ResourceRef: input.EnvironmentRef})
	if err != nil {
		return rejected(req, wire.FailureInvalidArguments, "operation identity or payload mismatch"), nil
	}
	if op.State == "observed" && len(op.Receipt) != 0 {
		return accepted(req, op.Receipt), nil
	}
	state := "unknown"
	var sbx sandbox
	if err := a.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, "", &sbx); err != nil {
		var status apiError
		if !errors.As(err, &status) || status.status != http.StatusNotFound {
			return wire.Reply{}, err
		}
		state = "absent"
	} else {
		switch strings.ToLower(sbx.State) {
		case "running":
			state = "running"
		case "paused":
			state = "hibernated"
		}
	}
	raw, _, err := a.cfg.Journal.RecordObservation(req.Operation, input.EnvironmentRef, func(seq uint64) (json.RawMessage, error) {
		return json.Marshal(observation{ResourceRef: input.EnvironmentRef, State: state, Sequence: seq, ObservedAt: a.cfg.Now()})
	})
	if err != nil {
		return wire.Reply{}, err
	}
	return accepted(req, raw), nil
}

func (a *Adapter) findOperation(ctx context.Context, operation string) (*sandbox, error) {
	query := url.Values{"metadata": []string{metadataOperation + "=" + operation}}
	var found []sandbox
	if err := a.doJSON(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, "", &found); err != nil {
		return nil, err
	}
	var result *sandbox
	for i := range found {
		if found[i].Metadata[metadataOperation] != operation {
			continue
		}
		if result != nil {
			return nil, errors.New("multiple E2B sandboxes claim the same DAC operation")
		}
		result = &found[i]
	}
	return result, nil
}

func (a *Adapter) doJSON(ctx context.Context, method, path string, body any, idemKey string, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", a.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("cannot read E2B response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError{status: resp.StatusCode}
	}
	if out != nil && len(data) != 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("E2B API returned malformed JSON")
		}
	}
	return nil
}

func (a *Adapter) Replay(ctx context.Context, req wire.Request) (wire.Reply, error) {
	return a.Send(ctx, req)
}

func strictDecode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func sandboxID(ref string) (string, bool) {
	id, ok := strings.CutPrefix(ref, "sandbox:")
	return id, ok && id != "" && !strings.ContainsAny(id, "/?#")
}
func accepted(r wire.Request, raw []byte) wire.Reply {
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeOK, Durability: wire.DurabilityAccepted, Result: raw}
}
func rejected(r wire.Request, code, message string) wire.Reply {
	return wire.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: wire.OutcomeFailed, Durability: wire.DurabilityRejected, Failure: &wire.Failure{Code: code, Message: message}}
}
