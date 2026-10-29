package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

func TestArtifactUploadHandlerTransfersExactBytes(t *testing.T) {
	tenant := fixtureID[ids.Tenant](t, "01a18305-0000-7000-8000-000000000001")
	storeID := fixtureID[ids.Provider](t, "01a18305-0000-7000-8000-000000000002")
	producer := fixtureID[ids.Provider](t, "01a18305-0000-7000-8000-000000000003")
	attempt := fixtureID[ids.Attempt](t, "01a18305-0000-7000-8000-000000000004")
	data := []byte("qualified\n")
	manifest := lifecycle.ArtifactManifest{
		Artifact: fixtureID[ids.Artifact](t, "01a18305-0000-7000-8000-000000000005"), Tenant: tenant,
		Task: fixtureID[ids.Task](t, "01a18305-0000-7000-8000-000000000006"), Attempt: attempt,
		Operation: fixtureID[ids.Operation](t, "01a18305-0000-7000-8000-000000000007"), Producer: producer,
		Kind: lifecycle.ArtifactCodeChange, Digest: identity.DigestOf(data), Size: uint64(len(data)),
		MediaType: "text/plain", SourceRevision: "codex-turn:fixture", WriterEpoch: 1,
	}
	store, err := simulator.NewByteArtifactStore(simulator.Config{Tenant: tenant, Provider: storeID})
	if err != nil {
		t.Fatal(err)
	}
	handler := &artifactUploadHandler{store: store, pending: map[string]contracts.ArtifactTransfer{}}
	server := httptest.NewServer(handler)
	defer server.Close()
	handler.public = server.URL
	transfer := contracts.ArtifactTransfer{Manifest: manifest, NativeRef: "result.txt"}
	arguments, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.Request{
		Operation: fixtureID[ids.Operation](t, "01a18305-0000-7000-8000-000000000008"),
		Delivery:  fixtureID[ids.Delivery](t, "01a18305-0000-7000-8000-000000000009"),
		Tenant:    tenant, Attempt: attempt, Role: contracts.RoleArtifactStore,
		Method: contracts.MethodAuthorizeUpload, Arguments: arguments,
		Demand: contracts.DemandBackground, Deadline: time.Now().Add(time.Minute),
	}
	reply, err := handler.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var authorized contracts.UploadAuthorized
	if err = json.Unmarshal(reply.Result, &authorized); err != nil {
		t.Fatal(err)
	}
	upload, err := http.NewRequestWithContext(context.Background(), http.MethodPut, authorized.Upload, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status=%d", response.StatusCode)
	}
	replay, err := http.NewRequestWithContext(context.Background(), http.MethodPut, authorized.Upload, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := http.DefaultClient.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	_ = replayed.Body.Close()
	if replayed.StatusCode != http.StatusNoContent {
		t.Fatalf("replayed upload status=%d", replayed.StatusCode)
	}
	stored := store.Stored(manifest.Artifact)
	if !stored.Present || stored.Digest != manifest.Digest || stored.Size != manifest.Size {
		t.Fatalf("stored=%+v", stored)
	}
	evidence, err := http.Get(server.URL + "/v1/evidence/" + manifest.Artifact.String())
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Body.Close()
	got, err := io.ReadAll(evidence.Body)
	if err != nil || evidence.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("evidence status=%d bytes=%q err=%v", evidence.StatusCode, got, err)
	}
}

func fixtureID[T any](t *testing.T, raw string) ids.ID[T] {
	t.Helper()
	id, err := ids.Parse[T](raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
