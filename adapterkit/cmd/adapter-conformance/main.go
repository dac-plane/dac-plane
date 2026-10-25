// Command adapter-conformance runs the ControlFrame conformance suite
// against an adapter written in any language. It plays the control plane,
// starts the adapter command with where to connect, and reports each
// scenario:
//
//	adapter-conformance -fixture fixture.json -- ./my-adapter --flag
//
// The adapter receives DAC_ADAPTER_URL (the endpoint), DAC_ADAPTER_CA_FILE
// (the PEM certificate to trust) and DAC_ADAPTER_TOKEN_FILE (an unsigned test
// token with an exp claim; the harness does not verify tokens).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/conformance"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("adapter-conformance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fixturePath := flags.String("fixture", "", "JSON file describing the adapter and a request it answers")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if *fixturePath == "" || len(command) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: adapter-conformance -fixture FILE -- COMMAND [ARGS...]")
		return 2
	}
	fixture, err := readFixture(*fixturePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "adapter-conformance: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results := conformance.Check(ctx, conformance.Target{Fixture: fixture, Start: startCommand(command, stderr)})
	failed := 0
	for _, result := range results {
		status := "PASS"
		if result.Err != nil {
			status, failed = "FAIL", failed+1
		}
		_, _ = fmt.Fprintf(stdout, "%s  %s", status, result.Name)
		if result.Err != nil {
			_, _ = fmt.Fprintf(stdout, ": %v", result.Err)
		}
		_, _ = fmt.Fprintln(stdout)
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(stdout, "%d of %d scenarios failed\n", failed, len(results))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "all %d scenarios passed\n", len(results))
	return 0
}

func readFixture(path string) (conformance.Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return conformance.Fixture{}, errors.New("cannot read the fixture file")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var fixture conformance.Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return conformance.Fixture{}, fmt.Errorf("fixture: %w", err)
	}
	return fixture, nil
}

// startCommand starts the adapter process in its own scratch directory.
func startCommand(command []string, stderr io.Writer) func(context.Context, conformance.Endpoint) (func(), error) {
	return func(ctx context.Context, endpoint conformance.Endpoint) (func(), error) {
		dir, err := os.MkdirTemp("", "adapter-conformance-")
		if err != nil {
			return nil, err
		}
		caFile, tokenFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
		claims := fmt.Sprintf(`{"sub":"conformance","exp":%d}`, time.Now().Add(time.Hour).Unix())
		token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".conformance"
		if err := os.WriteFile(caFile, endpoint.CertificatePEM, 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Env = append(os.Environ(), "DAC_ADAPTER_URL="+endpoint.URL, "DAC_ADAPTER_CA_FILE="+caFile, "DAC_ADAPTER_TOKEN_FILE="+tokenFile)
		cmd.Stdout, cmd.Stderr = stderr, stderr
		if err := cmd.Start(); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("start adapter: %w", err)
		}
		return func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = os.RemoveAll(dir)
		}, nil
	}
}
