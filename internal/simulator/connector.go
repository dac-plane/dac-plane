package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

// Connector holds a canary credential locally and returns safe account and
// tool projections only. Call effects and results are operation-idempotent.
type Connector struct {
	config      Config
	ledger      *Ledger
	deliveries  *deliveries
	mu          sync.Mutex
	credentials map[ids.ConnectionID]string
	results     map[ids.OperationID]contracts.ConnectorResult
	active      map[ids.OperationID]context.CancelFunc
	cancelled   map[ids.OperationID]bool
	proofs      map[ids.ConnectionID]string
	markers     map[ids.ConnectionID]string
}

func NewConnector(config Config) (*Connector, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Connector{config: config, ledger: newLedger(), deliveries: newDeliveries(), credentials: map[ids.ConnectionID]string{}, proofs: map[ids.ConnectionID]string{}, markers: map[ids.ConnectionID]string{}, results: map[ids.OperationID]contracts.ConnectorResult{}, active: map[ids.OperationID]context.CancelFunc{}, cancelled: map[ids.OperationID]bool{}}, nil
}
func (c *Connector) Role() contracts.Role       { return contracts.RoleConnector }
func (c *Connector) Hello() contracts.Lifecycle { return c.config.Hello }
func (c *Connector) Ledger() *Ledger            { return c.ledger }
func (c *Connector) Control() *Control          { return c.config.Control }

// Consent simulates consent at the external application. The credential never
// enters a protocol value, task input or output.
func (c *Connector) Consent(id ids.ConnectionID, credential string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentials[id] = credential
	proof := strings.TrimPrefix(string(identity.DigestOf([]byte("simulator-proof:"+id.String()))), "sha256:")
	c.proofs[id] = proof
	c.markers[id] = strings.TrimPrefix(string(identity.DigestOf([]byte("simulator-marker:"+id.String()+":"+credential))), "sha256:")[:12]
	return proof
}

// Marker is the value a connection's get_issue answers carry in their title.
// It is derived from the simulated credential, so a test can tell an answer
// the connector gave from one a model could have guessed from its prompt.
func (c *Connector) Marker(id ids.ConnectionID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.markers[id]
}
func (c *Connector) Send(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
	if err := checkRequest(c.config, c.Role(), r); err != nil {
		return contracts.Reply{}, err
	}
	c.ledger.recordRequest(r)
	var value any
	switch r.Method {
	case contracts.MethodListTools:
		value = contracts.ToolCatalogue{Tools: []contracts.ConnectorTool{
			{Name: "get_issue", Description: "Read one issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"}},"required":["number"],"additionalProperties":false}`), ReadOnly: true},
			{Name: "create_comment", Description: "Comment on an issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"},"body":{"type":"string"}},"required":["number","body"],"additionalProperties":false}`)},
			{Name: "list_repositories", Description: "List accessible repositories", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), ReadOnly: true},
		}}
	case contracts.MethodBeginAuthorization:
		var arg contracts.BeginAuthorization
		if err := json.Unmarshal(r.Arguments, &arg); err != nil || arg.Validate() != nil {
			return contracts.Reply{}, ErrMalformedRequest
		}
		value = contracts.AuthorizationStarted{URL: "https://connector.example/consent?connection=" + arg.Connection.String(), ExpiresAt: arg.ExpiresAt}
	case contracts.MethodCompleteAuthorization, contracts.MethodRevokeConnection:
		var arg contracts.ConnectionReference
		if r.Method == contracts.MethodCompleteAuthorization {
			var completed contracts.CompleteAuthorization
			if err := json.Unmarshal(r.Arguments, &completed); err != nil || completed.Validate() != nil {
				return contracts.Reply{}, ErrMalformedRequest
			}
			arg.Connection = completed.Connection
			c.mu.Lock()
			proof := c.proofs[arg.Connection]
			c.mu.Unlock()
			if proof != completed.Confirmation {
				return Accepted([]byte(`{"status":"pending"}`)).stamp(r.Operation, r.Delivery)
			}
		} else if err := json.Unmarshal(r.Arguments, &arg); err != nil || arg.Validate() != nil {
			return contracts.Reply{}, ErrMalformedRequest
		}
		c.mu.Lock()
		credential := c.credentials[arg.Connection]
		if r.Method == contracts.MethodRevokeConnection {
			delete(c.credentials, arg.Connection)
		}
		c.mu.Unlock()
		if r.Method == contracts.MethodRevokeConnection {
			value = struct{}{}
		} else if credential == "" {
			value = contracts.AuthorizationResult{Status: contracts.AuthorizationPending}
		} else {
			value = contracts.AuthorizationResult{Status: contracts.AuthorizationAuthorized, Account: &lifecycle.ConnectionAccount{Account: "simulator-account", Permissions: []string{"issues:write"}}}
		}
	case contracts.MethodReadCall:
		c.mu.Lock()
		result, ok := c.results[r.Operation]
		c.mu.Unlock()
		if !ok {
			return Unknown(&contracts.Failure{Code: contracts.FailureUnknownOperation, Message: "call outcome is unknown"}).stamp(r.Operation, r.Delivery)
		}
		value = result
	case contracts.MethodCallTool:
		var e contracts.ConnectorExecution
		if err := json.Unmarshal(r.Arguments, &e); err != nil || e.Validate() != nil || e.Request.Operation != r.Operation || e.Request.Attempt != r.Attempt || e.Request.Tenant != r.Tenant || e.Request.Provider != c.config.Provider {
			return contracts.Reply{}, ErrMalformedRequest
		}
		recorded, done, release, err := c.deliveries.claim(ctx, r)
		if err != nil {
			return contracts.Reply{}, err
		}
		if done {
			return recorded, nil
		}
		defer release()
		c.mu.Lock()
		credential := c.credentials[e.Connection]
		c.mu.Unlock()
		if credential == "" {
			return Rejected(&contracts.Failure{Code: contracts.FailureUnauthorized, Message: "connection is not authorized"}).stamp(r.Operation, r.Delivery)
		}
		callCtx, stop := context.WithCancel(ctx)
		c.mu.Lock()
		c.active[r.Operation] = stop
		if c.cancelled[r.Operation] {
			stop()
		}
		c.mu.Unlock()
		defer func() { stop(); c.mu.Lock(); delete(c.active, r.Operation); c.mu.Unlock() }()
		ctx = callCtx
		if err := c.config.Control.reach(ctx, r.Operation, BeforeEffect); err != nil {
			return c.cancelledReply(r)
		}
		if ctx.Err() != nil {
			return c.cancelledReply(r)
		}
		var input map[string]any
		if err := json.Unmarshal(e.Input, &input); err != nil {
			return contracts.Reply{}, err
		}
		switch e.Tool {
		case "get_issue", "create_comment", "list_repositories":
		default:
			return Rejected(&contracts.Failure{Code: contracts.FailureUnknownOperation, Message: "unknown connector tool"}).stamp(r.Operation, r.Delivery)
		}
		// Return fixed evidence, rather than echoing arbitrary input into
		// output. A read of an issue names only its validated number, so a
		// live model's report can be checked against what the call returned.
		output := json.RawMessage(`{"ok":true}`)
		if number, ok := input["number"].(float64); e.Tool == "get_issue" && ok && number == float64(int64(number)) && number > 0 && number < 1e9 {
			c.mu.Lock()
			marker := c.markers[e.Connection]
			c.mu.Unlock()
			output = json.RawMessage(fmt.Sprintf(`{"number":%d,"title":"Simulated issue %d %s","state":"open"}`, int64(number), int64(number), marker))
		}
		result := contracts.ConnectorResult{Operation: r.Operation, Output: output, OutputDigest: identity.DigestOf(output), CompletedAt: time.Now().UTC()}
		c.mu.Lock()
		c.results[r.Operation] = result
		c.mu.Unlock()
		raw, _ := json.Marshal(result)
		reply, err := Accepted(raw).stamp(r.Operation, r.Delivery)
		if err != nil {
			return reply, err
		}
		c.ledger.recordEffect(Effect{Role: c.Role(), Operation: r.Operation, Method: r.Method, Kind: EffectKind("connector_called")})
		c.deliveries.finish(r.Operation, r.Method, reply)
		if err := c.config.Control.reach(ctx, r.Operation, AfterEffect); err != nil {
			return contracts.Reply{}, err
		}
		return reply, nil
	default:
		return contracts.Reply{}, fmt.Errorf("%w: connector method", ErrMalformedRequest)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return contracts.Reply{}, err
	}
	return Accepted(raw).stamp(r.Operation, r.Delivery)
}
func (c *Connector) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := recordCancel(c.ledger, cancel); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled[cancel.Operation] = true
	if stop := c.active[cancel.Operation]; stop != nil {
		stop()
	}
	return nil
}
func (c *Connector) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	return subscribeInert(c.config, c.ledger, true, cursor)
}

func (c *Connector) cancelledReply(r contracts.Request) (contracts.Reply, error) {
	output := json.RawMessage(`{"cancelled":true,"external_request_sent":false}`)
	result := contracts.ConnectorResult{Operation: r.Operation, Output: output, OutputDigest: identity.DigestOf(output), IsError: true, CompletedAt: time.Now().UTC()}
	c.mu.Lock()
	c.results[r.Operation] = result
	c.mu.Unlock()
	raw, _ := json.Marshal(result)
	reply, err := Accepted(raw).stamp(r.Operation, r.Delivery)
	if err == nil {
		c.deliveries.finish(r.Operation, r.Method, reply)
	}
	return reply, err
}
