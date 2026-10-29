// Command dac-sim-adapter runs a deterministic simulator as a reference
// adapter (ADR-0008): it reads its scenario from a JSON file and connects to
// the control plane the adapter kit way. It serves the conformance runner and
// the reference stack's development profile (ADR-0009). It links the
// simulator, so it is a test and demonstration binary; the control plane
// never depends on it.
//
// Settings come from the environment:
//
//	DAC_ADAPTER_URL                 one adapter endpoint, or
//	DAC_ADAPTER_URLS                comma-separated endpoints for operations tests
//	DAC_ADAPTER_CA_FILE             PEM roots to trust; empty uses the system roots
//	DAC_ADAPTER_TOKEN_FILE          a JWT access token file, as the conformance contract passes it
//	DAC_ADAPTER_TOKEN_URL           or: an OAuth token endpoint for client credentials, with
//	DAC_ADAPTER_CLIENT_ID           the runtime's client and
//	DAC_ADAPTER_CLIENT_SECRET_FILE  its secret file,
//	DAC_ADAPTER_SCOPE               the scope naming the adapter endpoint's audience, and, when
//	                                the scenario has an artifact store, its own client in
//	DAC_ARTIFACT_STORE_CLIENT_ID and DAC_ARTIFACT_STORE_CLIENT_SECRET_FILE
//	DAC_SIM_CONTROL_ADDR            optional simulator evidence listener (tests only)
//	DAC_SIM_ARTIFACT_ONLY           serve only the scenario's artifact store (tests only)
//	DAC_SIM_ARTIFACT_ADDR           HTTPS byte-upload listener used with artifact-only mode
//	DAC_SIM_ARTIFACT_PUBLIC_URL     listener URL reachable by the runtime adapter
//	DAC_SIM_ARTIFACT_CERT_FILE      upload-listener certificate
//	DAC_SIM_ARTIFACT_KEY_FILE       upload-listener private key
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit"
	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// scenarioFile is the JSON form of a simulator script. Operations script
// known operation identities, with their replies and effects and an optional
// stream. Default scripts every other operation, for a stack where
// identities are generated at submission: its rows stream on subscribe and
// its after_decision rows follow once a decision was delivered. An artifact
// store, when present, receives the runtime's exports of the named fixtures.
// Everything the adapter answers comes from this file; an unscripted
// operation fails loudly, never inventing a success.
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
	Default *struct {
		Reply         json.RawMessage `json:"reply"`
		Effect        string          `json:"effect"`
		Rows          []scriptRow     `json:"rows"`
		AfterDecision []scriptRow     `json:"after_decision"`
	} `json:"default"`
	ArtifactStore *struct {
		Provider string            `json:"provider_id"`
		Fixtures map[string]string `json:"fixtures"`
	} `json:"artifact_store"`
}

// scriptRow is one stream entry; sequences are assigned in order.
type scriptRow struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// adapter is one registration this process serves.
type adapter struct {
	provider string
	role     wire.Role
	port     contracts.Port
	token    adapterkit.TokenSource
	offered  []string
}

type simulatorSet struct {
	adapters []adapter
	runtime  *simulator.Runtime
	store    *simulator.ByteArtifactStore
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: dac-sim-adapter SCENARIO.json")
		return 2
	}
	fail := func(code int, err error) int {
		_, _ = fmt.Fprintf(stderr, "dac-sim-adapter: %v\n", err)
		return code
	}
	scenario, err := readScenario(args[0])
	if err != nil {
		return fail(2, err)
	}
	urls, err := adapterURLs(os.Getenv("DAC_ADAPTER_URL"), os.Getenv("DAC_ADAPTER_URLS"))
	if err != nil {
		return fail(2, err)
	}
	tlsConfig, err := trust(os.Getenv("DAC_ADAPTER_CA_FILE"))
	if err != nil {
		return fail(2, err)
	}
	artifactOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("DAC_SIM_ARTIFACT_ONLY")), "true")
	set, err := scenario.adapters(tlsConfig, artifactOnly)
	if err != nil {
		return fail(2, err)
	}
	var uploads *artifactUploadHandler
	if artifactOnly {
		uploads, err = configureArtifactOnly(set, os.Getenv)
		if err != nil {
			return fail(2, err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	workers := len(urls)*len(set.adapters) + 2
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for _, endpoint := range urls {
		for _, a := range set.adapters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := adapterkit.Run(ctx, adapterkit.Config{
					URL: endpoint, Provider: a.provider, Role: a.role, Handler: simulator.Handler(a.port),
					Token: a.token, Offered: a.offered, TLS: tlsConfig, Logger: logger.With("role", string(a.role), "endpoint", endpoint),
				})
				if err != nil && !errors.Is(err, context.Canceled) {
					errs <- fmt.Errorf("%s at %s: %w", a.role, endpoint, err)
					stop()
				}
			}()
		}
	}
	if addr := controlAddress(os.Getenv); addr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveControl(ctx, addr, set, logger.With("component", "control")); err != nil && !errors.Is(err, context.Canceled) {
				errs <- err
				stop()
			}
		}()
	}
	if uploads != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveArtifactUploads(ctx, os.Getenv("DAC_SIM_ARTIFACT_ADDR"), os.Getenv("DAC_SIM_ARTIFACT_CERT_FILE"), os.Getenv("DAC_SIM_ARTIFACT_KEY_FILE"), uploads, logger.With("component", "artifact-upload")); err != nil && !errors.Is(err, context.Canceled) {
				errs <- err
				stop()
			}
		}()
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if err := errors.Join(failures...); err != nil {
		return fail(1, err)
	}
	return 0
}

func controlAddress(getenv func(string) string) string {
	return strings.TrimSpace(getenv("DAC_SIM_CONTROL_ADDR"))
}

func adapterURLs(single, multiple string) ([]string, error) {
	single, multiple = strings.TrimSpace(single), strings.TrimSpace(multiple)
	if single != "" && multiple != "" {
		return nil, errors.New("set DAC_ADAPTER_URL or DAC_ADAPTER_URLS, not both")
	}
	if single != "" {
		return []string{single}, nil
	}
	seen := map[string]bool{}
	var urls []string
	for _, endpoint := range strings.Split(multiple, ",") {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" || seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		urls = append(urls, endpoint)
	}
	if len(urls) == 0 {
		return nil, errors.New("DAC_ADAPTER_URL or DAC_ADAPTER_URLS is required")
	}
	return urls, nil
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

// adapters builds the runtime, and the artifact store it exports to when
// the scenario names one, each with its own token source.
func (s scenarioFile) adapters(tlsConfig *tls.Config, artifactOnly bool) (*simulatorSet, error) {
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
	set := &simulatorSet{}
	if !artifactOnly {
		runtimeToken, err := tokenSource("DAC_ADAPTER", tlsConfig)
		if err != nil {
			return nil, err
		}
		scenario, err := s.runtimeScenario()
		if err != nil {
			return nil, err
		}
		runtime, err := simulator.NewRuntime(simulator.Config{Tenant: tenant, Provider: provider}, scenario)
		if err != nil {
			return nil, err
		}
		set.runtime = runtime
		set.adapters = []adapter{{provider: s.Provider, role: wire.RoleRuntime, port: runtime, token: runtimeToken, offered: []string{wire.CapabilityOperationReplay}}}
	}
	if s.ArtifactStore == nil {
		return set, nil
	}
	storeID, err := ids.Parse[ids.Provider](s.ArtifactStore.Provider)
	if err != nil {
		return nil, fmt.Errorf("scenario: artifact_store.provider_id: %w", err)
	}
	store, err := simulator.NewByteArtifactStore(simulator.Config{Tenant: tenant, Provider: storeID})
	if err != nil {
		return nil, err
	}
	fixtures := map[string][]byte{}
	for ref, text := range s.ArtifactStore.Fixtures {
		fixtures[ref] = []byte(text)
	}
	if set.runtime != nil {
		set.runtime.WithArtifactExport(store, fixtures)
	}
	set.store = store
	storeToken, err := tokenSource("DAC_ARTIFACT_STORE", tlsConfig)
	if err != nil {
		return nil, err
	}
	set.adapters = append(set.adapters, adapter{provider: s.ArtifactStore.Provider, role: wire.RoleArtifactStore, port: store, token: storeToken})
	return set, nil
}

func (s scenarioFile) runtimeScenario() (simulator.RuntimeScenario, error) {
	operations := map[ids.OperationID]simulator.RuntimeOp{}
	for _, op := range s.Operations {
		operation, err := ids.Parse[ids.Operation](op.Operation)
		if err != nil {
			return simulator.RuntimeScenario{}, fmt.Errorf("scenario: operation_id: %w", err)
		}
		step := simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.EffectKind(op.Effect)}
		if err := reply(op.Reply, &step.Reply); err != nil {
			return simulator.RuntimeScenario{}, fmt.Errorf("scenario: reply for %s: %w", op.Operation, err)
		}
		if op.Stream != nil {
			frames := []struct {
				Kind string          `json:"kind"`
				Body json.RawMessage `json:"body"`
			}{}
			if err := json.Unmarshal(*op.Stream, &frames); err != nil {
				return simulator.RuntimeScenario{}, fmt.Errorf("scenario: stream for %s: %w", op.Operation, err)
			}
			parsed := make([]contracts.StreamFrame, 0, len(frames))
			for _, frame := range frames {
				var parsedFrame contracts.StreamFrame
				if err := json.Unmarshal(frame.Body, &parsedFrame); err != nil {
					return simulator.RuntimeScenario{}, fmt.Errorf("scenario: stream frame %s: %w", frame.Kind, err)
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
	scenario := simulator.RuntimeScenario{Operations: operations}
	if s.Default == nil {
		return scenario, nil
	}
	step := simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.EffectKind(s.Default.Effect)}
	if err := reply(s.Default.Reply, &step.Reply); err != nil {
		return simulator.RuntimeScenario{}, fmt.Errorf("scenario: default reply: %w", err)
	}
	fixtures := map[string]string{}
	if s.ArtifactStore != nil {
		fixtures = s.ArtifactStore.Fixtures
	}
	rows, err := scriptRows(s.Default.Rows, 1, fixtures)
	if err != nil {
		return simulator.RuntimeScenario{}, err
	}
	step.Stream = &simulator.StreamScript{Frames: []contracts.StreamFrame{simulator.ConnectedFrame(time.Now().UTC()), simulator.RowsFrame(false, rows...)}}
	if len(s.Default.AfterDecision) > 0 {
		after, err := scriptRows(s.Default.AfterDecision, uint64(len(rows))+1, fixtures)
		if err != nil {
			return simulator.RuntimeScenario{}, err
		}
		step.AfterDecision = &simulator.StreamScript{Frames: []contracts.StreamFrame{simulator.RowsFrame(false, after...)}}
	}
	scenario.Default = &step
	scenario.DefaultDecision = &simulator.RuntimeOp{Reply: simulator.Accepted(nil), Effect: simulator.TurnSteered}
	return scenario, nil
}

func reply(raw json.RawMessage, into *simulator.Reply) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// scriptRows numbers rows from first. A result.proposed artifact that names
// a fixture without a digest gets the fixture's digest and size, so the
// scenario file never has to repeat a hash.
func scriptRows(rows []scriptRow, first uint64, fixtures map[string]string) ([]simulator.Row, error) {
	out := make([]simulator.Row, 0, len(rows))
	for i, row := range rows {
		var compact bytes.Buffer
		if err := json.Compact(&compact, row.Payload); err != nil {
			return nil, fmt.Errorf("scenario: %s payload: %w", row.Kind, err)
		}
		payload := json.RawMessage(compact.Bytes())
		if row.Kind == contracts.EntryResultProposed {
			var proposed contracts.ResultProposed
			if err := json.Unmarshal(payload, &proposed); err != nil {
				return nil, fmt.Errorf("scenario: result.proposed: %w", err)
			}
			for j, artifact := range proposed.Artifacts {
				data, ok := fixtures[artifact.NativeRef]
				if artifact.Digest == "" && ok {
					proposed.Artifacts[j].Digest, proposed.Artifacts[j].Size = identity.DigestOf([]byte(data)), uint64(len(data))
				}
			}
			var err error
			if payload, err = json.Marshal(proposed); err != nil {
				return nil, err
			}
		}
		sequence := first + uint64(i)
		out = append(out, simulator.Row{Sequence: sequence, Updated: sequence, Kind: row.Kind, Payload: string(payload)})
	}
	return out, nil
}

// tokenSource reads the token settings under prefix: a token file, or the
// client credentials of the registration's own client.
func tokenSource(prefix string, tlsConfig *tls.Config) (adapterkit.TokenSource, error) {
	if path := os.Getenv(prefix + "_TOKEN_FILE"); path != "" {
		return adapterkit.FileToken(path), nil
	}
	clientID, secretFile := os.Getenv(prefix+"_CLIENT_ID"), os.Getenv(prefix+"_CLIENT_SECRET_FILE")
	tokenURL := os.Getenv("DAC_ADAPTER_TOKEN_URL")
	if clientID == "" || secretFile == "" || tokenURL == "" {
		return nil, fmt.Errorf("%s_TOKEN_FILE, or DAC_ADAPTER_TOKEN_URL with %s_CLIENT_ID and %s_CLIENT_SECRET_FILE, is required", prefix, prefix, prefix)
	}
	var scopes []string
	if scope := os.Getenv("DAC_ADAPTER_SCOPE"); scope != "" {
		scopes = strings.Fields(scope)
	}
	return adapterkit.ClientCredentials(adapterkit.ClientCredentialsConfig{
		TokenURL: tokenURL, ClientID: clientID, SecretFile: secretFile, Scopes: scopes,
		HTTPClient: httpClient(tlsConfig),
	})
}

// httpClient reaches the token endpoint with the same trust as the adapter
// endpoint.
func httpClient(tlsConfig *tls.Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// trust loads the PEM certificates the environment names; empty means the
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
