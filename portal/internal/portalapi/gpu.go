// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/taugrid/portal/internal/portal/gpuinventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const defaultGPUInventoryTimeout = 10 * time.Second

func (s *Server) handleWorkspaceGPUWorkloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	for _, key := range []string{"namespace", "queue", "team", "cluster"} {
		if r.URL.Query().Has(key) {
			writeJSONError(w, http.StatusBadRequest, key+" query is not accepted; workspace scope is server-resolved")
			return
		}
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(scope.Namespace) == "" || strings.TrimSpace(scope.LocalQueue) == "" {
		writeScopedJSON(w, http.StatusServiceUnavailable, map[string]string{
			"reason": "workspace GPU workloads require a resolved namespace and LocalQueue",
		}, scope, "setup_required")
		return
	}
	if s.gpu.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "workspace GPU workloads unavailable: portal started without Kubernetes access")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.gpuInventoryTimeout())
	defer cancel()
	snapshot, err := gpuinventory.Inventory(ctx, s.gpu.Reader, gpuinventory.Options{
		Namespace:    scope.Namespace,
		LocalQueue:   scope.LocalQueue,
		ManagedOnly:  true,
		MaxWorkloads: s.gpu.MaxWorkloads,
	})
	if err != nil {
		if apierrors.IsForbidden(err) {
			writeScopedJSON(w, http.StatusForbidden, map[string]string{
				"reason": "workspace GPU workload inventory is not authorized to list Pods",
			}, scope, "forbidden")
			return
		}
		writeScopedError(w, http.StatusBadGateway, scope, "workspace GPU workload inventory could not be read")
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Workloads) == 0))
}

func (s *Server) handleGPUConsumers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.gpu.FleetEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"state":  "disabled",
			"reason": "fleet GPU consumers are disabled; enable the trusted authenticated-proxy deployment option",
		})
		return
	}
	if s.gpu.Reader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"state":  "unavailable",
			"reason": "fleet GPU consumers are unavailable because Kubernetes access is not configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.gpuInventoryTimeout())
	defer cancel()
	snapshot, err := gpuinventory.Inventory(ctx, s.gpu.Reader, gpuinventory.Options{
		MaxWorkloads: s.gpu.MaxWorkloads,
	})
	if err != nil {
		if apierrors.IsForbidden(err) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"state":  "forbidden",
				"reason": "fleet GPU consumer inventory is not authorized to list Pods",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"state":  "unavailable",
			"reason": "fleet GPU consumer inventory could not be read",
		})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) gpuInventoryTimeout() time.Duration {
	timeout := s.gpu.RequestTimeout
	if timeout <= 0 {
		return defaultGPUInventoryTimeout
	}
	if timeout > 30*time.Second {
		return 30 * time.Second
	}
	return timeout
}
