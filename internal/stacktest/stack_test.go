//go:build stack

package stacktest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/httpapi"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

const (
	tenantID   = "01a167fd-51d9-76ba-b563-496e2bd89646"
	providerID = "01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10"
	apiRoot    = "https://dac-plane:8443/v1/tenants/" + tenantID
	tokenURL   = "https://issuer:8443/default/token"
)

func TestDevelopmentStackLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	client := stackClient(t)
	token := clientCredentialsToken(t, ctx, client)

	submitted := request[httpapi.SubmissionBody](t, ctx, client, token, http.MethodPost, "/tasks", map[string]string{
		"Idempotency-Key": "stack-submit-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	}, httpapi.SubmitTaskRequest{
		Provider: mustProviderID(t),
		Demand:   contracts.DemandPerson,
		Deadline: time.Now().UTC().Add(time.Hour),
		Unit:     "tokens",
		Ceiling:  1000,
		Amount:   100,
	}, http.StatusCreated)

	attention := poll(t, ctx, "attention request", func() (lifecycle.AttentionRequest, bool) {
		list := request[httpapi.AttentionListBody](t, ctx, client, token, http.MethodGet, "/attention", nil, nil, http.StatusOK)
		if len(list.value.Attention) == 0 {
			return lifecycle.AttentionRequest{}, false
		}
		return list.value.Attention[0], true
	})

	request[httpapi.DecisionBody](t, ctx, client, token, http.MethodPost, "/attention/"+attention.ID.String()+"/decisions", map[string]string{
		"Idempotency-Key": "stack-decision-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"If-Match":        strconv.Quote(strconv.FormatUint(attention.Version, 10)),
	}, httpapi.DecisionRequest{
		Kind:         lifecycle.DecisionAnswer,
		AnswerDigest: "sha256:da5f2e58d7277762a4907c15c2cba4a079b259f959e947966b9df6c68b23f267",
	}, http.StatusCreated)

	taskPath := "/tasks/" + submitted.value.Task.ID.String()
	ready := poll(t, ctx, "result revision", func() (responseValue[httpapi.TaskBody], bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, taskPath, nil, nil, http.StatusOK)
		return view, view.value.Task.State == lifecycle.TaskReadyForReview && len(view.value.Revisions) == 1
	})
	if len(ready.value.Revisions[0].Revision.Artifacts) != 1 || len(ready.value.Revisions[0].Revision.Verifications) != 1 {
		t.Fatalf("revision has %d artifacts and %d verifications, want one of each", len(ready.value.Revisions[0].Revision.Artifacts), len(ready.value.Revisions[0].Revision.Verifications))
	}

	revision := ready.value.Revisions[0].Revision
	request[httpapi.ReviewBody](t, ctx, client, token, http.MethodPost, taskPath+"/reviews", map[string]string{
		"Idempotency-Key": "stack-review-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"If-Match":        ready.header.Get("ETag"),
	}, httpapi.ReviewRequest{Revision: revision.ID, Disposition: lifecycle.DispositionAccept}, http.StatusCreated)

	succeeded := poll(t, ctx, "successful task", func() (httpapi.TaskBody, bool) {
		view := request[httpapi.TaskBody](t, ctx, client, token, http.MethodGet, taskPath, nil, nil, http.StatusOK).value
		return view, view.Task.State == lifecycle.TaskSucceeded
	})
	if len(succeeded.Revisions) != 1 || len(succeeded.Revisions[0].Reviews) != 1 {
		t.Fatalf("succeeded task has %d revisions and %d reviews", len(succeeded.Revisions), len(succeeded.Revisions[0].Reviews))
	}
}

type responseValue[T any] struct {
	value  T
	header http.Header
}

func request[T any](t *testing.T, ctx context.Context, client *http.Client, token, method, path string, headers map[string]string, body any, want int) responseValue[T] {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiRoot+path, input)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, want, data)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return responseValue[T]{value: value, header: res.Header}
}

func poll[T any](t *testing.T, ctx context.Context, description string, load func() (T, bool)) T {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, done := load()
		if done {
			return value
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func clientCredentialsToken(t *testing.T, ctx context.Context, client *http.Client) string {
	t.Helper()
	secretPath := os.Getenv("DAC_STACK_SURFACE_CLIENT_SECRET_FILE")
	if secretPath == "" {
		t.Fatal("DAC_STACK_SURFACE_CLIENT_SECRET_FILE is required")
	}
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"dac-plane-surface"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("surface-owner", strings.TrimSpace(string(secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint = %d: %s", res.StatusCode, data)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" {
		t.Fatal("token endpoint returned no access_token")
	}
	return payload.AccessToken
}

func stackClient(t *testing.T) *http.Client {
	t.Helper()
	caPath := os.Getenv("DAC_CA_FILE")
	if caPath == "" {
		t.Fatal("DAC_CA_FILE is required")
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("development CA contains no certificate")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			mapped := map[string]string{"issuer:8443": "127.0.0.1:18080", "dac-plane:8443": "127.0.0.1:18443"}[address]
			if mapped == "" {
				return nil, fmt.Errorf("stack test refuses unexpected address %q", address)
			}
			return dialer.DialContext(ctx, network, mapped)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func mustProviderID(t *testing.T) ids.ProviderID {
	t.Helper()
	id, err := ids.Parse[ids.Provider](providerID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
