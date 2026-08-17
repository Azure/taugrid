// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	podsStatus      string
	lastReconciled  string
}

func (h *Health) MarkSuccess(result Result, observedAt string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = true
	h.rayJobsStatus = result.RayJobsStatus
	h.workloadsStatus = result.WorkloadsStatus
	h.podsStatus = result.PodsStatus
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
		podsStatus := h.podsStatus
		last := h.lastReconciled
		h.mu.RUnlock()
		if !ready {
			http.Error(w, "first reconcile has not succeeded", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// pods_status is operator-facing, not decorative: when it is not
		// "available" every batch-Job failure summary silently degrades to the
		// Job's own "BackoffLimitExceeded" with no other signal. The usual
		// cause is a Role without the pods read verb. Runbooks direct
		// operators here first, so it has to be readable from outside the
		// process.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"rayjobs_status":   status,
			"workloads_status": workloadsStatus,
			"pods_status":      podsStatus,
			"last_reconciled":  last,
		})
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
