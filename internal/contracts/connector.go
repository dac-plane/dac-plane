package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"github.com/dac-plane/dac-plane/internal/lifecycle"
)

const (
	// MaxConnectorTools bounds a connector's tool catalogue.
	MaxConnectorTools = 64
	// MaxToolDescriptionCharacters bounds a tool's description.
	MaxToolDescriptionCharacters = 1_000
	// MaxToolSchemaBytes bounds a tool's input schema.
	MaxToolSchemaBytes = 16 << 10
	// MaxToolInputBytes bounds one call's input.
	MaxToolInputBytes = 32 << 10
	// MaxToolOutputBytes bounds one call's output.
	MaxToolOutputBytes = 256 << 10
	// maxAuthorizationURLBytes bounds an authorization URL.
	maxAuthorizationURLBytes = 4 << 10
	// maxGatewayTokenBytes bounds an attempt's gateway token.
	maxGatewayTokenBytes = 512
)

// ConnectorTool is one operation a connector offers. Its name is one segment
// of the governed action class connector.<name>. InputSchema is a JSON Schema
// object describing the call's input; ReadOnly says the call changes nothing
// at the external application.
type ConnectorTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
}

// Validate reports whether the tool is well formed.
func (t ConnectorTool) Validate() error {
	if err := lifecycle.ValidateToolName(t.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !utf8.ValidString(t.Description) || strings.TrimSpace(t.Description) == "" || utf8.RuneCountInString(t.Description) > MaxToolDescriptionCharacters {
		return fmt.Errorf("%w: tool description must be non-blank and at most %d characters", ErrInvalid, MaxToolDescriptionCharacters)
	}
	var schema map[string]json.RawMessage
	if len(t.InputSchema) > MaxToolSchemaBytes || json.Unmarshal(t.InputSchema, &schema) != nil || string(schema["type"]) != `"object"` {
		return fmt.Errorf("%w: tool input schema must be a bounded JSON Schema object of type object", ErrInvalid)
	}
	return nil
}

// ToolCatalogue is a connector's reply to list_tools.
type ToolCatalogue struct {
	Tools []ConnectorTool `json:"tools"`
}

// Validate reports whether the catalogue is bounded and its names distinct.
func (c ToolCatalogue) Validate() error {
	if len(c.Tools) == 0 || len(c.Tools) > MaxConnectorTools {
		return fmt.Errorf("%w: catalogue lists 1 to %d tools", ErrInvalid, MaxConnectorTools)
	}
	seen := make(map[string]bool, len(c.Tools))
	for _, tool := range c.Tools {
		if err := tool.Validate(); err != nil {
			return err
		}
		if seen[tool.Name] {
			return fmt.Errorf("%w: catalogue names a tool twice", ErrInvalid)
		}
		seen[tool.Name] = true
	}
	return nil
}

// Tool returns the named tool.
func (c ToolCatalogue) Tool(name string) (ConnectorTool, bool) {
	for _, tool := range c.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return ConnectorTool{}, false
}

// ConnectionReference names the connection a connector request is about:
// the arguments of complete_authorization and revoke_connection.
type ConnectionReference struct {
	Connection ids.ConnectionID `json:"connection_id"`
}

// Validate reports whether the reference is set.
func (r ConnectionReference) Validate() error {
	if r.Connection.IsZero() {
		return fmt.Errorf("%w: connection required", ErrInvalid)
	}
	return nil
}

// BeginAuthorization asks a connector for a consent URL for one connection.
// The state it binds to the URL must be single-use and expire by ExpiresAt.
type BeginAuthorization struct {
	Connection ids.ConnectionID `json:"connection_id"`
	ExpiresAt  time.Time        `json:"expires_at"`
}

// Validate reports whether the request is complete.
func (b BeginAuthorization) Validate() error {
	if b.Connection.IsZero() || b.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: begin authorization requires connection and expiry", ErrInvalid)
	}
	return nil
}

// AuthorizationStarted is a connector's reply to begin_authorization: the URL
// the connection's owner opens to consent, never anything a credential could
// be derived from.
type AuthorizationStarted struct {
	URL       string    `json:"authorization_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate reports whether the URL is an absolute HTTPS URL, or HTTP on a
// loopback host for local development.
func (a AuthorizationStarted) Validate() error {
	if len(a.URL) > maxAuthorizationURLBytes || a.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: authorization requires a bounded URL and an expiry", ErrInvalid)
	}
	parsed, err := url.Parse(a.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%w: authorization URL must be absolute, without credentials or fragment", ErrInvalid)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return fmt.Errorf("%w: authorization URL must use HTTPS", ErrInvalid)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// AuthorizationStatus is how far a connection's consent has come.
type AuthorizationStatus string

const (
	AuthorizationPending    AuthorizationStatus = "pending"
	AuthorizationAuthorized AuthorizationStatus = "authorized"
	AuthorizationFailed     AuthorizationStatus = "failed"
)

// AuthorizationStatuses lists every AuthorizationStatus.
func AuthorizationStatuses() []AuthorizationStatus {
	return []AuthorizationStatus{AuthorizationPending, AuthorizationAuthorized, AuthorizationFailed}
}

// AuthorizationResult is a connector's reply to complete_authorization. An
// authorized result reports the account and the permissions granted.
type AuthorizationResult struct {
	Status  AuthorizationStatus          `json:"status"`
	Account *lifecycle.ConnectionAccount `json:"account,omitempty"`
}

// Validate reports whether the account is present exactly when authorized.
func (r AuthorizationResult) Validate() error {
	switch r.Status {
	case AuthorizationAuthorized:
		if r.Account == nil {
			return fmt.Errorf("%w: an authorized result reports the account", ErrInvalid)
		}
		if err := r.Account.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	case AuthorizationPending, AuthorizationFailed:
		if r.Account != nil {
			return fmt.Errorf("%w: only an authorized result reports an account", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown authorization status", ErrInvalid)
	}
	return nil
}

// ConnectorExecution is the call_tool argument for a connector: one
// authorized call of one tool on one connection, with its exact input. The
// connector resolves the credential from the connection; nothing here could
// stand in for it. Authority provenance is the attempt grant used and the
// connection grant that covered the call; a one-time approval also names its
// decision.
type ConnectorExecution struct {
	Request         identity.ActionRequest   `json:"request"`
	Grant           ids.GrantID              `json:"grant_id"`
	Decision        ids.DecisionID           `json:"decision_id,omitzero"`
	Source          identity.AuthoritySource `json:"source"`
	ConnectionGrant ids.ConnectionGrantID    `json:"connection_grant_id"`
	Connection      ids.ConnectionID         `json:"connection_id"`
	Tool            string                   `json:"tool"`
	Input           json.RawMessage          `json:"input"`
}

// Validate reports whether the execution is complete and its parts agree:
// the action is the tool's, the scope is the connection and the input hashes
// to the authorized digest.
func (e ConnectorExecution) Validate() error {
	if err := e.Request.Validate(); err != nil {
		return err
	}
	if e.Grant.IsZero() || e.ConnectionGrant.IsZero() || e.Connection.IsZero() {
		return fmt.Errorf("%w: connector execution requires grant, connection grant and connection", ErrInvalid)
	}
	switch e.Source {
	case identity.SourceOneTimeApproval:
		if e.Decision.IsZero() {
			return fmt.Errorf("%w: a one-time approval names its decision", ErrInvalid)
		}
	case identity.SourceStandingGrant:
	default:
		return fmt.Errorf("%w: execution requires authority provenance", ErrInvalid)
	}
	if err := lifecycle.ValidateToolName(e.Tool); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if e.Request.Action != lifecycle.ConnectorAction(e.Tool) || e.Request.Scope != ConnectionScope(e.Connection) {
		return fmt.Errorf("%w: action and scope must name the tool and connection", ErrInvalid)
	}
	if err := ValidateToolInput(e.Input); err != nil {
		return err
	}
	if identity.DigestOf(e.Input) != e.Request.InputDigest {
		return fmt.Errorf("%w: input does not match its digest", ErrInvalid)
	}
	return nil
}

// ConnectionScope is the scope of every governed action on connection.
func ConnectionScope(connection ids.ConnectionID) identity.Scope {
	return identity.Scope{Class: identity.ScopeSubject, Target: "connection:" + connection.String()}
}

// ValidateToolInput reports whether input is a bounded JSON object in
// canonical form.
func ValidateToolInput(input json.RawMessage) error {
	var object map[string]json.RawMessage
	if len(input) == 0 || len(input) > MaxToolInputBytes || json.Unmarshal(input, &object) != nil || object == nil {
		return fmt.Errorf("%w: tool input must be a JSON object of at most %d bytes", ErrInvalid, MaxToolInputBytes)
	}
	if canonical, err := CanonicalJSON(input); err != nil || !bytes.Equal(canonical, input) {
		return fmt.Errorf("%w: tool input must be in canonical form", ErrInvalid)
	}
	return nil
}

// CanonicalJSON returns raw in the form encoding/json writes a
// json.RawMessage: compact, with <, > and & escaped as \u003c, \u003e and
// \u0026, and keys in their original order. A tool call's input digest covers
// this form, so encoding a record that carries the input never changes it.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: not JSON", ErrInvalid)
	}
	return json.Marshal(raw)
}

// ConnectorResult is a connector's reply to call_tool: the call's output
// as canonical JSON and whether the external application refused or failed it. A
// refusal by the external application is a completed call, not a protocol
// failure. The output never carries a credential.
type ConnectorResult struct {
	Operation    ids.OperationID `json:"operation_id"`
	Output       json.RawMessage `json:"output"`
	OutputDigest identity.Digest `json:"output_digest"`
	IsError      bool            `json:"is_error,omitempty"`
	CompletedAt  time.Time       `json:"completed_at"`
}

// Validate reports whether the output is bounded JSON matching its digest.
func (r ConnectorResult) Validate() error {
	if r.Operation.IsZero() || r.CompletedAt.IsZero() {
		return fmt.Errorf("%w: connector result requires operation and completion time", ErrInvalid)
	}
	if len(r.Output) == 0 || len(r.Output) > MaxToolOutputBytes || !json.Valid(r.Output) {
		return fmt.Errorf("%w: connector output must be JSON of at most %d bytes", ErrInvalid, MaxToolOutputBytes)
	}
	if canonical, err := CanonicalJSON(r.Output); err != nil || !bytes.Equal(canonical, r.Output) {
		return fmt.Errorf("%w: connector output must be in canonical form", ErrInvalid)
	}
	if identity.DigestOf(r.Output) != r.OutputDigest {
		return fmt.Errorf("%w: output does not match its digest", ErrInvalid)
	}
	return nil
}

// GatewayAccess is how a runtime reaches the connector gateway for one
// attempt: the gateway's MCP endpoint and a bearer token whose only audience
// is that gateway. The token authorizes nothing at an external application.
type GatewayAccess struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Validate reports whether the endpoint is absolute HTTP(S) and the token a
// bounded printable string.
func (g GatewayAccess) Validate() error {
	parsed, err := url.Parse(g.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%w: gateway URL must be absolute HTTP(S) without credentials", ErrInvalid)
	}
	if g.Token == "" || len(g.Token) > maxGatewayTokenBytes || bytes.ContainsFunc([]byte(g.Token), func(r rune) bool { return r <= ' ' || r > '~' }) {
		return fmt.Errorf("%w: gateway token must be bounded printable ASCII", ErrInvalid)
	}
	return nil
}
