// Command dac-sim-adapter runs a deterministic simulator as a reference
// adapter (ADR-0008) for the conformance runner: it reads its scenario from a
// JSON file and connects to the control plane the adapter kit way, taking the
// endpoint, the trusted certificate and the access token from the
// conformance contract's environment. It links the simulator, so it is a
// test and demonstration binary; the control plane never depends on it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// scenarioFile is the JSON form of a simulator script: operations with their
// replies and effects, and an optional stream the conformance suite
// subscribes to, keyed by its operation. Everything the adapter answers comes
// from this file; an unscripted operation fails loudly, never inventing a
// success.
type scenarioFile struct {
	Tenant     string `json:"tenant_id"`
	Provider   string `json:"provider_id"`
	Role       string `json:"role"`
	Operations []struct {
		Operation string           `json:"operation_id"`
		Reply     json.RawMessage  `json:"reply"`
		Effect    string           `json:"effect"`
		Stream    *json.RawMessage `json:"stream"`
	} `json:"operations"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: dac-sim-adapter SCENARIO.json")
		return 2
	}
	scenario, err := readScenario(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-sim-adapter: %v\n", err)
		return 2
	}
	url := os.Getenv("DAC_ADAPTER_URL")
	if url == "" {
		_, _ = fmt.Fprintln(stderr, "dac-sim-adapter: DAC_ADAPTER_URL is required")
		return 2
	}
	tokenPath := os.Getenv("DAC_ADAPTER_TOKEN_FILE")
	if tokenPath == "" {
		_, _ = fmt.Fprintln(stderr, "dac-sim-adapter: DAC_ADAPTER_TOKEN_FILE is required")
		return 2
	}
	tlsConfig, err := trust(os.Getenv("DAC_ADAPTER_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-sim-adapter: %v\n", err)
		return 2
	}
	port, err := scenario.port()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-sim-adapter: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	err = adapterkit.Run(ctx, adapterkit.Config{
		URL: url, Provider: scenario.Provider, Role: wire.Role(scenario.Role), Handler: simulator.Handler(port),
		Token: adapterkit.FileToken(tokenPath), TLS: tlsConfig, Logger: logger,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(stderr, "dac-sim-adapter: %v\n", err)
		return 1
	}
	return 0
}

// readScenario reads and validates the scenario file.
func readScenario(path string) (scenarioFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scenarioFile{}, errors.New("cannot read the scenario file")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var scenario scenarioFile
	if err := decoder.Decode(&scenario); err != nil {
		return scenarioFile{}, fmt.Errorf("scenario: %w", err)
	}
	return scenario, nil
}

// port builds the simulator the scenario scripts.
func (s scenarioFile) port() (contracts.Port, error) {
	tenant, err := ids.Parse[ids.Tenant](s.Tenant)
	if err != nil {
		return nil, fmt.Errorf("scenario: tenant_id: %w", err)
	}
	provider, err := ids.Parse[ids.Provider](s.Provider)
	if err != nil {
		return nil, fmt.Errorf("scenario: provider_id: %w", err)
	}
	if s.Role != string(contracts.RoleRuntime) {
		return nil, fmt.Errorf("scenario: role: dac-sim-adapter serves a %s", contracts.RoleRuntime)
	}
	operations := map[ids.OperationID]simulator.RuntimeOp{}
	for _, op := range s.Operations {
		operation, err := ids.Parse[ids.Operation](op.Operation)
		if err != nil {
			return nil, fmt.Errorf("scenario: operation_id: %w", err)
		}
		step := simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.EffectKind(op.Effect)}
		if len(op.Reply) > 0 {
			if err := json.Unmarshal(op.Reply, &step.Reply); err != nil {
				return nil, fmt.Errorf("scenario: reply for %s: %w", op.Operation, err)
			}
		}
		if op.Stream != nil {
			frames := []struct {
				Kind string          `json:"kind"`
				Body json.RawMessage `json:"body"`
			}{}
			if err := json.Unmarshal(*op.Stream, &frames); err != nil {
				return nil, fmt.Errorf("scenario: stream for %s: %w", op.Operation, err)
			}
			parsed := make([]contracts.StreamFrame, 0, len(frames))
			for _, frame := range frames {
				var parsedFrame contracts.StreamFrame
				if err := json.Unmarshal(frame.Body, &parsedFrame); err != nil {
					return nil, fmt.Errorf("scenario: stream frame %s: %w", frame.Kind, err)
				}
				if parsedFrame.Kind == "" {
					parsedFrame.Kind = contracts.StreamKind(frame.Kind)
				}
				parsed = append(parsed, parsedFrame)
			}
			step.Stream = &simulator.StreamScript{Frames: parsed}
		}
		operations[operation] = step
	}
	runtime, err := simulator.NewRuntime(simulator.Config{Tenant: tenant, Provider: provider}, simulator.RuntimeScenario{Operations: operations})
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

// trust loads the PEM certificate the environment names; empty means the
// system roots.
func trust(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("cannot read the certificate file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the certificate file holds no certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
