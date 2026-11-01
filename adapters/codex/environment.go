package codexadapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/execclient"
	"github.com/AssahBismarkabah/dac-plane/adapters/codex/journal"
)

const (
	// defaultEnvironmentGrace allows a dropped connection to return. Observed on
	// Codex 0.165.0, a deleted sandbox was reported lost after 26 seconds, so this
	// adds little to how long a dead environment takes to end its turn.
	defaultEnvironmentGrace = 10 * time.Second
	// remoteReadTimeout bounds a snapshot or file read over exec-server.
	remoteReadTimeout = 2 * time.Minute
	// attachTokenLabel separates this use of the attach key from any other.
	attachTokenLabel = "dac-exec-server-v1\x00"
)

// attachToken derives a resource's bearer token. The environment adapter derives
// the same value from the shared key, so no secret crosses the control plane.
func attachToken(key []byte, ref string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(attachTokenLabel + ref))
	return hex.EncodeToString(mac.Sum(nil))
}

// environmentID names a resource in Codex, which limits ids to 64 characters.
func environmentID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "env-" + hex.EncodeToString(sum[:8])
}

// execServerURL fills the deployment's address with the part of the opaque
// reference after its last colon.
func (a *Adapter) execServerURL(ref string) string {
	return strings.ReplaceAll(a.config.ExecServerURL, "{resource}", ref[strings.LastIndexByte(ref, ':')+1:])
}

// addEnvironment registers a resource with Codex, which does not persist it
// across a restart.
func (a *Adapter) addEnvironment(ctx context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	var result any
	err := a.config.Native.Call(ctx, "environment/add", map[string]any{"environmentId": environmentID(ref), "execServerUrl": a.execServerURL(ref), "authBearerToken": attachToken(a.config.AttachKey, ref)}, &result)
	if err != nil {
		return fmt.Errorf("add Codex environment: %w", err)
	}
	return nil
}

func (a *Adapter) threadEnvironments(ref string) []map[string]any {
	return []map[string]any{{"environmentId": environmentID(ref), "cwd": a.config.Workspace}}
}

func (a *Adapter) dialEnvironment(ctx context.Context, ref string) (*execclient.Client, error) {
	return execclient.Dial(ctx, a.execServerURL(ref), attachToken(a.config.AttachKey, ref), a.config.HTTPClient)
}

// workspaceSnapshot digests the workspace where the commands run. Sizes are
// returned only for a remote one, whose files the adapter cannot stat.
func (a *Adapter) workspaceSnapshot(ctx context.Context, ref string) (map[string]string, map[string]uint64, error) {
	if ref == "" {
		digests, err := snapshot(a.config.Workspace)
		return digests, nil, err
	}
	client, err := a.dialEnvironment(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	defer client.Close()
	files, err := client.Snapshot(ctx, a.config.Workspace)
	if err != nil {
		return nil, nil, err
	}
	digests, sizes := make(map[string]string, len(files)), make(map[string]uint64, len(files))
	for path, file := range files {
		digests[path], sizes[path] = file.Digest, file.Size
	}
	return digests, sizes, nil
}

func (a *Adapter) exportRemote(ctx context.Context, r wire.Request, t transfer, ref string, artifact journal.Artifact) (wire.Reply, error) {
	client, err := a.dialEnvironment(ctx, ref)
	if err != nil {
		return wire.Reply{}, err
	}
	defer client.Close()
	data, err := client.ReadFile(ctx, artifact.Path)
	if err != nil {
		return wire.Reply{}, err
	}
	if uint64(len(data)) != artifact.Size || journal.Hash(data) != artifact.Digest {
		return rejected(r, wire.FailureStalePrecondition, "artifact bytes changed"), nil
	}
	if err := a.upload(ctx, t.Upload, bytes.NewReader(data), artifact); err != nil {
		return wire.Reply{}, err
	}
	return accepted(r, nil), nil
}

func (a *Adapter) setEnvironmentLost(thread string, lost bool) {
	a.lostMu.Lock()
	defer a.lostMu.Unlock()
	if lost {
		a.lostEnvironments[thread] = true
	} else {
		delete(a.lostEnvironments, thread)
	}
}

func (a *Adapter) isEnvironmentLost(thread string) bool {
	a.lostMu.Lock()
	defer a.lostMu.Unlock()
	return a.lostEnvironments[thread]
}

// environmentLost ends a running turn whose environment does not return. Left
// alone, the model retries against the dead link and then finishes the turn as
// completed, which was observed on Codex 0.165.0.
func (a *Adapter) environmentLost(thread string) {
	if thread == "" {
		return
	}
	a.setEnvironmentLost(thread, true)
	go func() {
		select {
		case <-a.stop:
			return
		case <-time.After(a.config.EnvironmentGrace):
		}
		if !a.isEnvironmentLost(thread) {
			return
		}
		if err := a.interruptLostTurn(thread); err != nil && a.config.Logger != nil {
			a.config.Logger.Warn("interrupt a turn without its environment", "error_type", fmt.Sprintf("%T", err))
		}
	}()
}

func (a *Adapter) interruptLostTurn(thread string) error {
	op, err := a.config.Journal.OperationByThread(thread)
	if err != nil {
		return err
	}
	if op.State != "running" || op.TurnID == "" {
		return errors.New("no running turn to interrupt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalCleanTimeout)
	defer cancel()
	var result any
	return a.config.Native.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": op.TurnID}, &result)
}
