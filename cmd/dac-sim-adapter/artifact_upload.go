package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/adapterkit/wire"
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

// artifactUploadHandler adapts the simulator's opaque in-process upload
// capability to a short-lived HTTPS capability for out-of-process runtime
// qualification. It is test infrastructure, not a production artifact store.
type artifactUploadHandler struct {
	store   *simulator.ByteArtifactStore
	public  string
	mu      sync.Mutex
	pending map[string]contracts.ArtifactTransfer
}

func configureArtifactOnly(set *simulatorSet, getenv func(string) string) (*artifactUploadHandler, error) {
	if set == nil || set.store == nil {
		return nil, errors.New("artifact-only mode requires scenario.artifact_store")
	}
	addr := strings.TrimSpace(getenv("DAC_SIM_ARTIFACT_ADDR"))
	public := strings.TrimRight(strings.TrimSpace(getenv("DAC_SIM_ARTIFACT_PUBLIC_URL")), "/")
	cert, key := strings.TrimSpace(getenv("DAC_SIM_ARTIFACT_CERT_FILE")), strings.TrimSpace(getenv("DAC_SIM_ARTIFACT_KEY_FILE"))
	if addr == "" || public == "" || cert == "" || key == "" {
		return nil, errors.New("artifact-only mode requires upload address, public URL, certificate and key")
	}
	u, err := url.Parse(public)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("artifact upload public URL must be a plain HTTPS origin")
	}
	h := &artifactUploadHandler{store: set.store, public: public, pending: map[string]contracts.ArtifactTransfer{}}
	var artifact adapter
	for _, candidate := range set.adapters {
		if candidate.role == wire.RoleArtifactStore {
			artifact = candidate
			break
		}
	}
	if artifact.port == nil {
		return nil, errors.New("artifact-only mode found no artifact adapter")
	}
	artifact.port = h
	set.adapters = []adapter{artifact}
	set.runtime = nil
	return h, nil
}

func (h *artifactUploadHandler) Role() contracts.Role { return contracts.RoleArtifactStore }
func (h *artifactUploadHandler) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	return h.store.Cancel(ctx, cancel)
}
func (h *artifactUploadHandler) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	return h.store.Subscribe(ctx, cursor)
}
func (h *artifactUploadHandler) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	reply, err := h.store.Send(ctx, request)
	if err != nil || request.Method != contracts.MethodAuthorizeUpload || reply.Outcome != contracts.OutcomeOK {
		return reply, err
	}
	var transfer contracts.ArtifactTransfer
	var authorized contracts.UploadAuthorized
	if json.Unmarshal(request.Arguments, &transfer) != nil || json.Unmarshal(reply.Result, &authorized) != nil || transfer.Validate() != nil || authorized.Validate() != nil {
		return contracts.Reply{}, contracts.ErrInvalid
	}
	transfer.Upload = authorized.Upload
	h.mu.Lock()
	h.pending[authorized.Upload] = transfer
	h.mu.Unlock()
	authorized.Upload = h.public + "/v1/uploads/" + url.PathEscape(authorized.Upload)
	reply.Result, err = json.Marshal(authorized)
	return reply, err
}

func (h *artifactUploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/evidence/") {
		h.evidence(w, r)
		return
	}
	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/v1/uploads/") {
		http.NotFound(w, r)
		return
	}
	capability, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/uploads/"))
	if err != nil || capability == "" {
		http.Error(w, "invalid capability", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	transfer, ok := h.pending[capability]
	h.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(transfer.Manifest.Size)+1))
	if err != nil || uint64(len(data)) != transfer.Manifest.Size {
		http.Error(w, "artifact size mismatch", http.StatusBadRequest)
		return
	}
	if existing, present := h.store.Bytes(transfer.Manifest.Artifact); present && bytes.Equal(existing, data) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.Upload(transfer, data); err != nil {
		http.Error(w, "artifact rejected", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *artifactUploadHandler) evidence(w http.ResponseWriter, r *http.Request) {
	artifact, err := ids.Parse[ids.Artifact](strings.TrimPrefix(r.URL.Path, "/v1/evidence/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, ok := h.store.Bytes(artifact)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func serveArtifactUploads(ctx context.Context, addr, certFile, keyFile string, handler http.Handler, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("artifact upload listener: %w", err)
	}
	server := &http.Server{Handler: handler, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, certFile, keyFile) }()
	logger.Info("artifact upload fixture serving", "address", addr)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		err = <-done
		if errors.Is(err, http.ErrServerClosed) {
			return ctx.Err()
		}
		return err
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("artifact upload server: %w", err)
	}
}
