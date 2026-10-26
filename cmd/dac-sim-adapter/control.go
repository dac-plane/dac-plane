package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/simulator"
)

const controlBodyLimit = 4 << 10

type pauseKey struct {
	role      contracts.Role
	operation ids.OperationID
	boundary  simulator.Boundary
}

type controlHandler struct {
	set    *simulatorSet
	mu     sync.Mutex
	pauses map[pauseKey]*simulator.Pause
}

func newControlHandler(set *simulatorSet) (*controlHandler, error) {
	if set == nil || set.runtime == nil {
		return nil, errors.New("simulator control requires a runtime")
	}
	return &controlHandler{set: set, pauses: map[pauseKey]*simulator.Pause{}}, nil
}

func serveControl(ctx context.Context, addr string, set *simulatorSet, logger *slog.Logger) error {
	handler, err := newControlHandler(set)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("simulator control listener: %w", err)
	}
	server := &http.Server{Handler: handler.routes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("simulator control serving", "address", addr)
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
		return fmt.Errorf("simulator control server: %w", err)
	}
}

func (h *controlHandler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pauses", h.registerPause)
	mux.HandleFunc("GET /v1/pauses/{role}/{operation}/{boundary}", h.pauseStatus)
	mux.HandleFunc("POST /v1/pauses/{role}/{operation}/{boundary}/release", h.releasePause)
	mux.HandleFunc("GET /v1/ledger/{role}/{operation}", h.ledger)
	mux.HandleFunc("GET /v1/artifacts/{artifact}", h.artifact)
	return mux
}

func (h *controlHandler) registerPause(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Role      contracts.Role     `json:"role"`
		Operation ids.OperationID    `json:"operation_id"`
		Boundary  simulator.Boundary `json:"boundary"`
	}
	limited := io.LimitReader(r.Body, controlBodyLimit+1)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Operation.IsZero() {
		controlError(w, http.StatusBadRequest, "malformed pause")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		controlError(w, http.StatusBadRequest, "malformed pause")
		return
	}
	key := pauseKey{role: request.Role, operation: request.Operation, boundary: request.Boundary}
	control, _, ok := h.endpoint(key.role)
	if !ok || (key.boundary != simulator.BeforeEffect && key.boundary != simulator.AfterEffect && key.boundary != simulator.BeforeFrame) {
		controlError(w, http.StatusBadRequest, "unknown role or boundary")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.pauses[key]; exists {
		controlError(w, http.StatusConflict, "pause already registered")
		return
	}
	h.pauses[key] = control.Pause(key.operation, key.boundary)
	controlJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

func (h *controlHandler) pauseStatus(w http.ResponseWriter, r *http.Request) {
	key, ok := parsePausePath(r)
	if !ok {
		controlError(w, http.StatusBadRequest, "malformed pause")
		return
	}
	h.mu.Lock()
	pause := h.pauses[key]
	h.mu.Unlock()
	if pause == nil {
		controlError(w, http.StatusNotFound, "pause not found")
		return
	}
	reached := false
	select {
	case <-pause.Reached():
		reached = true
	default:
	}
	controlJSON(w, http.StatusOK, map[string]bool{"reached": reached})
}

func (h *controlHandler) releasePause(w http.ResponseWriter, r *http.Request) {
	key, ok := parsePausePath(r)
	if !ok {
		controlError(w, http.StatusBadRequest, "malformed pause")
		return
	}
	h.mu.Lock()
	pause := h.pauses[key]
	if pause != nil {
		delete(h.pauses, key)
	}
	h.mu.Unlock()
	if pause == nil {
		controlError(w, http.StatusNotFound, "pause not found")
		return
	}
	pause.Release()
	controlJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

func (h *controlHandler) ledger(w http.ResponseWriter, r *http.Request) {
	role := contracts.Role(r.PathValue("role"))
	operation, err := ids.Parse[ids.Operation](r.PathValue("operation"))
	if err != nil {
		controlError(w, http.StatusBadRequest, "malformed operation")
		return
	}
	_, ledger, ok := h.endpoint(role)
	if !ok {
		controlError(w, http.StatusBadRequest, "unknown role")
		return
	}
	controlJSON(w, http.StatusOK, struct {
		Deliveries int `json:"deliveries"`
		Effects    int `json:"effects"`
		Cancels    int `json:"cancels"`
	}{ledger.Deliveries(operation), ledger.Effects(operation), ledger.Cancels(operation)})
}

func (h *controlHandler) artifact(w http.ResponseWriter, r *http.Request) {
	artifact, err := ids.Parse[ids.Artifact](r.PathValue("artifact"))
	if err != nil {
		controlError(w, http.StatusBadRequest, "malformed artifact")
		return
	}
	if h.set.store == nil {
		controlError(w, http.StatusNotFound, "artifact store unavailable")
		return
	}
	stored := h.set.store.Stored(artifact)
	controlJSON(w, http.StatusOK, struct {
		Present bool   `json:"present"`
		Digest  string `json:"digest,omitempty"`
		Size    uint64 `json:"size,omitempty"`
	}{stored.Present, string(stored.Digest), stored.Size})
}

func (h *controlHandler) endpoint(role contracts.Role) (*simulator.Control, *simulator.Ledger, bool) {
	switch role {
	case contracts.RoleRuntime:
		return h.set.runtime.Control(), h.set.runtime.Ledger(), true
	case contracts.RoleArtifactStore:
		if h.set.store != nil {
			return h.set.store.Control(), h.set.store.Ledger(), true
		}
	}
	return nil, nil, false
}

func parsePausePath(r *http.Request) (pauseKey, bool) {
	operation, err := ids.Parse[ids.Operation](r.PathValue("operation"))
	if err != nil {
		return pauseKey{}, false
	}
	key := pauseKey{role: contracts.Role(r.PathValue("role")), operation: operation, boundary: simulator.Boundary(r.PathValue("boundary"))}
	if key.boundary != simulator.BeforeEffect && key.boundary != simulator.AfterEffect && key.boundary != simulator.BeforeFrame {
		return pauseKey{}, false
	}
	return key, true
}

func controlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func controlError(w http.ResponseWriter, status int, message string) {
	controlJSON(w, status, map[string]string{"error": strings.TrimSpace(message)})
}
