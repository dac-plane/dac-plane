package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	Binary, Home, Provider, Model, BaseURL, APIKeyEnv, APIKey string
	Logger                                                    *slog.Logger
}

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("app-server RPC %d: %s", e.Code, e.Message) }

type Client struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	mu            sync.Mutex
	next          uint64
	pending       map[string]chan Message
	done          chan struct{}
	err           error
	Notifications chan Message
	Requests      chan Message
	logger        *slog.Logger
	secrets       []string
}

func Start(ctx context.Context, config Config) (*Client, error) {
	if config.Binary == "" || config.Home == "" || config.Provider == "" || config.Model == "" || config.BaseURL == "" || config.APIKeyEnv == "" || config.APIKey == "" {
		return nil, errors.New("codex app-server configuration is incomplete")
	}
	if !filepath.IsAbs(config.Binary) || !filepath.IsAbs(config.Home) {
		return nil, errors.New("Codex app-server binary and home must be absolute")
	}
	if !providerName.MatchString(config.Provider) {
		return nil, errors.New("Codex model provider name is malformed")
	}
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && IsLoopback(endpoint.Hostname()))) {
		return nil, errors.New("Codex model provider URL must use HTTPS or loopback HTTP")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(config.Home, 0700); err != nil {
		return nil, fmt.Errorf("create Codex home: %w", err)
	}
	args := []string{
		"-c", `model_provider=` + strconv.Quote(config.Provider),
		"-c", `model=` + strconv.Quote(config.Model),
		"-c", `model_providers.` + config.Provider + `.name=` + strconv.Quote(config.Provider),
		"-c", `model_providers.` + config.Provider + `.base_url=` + strconv.Quote(config.BaseURL),
		"-c", `model_providers.` + config.Provider + `.env_key=` + strconv.Quote(config.APIKeyEnv),
		"-c", `model_providers.` + config.Provider + `.wire_api="responses"`,
		"--listen", "stdio://",
	}
	cmd := exec.CommandContext(ctx, config.Binary, args...)
	cmd.Env = append(withoutEnv(os.Environ(), "CODEX_HOME", config.APIKeyEnv), "CODEX_HOME="+config.Home, config.APIKeyEnv+"="+config.APIKey)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	c := &Client{cmd: cmd, stdin: stdin, pending: map[string]chan Message{}, done: make(chan struct{}), Notifications: make(chan Message, 256), Requests: make(chan Message, 64), logger: config.Logger, secrets: []string{config.APIKey}}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	go c.read(stdout)
	go c.logStderr(stderr)
	var initialized json.RawMessage
	if err := c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dac-codex-adapter", "title": "DAC Codex runtime adapter", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}, &initialized); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// IsLoopback reports whether host names the local machine. The adapter uses
// it for the only HTTP exception allowed by provider and upload endpoints.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func withoutEnv(env []string, names ...string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(v string) bool {
		for _, n := range names {
			if strings.HasPrefix(v, n+"=") {
				return true
			}
		}
		return false
	})
}

func (c *Client) read(reader io.Reader) {
	defer close(c.done)
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var m Message
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			c.finish(fmt.Errorf("decode app-server message: %w", err))
			return
		}
		if len(m.ID) > 0 && m.Method != "" {
			select {
			case c.Requests <- m:
			default:
				c.finish(errors.New("app-server request queue is full"))
				return
			}
			continue
		}
		if len(m.ID) > 0 {
			key := string(m.ID)
			c.mu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
				close(ch)
			}
			continue
		}
		if m.Method != "" {
			if m.Method == "turn/completed" {
				logTurnFailure(c.logger, m.Params)
			}
			select {
			case c.Notifications <- m:
			default:
				c.finish(errors.New("app-server notification queue is full"))
				return
			}
		}
	}
	if err := s.Err(); err != nil {
		c.finish(fmt.Errorf("read app-server: %w", err))
	} else {
		c.finish(io.EOF)
	}
}

func logTurnFailure(logger *slog.Logger, params json.RawMessage) {
	if logger == nil {
		return
	}
	var notification struct {
		Turn struct {
			Status string `json:"status"`
			Error  struct {
				Code string `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.Turn.Status != "failed" {
		return
	}
	logger.Warn("Codex turn failed", "turn_status", notification.Turn.Status,
		"codex_error_info", notification.Turn.Error.Code)
}

func (c *Client) finish(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	for key, ch := range c.pending {
		delete(c.pending, key)
		close(ch)
	}
	c.mu.Unlock()
}

func (c *Client) logStderr(reader io.Reader) {
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		line := s.Text()
		for _, secret := range c.secrets {
			line = strings.ReplaceAll(line, secret, "[REDACTED]")
		}
		c.logger.Debug("Codex app-server", "line", line)
	}
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := strconv.FormatUint(c.next, 10)
	ch := make(chan Message, 1)
	c.pending[id] = ch
	raw, err := json.Marshal(map[string]any{"id": c.next, "method": method, "params": params})
	if err == nil {
		_, err = c.stdin.Write(append(raw, '\n'))
	}
	if err != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			return err
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err == nil {
		_, err = c.stdin.Write(append(raw, '\n'))
	}
	return err
}
func (c *Client) Reply(id json.RawMessage, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var value any
	if err := json.Unmarshal(id, &value); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"id": value, "result": result})
	if err == nil {
		_, err = c.stdin.Write(append(raw, '\n'))
	}
	return err
}
func (c *Client) NotificationMessages() <-chan Message { return c.Notifications }
func (c *Client) RequestMessages() <-chan Message      { return c.Requests }
func (c *Client) Done() <-chan struct{}                { return c.done }
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *Client) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}
