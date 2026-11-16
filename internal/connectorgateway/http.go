package connectorgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dac-plane/dac-plane/internal/application"
	"github.com/dac-plane/dac-plane/internal/contracts"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
	store "github.com/dac-plane/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Path = "/connectors/mcp"
const ProtocolVersion = "2025-06-18"
const ApprovalWait = 2 * time.Minute

type Gateway struct {
	pool       *pgxpool.Pool
	ports      application.ConnectorPorts
	tokens     *Tokens
	wake       func()
	logger     *slog.Logger
	slots      chan struct{}
	rateMu     sync.Mutex
	rateSecond int64
	rateCount  int
}

func New(pool *pgxpool.Pool, ports application.ConnectorPorts, tokens *Tokens, wake func(), logger *slog.Logger) (*Gateway, error) {
	if pool == nil || ports == nil || tokens == nil || logger == nil {
		return nil, errors.New("gateway dependencies are required")
	}
	return &Gateway{pool: pool, ports: ports, tokens: tokens, wake: wake, logger: logger, slots: make(chan struct{}, 64)}, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), ApprovalWait+5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	// Runtimes are headless clients. No browser origin is trusted on this
	// credentialed endpoint; rejecting all Origins prevents DNS rebinding.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "origin is not allowed", 403)
		return
	}
	if !g.allowRequest() {
		http.Error(w, "gateway request rate exceeded", 429)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "gateway token required", 401)
		return
	}
	tenant, attempt, err := g.tokens.Verify(strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		http.Error(w, "invalid gateway token", 401)
		return
	}
	if _, err = application.ConnectorAttempt(r.Context(), g.pool, tenant, attempt); err != nil {
		http.Error(w, "attempt cannot use connectors", 403)
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && v != ProtocolVersion {
		http.Error(w, "unsupported protocol version", 400)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "JSON required", 415)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "application/json") || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "accept JSON and event-stream", 406)
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		http.Error(w, "gateway is busy", 429)
		return
	}
	var req rpcRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if decoder.Decode(&req) != nil {
		g.respond(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "invalid JSON"}})
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || req.JSONRPC != "2.0" || req.Method == "" {
		g.respond(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request"}})
		return
	}
	if len(req.ID) == 0 {
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(202)
			return
		}
		g.respond(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "request id required"}})
		return
	}
	var id any
	if json.Unmarshal(req.ID, &id) != nil {
		g.respond(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request id"}})
		return
	}
	switch id.(type) {
	case string, float64:
	default:
		g.respond(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request id"}})
		return
	}
	res := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
			res.Error = &rpcError{-32602, "protocolVersion required"}
			break
		}
		res.Result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "dac-plane-connectors", "version": "1"}}
	case "ping":
		res.Result = struct{}{}
	case "tools/list":
		catalogue, err := application.ConnectorTools(r.Context(), g.pool, g.ports, tenant, attempt)
		if err != nil {
			res.Error = &rpcError{-32603, "connector catalogue unavailable"}
			break
		}
		listed := []any{}
		for _, source := range catalogue.Unavailable {
			g.logger.WarnContext(r.Context(), "connector catalogue unavailable", "tenant", tenant.String(), "attempt", attempt.String(), "connection", source.Connection.String(), "provider", source.Provider.String())
		}
		for _, t := range catalogue.Tools {
			listed = append(listed, map[string]any{"name": t.Connection.String() + "__" + t.Tool.Name, "description": t.Tool.Description, "inputSchema": t.Tool.InputSchema, "annotations": map[string]any{"readOnlyHint": t.Tool.ReadOnly}})
		}
		res.Result = map[string]any{"tools": listed}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if json.Unmarshal(req.Params, &params) != nil {
			res.Error = &rpcError{-32602, "invalid tool call"}
			break
		}
		parts := strings.SplitN(params.Name, "__", 2)
		if len(parts) != 2 {
			res.Error = &rpcError{-32602, "invalid tool name"}
			break
		}
		connection, err := ids.Parse[ids.Connection](parts[0])
		if err != nil {
			res.Error = &rpcError{-32602, "invalid connection"}
			break
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		// Progress notifications use the same response stream as the final result.
		var stream bool
		progress := func() {
			if len(params.Meta.ProgressToken) == 0 {
				return
			}
			if !stream {
				w.Header().Set("Content-Type", "text/event-stream")
				stream = true
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": params.Meta.ProgressToken, "progress": 0, "message": "Waiting for connector authorization or result"}})
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
			_ = http.NewResponseController(w).Flush()
		}
		progress()
		result := g.call(r.Context(), tenant, attempt, connection, parts[1], params.Arguments, progress)
		res.Result = result
		if stream {
			raw, _ := json.Marshal(res)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
			return
		}
	default:
		res.Error = &rpcError{-32601, "method not found"}
	}
	g.respond(w, res)
}
func (g *Gateway) respond(w http.ResponseWriter, res rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
func toolError(text string) any {
	return map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": text}}}
}
func (g *Gateway) call(ctx context.Context, tenant ids.TenantID, attempt ids.AttemptID, connection ids.ConnectionID, tool string, input json.RawMessage, progress func()) any {
	ctx, cancel := context.WithTimeout(ctx, ApprovalWait)
	defer cancel()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	notify := time.NewTicker(10 * time.Second)
	defer notify.Stop()
	var operation ids.OperationID
	authorized := false
	for {
		a, err := application.ConnectorAttempt(ctx, g.pool, tenant, attempt)
		if err != nil {
			return toolError("Attempt can no longer use connectors")
		}
		// A runtime can call before the worker has applied its start report;
		// the call waits for the attempt to execute, and CallConnector keeps
		// refusing a starting attempt.
		if !authorized && a.State == lifecycle.AttemptStarting {
			select {
			case <-ctx.Done():
				return toolError("The task has not started; retry the same tool and arguments")
			case <-poll.C:
				continue
			}
		}
		if !authorized {
			var call store.ConnectorCall
			var err error
			if operation.IsZero() {
				call, err = application.CallConnector(ctx, g.pool, tenant, attempt, connection, tool, input)
			} else {
				call, err = application.ResumeConnectorCall(ctx, g.pool, tenant, attempt, operation, input)
			}
			if err == nil || errors.Is(err, application.ErrConnectorApprovalPending) {
				operation = call.Operation
			}
			if err == nil {
				authorized = true
				if g.wake != nil {
					g.wake()
				}
			} else if !errors.Is(err, application.ErrConnectorApprovalPending) {
				return toolError("Connector call refused")
			}
		}
		if authorized {
			call, err := application.ConnectorCallResult(ctx, g.pool, tenant, operation)
			if err != nil {
				return toolError("Connector outcome unavailable or uncertain; the call was not repeated")
			}
			if call.Result != nil {
				return map[string]any{"isError": call.Result.IsError, "content": []any{map[string]string{"type": "text", "text": string(call.Result.Output)}}}
			}
		}
		select {
		case <-ctx.Done():
			return toolError("Approval or result pending; retry the same tool and arguments after checking the task")
		case <-poll.C:
		case <-notify.C:
			progress()
		}
	}
}

// Access constructs the runtime-only reference; never persist it in an outbox,
// audit, event, task brief or provider error message.
func (t *Tokens) Access(url string, tenant ids.TenantID, attempt ids.AttemptID) *contracts.GatewayAccess {
	return &contracts.GatewayAccess{URL: url, Token: t.Issue(tenant, attempt)}
}

func (g *Gateway) allowRequest() bool {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	now := time.Now().Unix()
	if g.rateSecond != now {
		g.rateSecond = now
		g.rateCount = 0
	}
	if g.rateCount >= 100 {
		return false
	}
	g.rateCount++
	return true
}
