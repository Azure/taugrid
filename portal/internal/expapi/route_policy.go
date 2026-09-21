// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"net/http"
	"strings"
)

const workspaceRouteReason = "this Stellar route is not workspace-scoped in managed Portal mode"

type capabilityRoute struct {
	path            string
	method          string
	workspaceScoped bool
}

var capabilityRoutes = map[string]capabilityRoute{
	"snapshot":             {"/snapshot", http.MethodGet, true},
	"series_detail":        {"/series", http.MethodGet, true},
	"run_search":           {"/runs", http.MethodGet, true},
	"experiment_search":    {"/experiments", http.MethodGet, true},
	"experiment_mutation":  {"/experiments", http.MethodPost, false},
	"artifact_index":       {"/artifacts", http.MethodGet, true},
	"artifact_content":     {"/artifact", http.MethodGet, true},
	"status":               {"/status", http.MethodGet, false},
	"v2_experiment_search": {"/experiments/search", http.MethodGet, true},
	"v2_run_listing":       {"/experiments/_/runs", http.MethodGet, true},
	"v2_run_detail":        {"/runs/_", http.MethodGet, true},
	"v2_metric_catalog":    {"/runs/_/metrics", http.MethodGet, true},
	"v2_exact_series":      {"/runs/_/series", http.MethodGet, true},
}

// WorkspaceRouteAllowed is the shared managed Portal allowlist. Unknown routes
// fail closed, including any new capability not explicitly scoped here.
func WorkspaceRouteAllowed(method, path string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method != http.MethodGet {
		return false
	}
	if path == "/stellar" || path == "/stellar/" || strings.HasPrefix(path, "/stellar/assets/") {
		return true
	}
	if method == http.MethodGet && workspaceScopedV2Route(path) {
		return true
	}
	if path == stellarAPIV2Base+"/capabilities" {
		return true
	}
	for _, route := range capabilityRoutes {
		if route.workspaceScoped && method == route.method && path == stellarAPIV2Base+route.path {
			return true
		}
	}
	return false
}

func workspaceScopedV2Route(path string) bool {
	if path == stellarAPIV2Base+"/experiments/search" {
		return true
	}
	if strings.HasPrefix(path, stellarAPIV2Base+"/experiments/") {
		parts := strings.Split(strings.TrimPrefix(path, stellarAPIV2Base+"/experiments/"), "/")
		return len(parts) == 2 && parts[0] != "" && parts[1] == "runs"
	}
	if strings.HasPrefix(path, stellarAPIV2Base+"/runs/") {
		parts := strings.Split(strings.TrimPrefix(path, stellarAPIV2Base+"/runs/"), "/")
		return len(parts) == 1 && parts[0] != "" ||
			len(parts) == 2 && parts[0] != "" && (parts[1] == "metrics" || parts[1] == "series")
	}
	return false
}

type workspaceRoutePolicyKey struct{}

// WithWorkspaceRoutePolicy marks an already-authorized request at the Portal
// mount boundary. This is intentionally not a client-controlled query/header.
func WithWorkspaceRoutePolicy(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), workspaceRoutePolicyKey{}, true))
}

func (c *capabilitiesResponse) applyRoutePolicy(r *http.Request) {
	if restricted, _ := r.Context().Value(workspaceRoutePolicyKey{}).(bool); !restricted {
		return
	}
	for name, capability := range c.Capabilities {
		route, known := capabilityRoutes[name]
		if known && WorkspaceRouteAllowed(route.method, stellarAPIV2Base+route.path) {
			continue
		}
		for key, value := range capability {
			if _, ok := value.(bool); ok {
				capability[key] = false
			}
		}
		capability["supported"] = false
		capability["reason"] = workspaceRouteReason
	}
	c.Degradations = append(c.Degradations, capabilityDegradation{
		Code:   "MANAGED_PORTAL_ROUTE_POLICY",
		Detail: workspaceRouteReason + "; mutations, artifact bundles, and status are unavailable; artifact reads require an exact run target.",
	})
}
