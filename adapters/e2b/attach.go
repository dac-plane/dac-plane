package e2badapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// defaultExecServerPort is where exec-server listens in the sandbox.
	defaultExecServerPort = 8765
	// envdPort is the sandbox's own process and file service.
	envdPort = 49983
	// sandboxUser owns the workspace in the template.
	sandboxUser = "user"
	// defaultSandboxDomain is E2B's public port host.
	defaultSandboxDomain = "e2b.app"
	// execServerReadyTimeout bounds the wait for exec-server to listen.
	execServerReadyTimeout = 30 * time.Second
	execServerPollInterval = 250 * time.Millisecond
	// connectEndStream marks the Connect frame that ends a stream.
	connectEndStream = 2
)

// attachTokenLabel separates this use of the attach key from any other.
const attachTokenLabel = "dac-exec-server-v1\x00"

// attachToken derives the bearer token for a resource. The runtime adapter
// derives the same value from the shared key, so no secret crosses the control
// plane and nothing needs storing to attach again after a restart.
func attachToken(key []byte, ref string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(attachTokenLabel + ref))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Adapter) sandboxURL(port int, sandboxID string) string {
	if a.cfg.SandboxURL != nil {
		return a.cfg.SandboxURL(port, sandboxID)
	}
	return fmt.Sprintf("https://%d-%s.%s", port, sandboxID, a.cfg.SandboxDomain)
}

// prepareAttachment starts exec-server in a sandbox and waits until it listens.
// Repeating it is harmless: a second exec-server fails to bind and exits.
func (a *Adapter) prepareAttachment(ctx context.Context, sbx sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, execServerReadyTimeout)
	defer cancel()
	sum := sha256.Sum256([]byte(attachToken(a.cfg.AttachKey, "sandbox:"+sbx.SandboxID)))
	command := fmt.Sprintf(`mkdir -p "$HOME/.dac-codex" && CODEX_HOME="$HOME/.dac-codex" exec codex exec-server --listen ws://0.0.0.0:%d --ws-auth capability-token --ws-token-sha256 %s >/tmp/exec-server.log 2>&1`, a.cfg.ExecServerPort, hex.EncodeToString(sum[:]))
	if err := a.startProcess(ctx, sbx, command); err != nil {
		return err
	}
	return a.waitForExecServer(ctx, sbx.SandboxID)
}

// startProcess starts a command through envd and returns once it has started.
// Closing the stream leaves the process running.
func (a *Adapter) startProcess(ctx context.Context, sbx sandbox, command string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	message, err := json.Marshal(map[string]any{"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-l", "-c", command}}})
	if err != nil {
		return err
	}
	frame := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	frame = append(frame, message...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.sandboxURL(envdPort, sbx.SandboxID)+"/process.Process/Start", bytes.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(sandboxUser+":")))
	req.Header.Set("X-Access-Token", sbx.EnvdAccessToken)
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("start exec-server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("start exec-server: envd returned HTTP %d", resp.StatusCode)
	}
	var header [5]byte
	if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
		return fmt.Errorf("start exec-server: %w", err)
	}
	if header[0]&connectEndStream != 0 {
		return errors.New("start exec-server: envd refused the process")
	}
	return nil
}

// waitForExecServer treats any answer but the port host's 502 as listening. A
// request without a token is enough, since exec-server answers it with 401.
func (a *Adapter) waitForExecServer(ctx context.Context, sandboxID string) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.sandboxURL(a.cfg.ExecServerPort, sandboxID), nil)
		if err != nil {
			return err
		}
		resp, err := a.cfg.HTTPClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("exec-server did not start listening")
		case <-time.After(execServerPollInterval):
		}
	}
}
