// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expapi"
)

func TestManagedStellarCapabilitiesMatchRoutePolicy(t *testing.T) {
	stellar, err := expapi.NewServer(expapi.Options{StorePath: t.TempDir(), Workspace: "alpha", Source: "auto", KustoMetricsFile: "configured.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceDirectory: testWorkspaceDirectory(t), identity: normalizeIdentityOptions(IdentityOptions{})}
	for _, base := range []string{"/api/stellar", "/api/v1/stellar", "/api/v2/stellar"} {
		req := httptest.NewRequest(http.MethodGet, base+"/capabilities?workspace=alpha", nil)
		req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
		req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
		rec := httptest.NewRecorder()
		server.workspaceAwareStellar(stellar.Handler()).ServeHTTP(rec, req)
		var result struct {
			Capabilities map[string]map[string]any `json:"capabilities"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", base, rec.Code, rec.Body.String())
		}
		for _, capability := range []string{"experiment_mutation", "artifact_index", "artifact_content", "status"} {
			for key, value := range result.Capabilities[capability] {
				if value == true {
					t.Errorf("%s advertises forbidden %s.%s", base, capability, key)
				}
			}
			if result.Capabilities[capability]["reason"] == nil {
				t.Errorf("%s lacks unsupported reason for %s", base, capability)
			}
		}
		if result.Capabilities["run_search"]["kusto"] != true {
			t.Errorf("allowed run search lost capability: %s", rec.Body.String())
		}
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/experiments"},
			{http.MethodGet, "/artifacts"},
			{http.MethodGet, "/artifact?ref=az://container/file"},
			{http.MethodGet, "/status"},
		} {
			req := httptest.NewRequest(route.method, base+route.path, nil)
			req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
			req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
			rec := httptest.NewRecorder()
			server.workspaceAwareStellar(stellar.Handler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("blocked %s %s returned %d", route.method, route.path, rec.Code)
			}
		}
	}
	// Applying the request-local policy must not mutate standalone capabilities.
	rec := httptest.NewRecorder()
	stellar.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/capabilities?managed=true", nil))
	var direct struct {
		Capabilities map[string]map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &direct); err != nil {
		t.Fatal(err)
	}
	if direct.Capabilities["artifact_content"]["durable_ref"] != true || direct.Capabilities["experiment_mutation"]["local"] != true {
		t.Fatalf("managed request changed standalone policy: %s", rec.Body.String())
	}
	for _, path := range []string{"/api/stellar/capabilities", "/api/stellar/capabilities?workspace=beta"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path != "/api/stellar/capabilities" {
			req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
			req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
		}
		server.workspaceAwareStellar(stellar.Handler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
			t.Errorf("unauthorized capabilities returned %d: %s", rec.Code, rec.Body.String())
		}
	}
}
