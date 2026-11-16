package codexadapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dac-plane/dac-plane/adapters/codex/appserver"
	"github.com/dac-plane/dac-plane/adapters/codex/journal"
)

// The adapter is the connector gateway's MCP client and offers the gateway's
// tools to Codex as dynamic tools: plain functions whose calls Codex sends
// back to this adapter as item/tool/call. Codex 0.165.0 sends its own MCP
// servers' tools to the model inside a Responses API "namespace" tool, which
// OpenAI-compatible providers such as Camer drop without an error, so a
// thread given the gateway as an MCP server never shows its tools to the
// model. The gateway still authorizes every call; this adapter only relays it
// with the attempt's token, which never enters a prompt or the workspace.

const (
	gatewayProtocolVersion = "2025-06-18"
	// gatewayListTimeout bounds the tool listing a thread start waits for.
	gatewayListTimeout = 15 * time.Second
	// gatewayCallTimeout covers the gateway's wait for an approval, two
	// minutes, and its result.
	gatewayCallTimeout = 150 * time.Second
	maxGatewayResponse = 1 << 20
	maxDynamicToolName = 64
)

var dynamicToolUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// gatewayTool is one tool the gateway lists: its gateway name, which names
// the connection and the tool, and what the model is told about it.
type gatewayTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// dynamicToolName is the function name the model calls for a gateway tool:
// the tool's own name and a digest of the gateway name, so two connections
// offering the same tool stay distinct and the name stays within the
// Responses API's 64 characters. It is derived, never stored, so a restarted
// adapter resolves it by listing the tools again.
func dynamicToolName(gatewayName string) string {
	tool := gatewayName
	if i := strings.LastIndex(gatewayName, "__"); i >= 0 {
		tool = gatewayName[i+2:]
	}
	sum := sha256.Sum256([]byte(gatewayName))
	suffix := "_" + hex.EncodeToString(sum[:4])
	base := "dac_" + dynamicToolUnsafe.ReplaceAllString(tool, "_")
	if len(base)+len(suffix) > maxDynamicToolName {
		base = base[:maxDynamicToolName-len(suffix)]
	}
	return base + suffix
}

// gatewayHTTP is the configured client without its overall timeout, which a
// call waiting for an approval outlives; the call's context bounds it
// instead. Its redirect policy is kept, so a request carrying the attempt
// token is never forwarded beyond the gateway.
func (a *Adapter) gatewayHTTP() *http.Client {
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if a.config.HTTPClient != nil {
		client = *a.config.HTTPClient
		if client.CheckRedirect == nil {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
	}
	client.Timeout = 0
	return &client
}

// gatewayRPC sends one JSON-RPC request to the gateway and returns its
// result. The gateway answers with JSON, or with an event stream whose last
// message is the response.
func (a *Adapter) gatewayRPC(ctx context.Context, g *journal.GatewayAccess, method string, params any, result any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", gatewayProtocolVersion)
	res, err := a.gatewayHTTP().Do(req)
	if err != nil {
		// The transport error can name the URL, never the token.
		return fmt.Errorf("connector gateway %s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("connector gateway %s: HTTP %d", method, res.StatusCode)
	}
	raw, err := gatewayMessage(res)
	if err != nil {
		return fmt.Errorf("connector gateway %s: %w", method, err)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("connector gateway %s: malformed response", method)
	}
	if response.Error != nil {
		return fmt.Errorf("connector gateway %s: error %d", method, response.Error.Code)
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("connector gateway %s: malformed result", method)
	}
	return nil
}

func gatewayMessage(res *http.Response) ([]byte, error) {
	limited := io.LimitReader(res.Body, maxGatewayResponse)
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return io.ReadAll(limited)
	}
	var last []byte
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxGatewayResponse)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			var probe struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(data)), &probe) == nil && len(probe.ID) > 0 {
				last = []byte(strings.TrimSpace(data))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if last == nil {
		return nil, errors.New("event stream carried no response")
	}
	return last, nil
}

func (a *Adapter) gatewayTools(ctx context.Context, g *journal.GatewayAccess) ([]gatewayTool, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayListTimeout)
	defer cancel()
	var listed struct {
		Tools []gatewayTool `json:"tools"`
	}
	if err := a.gatewayRPC(ctx, g, "tools/list", map[string]any{}, &listed); err != nil {
		return nil, err
	}
	return listed.Tools, nil
}

// dynamicTools lists the attempt's gateway tools as Codex dynamic tool specs.
// A gateway that cannot be reached leaves the thread without connector tools
// rather than failing the start: nothing it holds back is unsafe to omit.
func (a *Adapter) dynamicTools(ctx context.Context, g *journal.GatewayAccess) []map[string]any {
	tools, err := a.gatewayTools(ctx, g)
	if err != nil {
		a.logger().Warn("connector gateway tools unavailable; the thread starts without them", "error", err)
		return nil
	}
	specs := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		specs = append(specs, map[string]any{"type": "function", "name": dynamicToolName(tool.Name), "description": tool.Description, "inputSchema": schema})
	}
	return specs
}

// dynamicToolCall relays one item/tool/call to the gateway and answers Codex
// with the tool's text. A refusal, an approval that did not come in time or
// an unreachable gateway is a failed call the model can read, never a silence
// Codex would wait on.
func (a *Adapter) dynamicToolCall(m appserver.Message) {
	var params struct {
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	answer := func(text string, success bool) {
		if err := a.config.Native.Reply(m.ID, map[string]any{"contentItems": []map[string]string{{"type": "inputText", "text": text}}, "success": success}); err != nil {
			a.logger().Warn("dynamic tool reply failed", "error", err)
		}
	}
	if json.Unmarshal(m.Params, &params) != nil || params.ThreadID == "" || params.Tool == "" {
		answer("The tool call was malformed.", false)
		return
	}
	op, err := a.config.Journal.OperationByThread(params.ThreadID)
	if err != nil || op.Gateway == nil {
		answer("No connector gateway is available to this thread.", false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCallTimeout)
	defer cancel()
	tools, err := a.gatewayTools(ctx, op.Gateway)
	if err != nil {
		answer("The connector gateway could not be reached; try again later.", false)
		return
	}
	var gatewayName string
	for _, tool := range tools {
		if dynamicToolName(tool.Name) == params.Tool {
			gatewayName = tool.Name
			break
		}
	}
	if gatewayName == "" {
		answer("This tool is no longer available to the task.", false)
		return
	}
	arguments := params.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := a.gatewayRPC(ctx, op.Gateway, "tools/call", map[string]any{"name": gatewayName, "arguments": arguments}, &result); err != nil {
		answer("The connector call did not complete; its outcome is not known.", false)
		return
	}
	var text []string
	for _, item := range result.Content {
		if item.Type == "text" {
			text = append(text, item.Text)
		}
	}
	answer(strings.Join(text, "\n"), !result.IsError)
}
