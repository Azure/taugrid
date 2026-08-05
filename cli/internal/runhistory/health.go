package runhistory

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
)

type Health struct {
	mu              sync.RWMutex
	ready           bool
	rayJobsStatus   string
	workloadsStatus string
	lastReconciled  string
}

func (h *Health) MarkSuccess(result Result, observedAt string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = true
	h.rayJobsStatus = result.RayJobsStatus
	h.workloadsStatus = result.WorkloadsStatus
	h.lastReconciled = observedAt
}

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.RLock()
		ready := h.ready
		status := h.rayJobsStatus
		workloadsStatus := h.workloadsStatus
		last := h.lastReconciled
		h.mu.RUnlock()
		if !ready {
			http.Error(w, "first reconcile has not succeeded", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"rayjobs_status": status, "workloads_status": workloadsStatus, "last_reconciled": last})
	})
	return mux
}

func StartHealthServer(ctx context.Context, address string, health *Health) (<-chan error, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: health.Handler()}
	errors := make(chan error, 1)
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errors <- err
		}
		close(errors)
	}()
	return errors, nil
}
