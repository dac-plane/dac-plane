// Package execclient reads a remote workspace from Codex exec-server. Codex's
// own fs RPCs take no environment, so they only reach the adapter's filesystem.
package execclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/coder/websocket"
)

const (
	// maxMessageBytes bounds one reply, and so one file, read from a workspace.
	maxMessageBytes = 128 << 20
	// maxFiles bounds a walk of a workspace the agent controls.
	maxFiles = 10_000
)

// File is a regular file found in a workspace.
type File struct {
	Digest string
	Size   uint64
}

type Client struct {
	conn *websocket.Conn
	next uint64
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Dial connects and completes exec-server's handshake. It trusts the system
// roots, unlike the adapter's own client, because the endpoint is the provider's
// public port host rather than the control plane.
func Dial(ctx context.Context, endpoint, token string) (*Client, error) {
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect to exec-server: %w", err)
	}
	conn.SetReadLimit(maxMessageBytes)
	c := &Client{conn: conn}
	if err := c.call(ctx, "initialize", map[string]any{"clientName": "dac-codex-adapter"}, nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"method": "initialized", "params": map[string]any{}})
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, raw)
	}
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("finish exec-server handshake: %w", err)
	}
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "") }

// call runs one request. The client is used by one goroutine at a time, so the
// next reply with this id is the answer.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	c.next++
	raw, err := json.Marshal(map[string]any{"id": c.next, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("exec-server %s: %w", method, err)
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("exec-server %s: %w", method, err)
		}
		var reply struct {
			ID     *uint64         `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			return fmt.Errorf("decode exec-server reply: %w", err)
		}
		if reply.ID == nil || *reply.ID != c.next {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("exec-server %s: %s", method, reply.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(reply.Result, result)
	}
}

func fileURI(p string) string { return (&url.URL{Scheme: "file", Path: p}).String() }

type entry struct {
	Name        string `json:"fileName"`
	IsDirectory bool   `json:"isDirectory"`
	IsFile      bool   `json:"isFile"`
}

func (c *Client) readDirectory(ctx context.Context, dir string) ([]entry, error) {
	var result struct {
		Entries []entry `json:"entries"`
	}
	err := c.call(ctx, "fs/readDirectory", map[string]any{"path": fileURI(dir)}, &result)
	return result.Entries, err
}

// ReadFile returns a file's bytes.
func (c *Client) ReadFile(ctx context.Context, file string) ([]byte, error) {
	var result struct {
		Data string `json:"dataBase64"`
	}
	if err := c.call(ctx, "fs/readFile", map[string]any{"path": fileURI(file)}, &result); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(result.Data)
}

// Snapshot walks root and digests its regular files, keyed by relative path. It
// skips what the local snapshot skips, so a baseline means the same in both.
func (c *Client) Snapshot(ctx context.Context, root string) (map[string]File, error) {
	out := map[string]File{}
	pending := []string{""}
	for len(pending) > 0 {
		relative := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := c.readDirectory(ctx, path.Join(root, relative))
		if err != nil {
			return nil, err
		}
		for _, item := range entries {
			child := path.Join(relative, item.Name)
			switch {
			case item.IsDirectory && (item.Name == ".git" || item.Name == ".codex"):
			case item.IsDirectory:
				pending = append(pending, child)
			case item.IsFile:
				if len(out) == maxFiles {
					return nil, errors.New("workspace has too many files to snapshot")
				}
				data, err := c.ReadFile(ctx, path.Join(root, child))
				if err != nil {
					return nil, err
				}
				sum := sha256.Sum256(data)
				out[child] = File{Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: uint64(len(data))}
			}
		}
	}
	return out, nil
}
