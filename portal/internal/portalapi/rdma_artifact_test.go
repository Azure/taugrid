// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/portal/internal/expapi"
	portalvalidation "github.com/Azure/taugrid/portal/internal/portal/rdmavalidation"
)

func TestFetchRDMAArtifactUsesWorkspaceScopedStellarFetchURL(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "core", "rdmavalidation", "testdata", "pass.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result corevalidation.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	hash := sha256Digest(raw)
	artifactRequests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			if r.URL.Query().Get("workspace") != result.WorkspaceID ||
				r.URL.Query().Get("target") != result.RunID {
				t.Fatalf("snapshot query = %s", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"target": result.RunID, "target_type": "run",
				"runs": []map[string]any{{
					"run_id": result.RunID, "workspace_id": result.WorkspaceID,
				}},
				"artifacts": []map[string]any{{
					"artifact_id": result.ValidationID + "-result",
					"run_id":      result.RunID, "type": corevalidation.ArtifactType,
					"uri":          "az://results/" + result.ValidationID + ".json",
					"name":         result.ValidationID + ".json",
					"content_type": corevalidation.ArtifactContentType,
					"digest":       hash, "size_bytes": strconv.Itoa(len(raw)),
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/artifact"):
			artifactRequests++
			if r.URL.Query().Get("workspace") != result.WorkspaceID ||
				r.URL.Query().Get("target") != result.RunID ||
				r.URL.Query().Get("artifact") != result.ValidationID+"-result" {
				t.Fatalf("artifact query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", corevalidation.ArtifactContentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: result.Cluster,
		Workspaces: []WorkspaceRecord{{
			ID: result.WorkspaceID, Cluster: result.Cluster, Namespace: "workspace-namespace",
			Source: "kusto", ExperimentsBackend: &ExperimentsBackend{URL: backend.URL},
			Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"viewer@example.com"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto", Workspace: result.WorkspaceID},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := directory.Resolve(context.Background(), Viewer{ID: "viewer@example.com"}, result.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations/"+result.ValidationID+"?workspace="+result.WorkspaceID, nil)
	request.Header.Set(defaultViewerUserHeader, "viewer@example.com")
	content, metadata, err := server.fetchRDMAArtifact(request.Context(), request, scope, portalvalidation.ArtifactMetadata{
		ValidationID: result.ValidationID, RunID: result.RunID, WorkspaceID: result.WorkspaceID,
		URI: "az://results/" + result.ValidationID + ".json", SHA256: hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(raw) || metadata.ContentType != corevalidation.ArtifactContentType ||
		metadata.SizeBytes != int64(len(raw)) || artifactRequests != 1 {
		t.Fatalf("metadata=%+v artifactRequests=%d", metadata, artifactRequests)
	}
}

func TestFetchRDMAArtifactRejectsUntrustedListingBeforeContentFetch(t *testing.T) {
	raw := []byte(`{"schema":"rdma-validation.v1"}`)
	artifactRequests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/snapshot") {
			writeJSON(w, http.StatusOK, map[string]any{
				"target": "run-a", "target_type": "run",
				"runs": []map[string]any{{"run_id": "run-a", "workspace_id": "research"}},
				"artifacts": []map[string]any{{
					"artifact_id": "validation-a-result", "run_id": "run-a",
					"type": corevalidation.ArtifactType, "uri": "az://wrong/result.json",
					"name": "validation-a.json", "content_type": corevalidation.ArtifactContentType,
					"digest": sha256Digest(raw), "size_bytes": strconv.Itoa(len(raw)),
				}},
			})
			return
		}
		artifactRequests++
		http.Error(w, "must not fetch", http.StatusInternalServerError)
	}))
	defer backend.Close()
	server, scope := newRemoteRDMAArtifactTestServer(t, backend.URL)
	request := httptest.NewRequest(http.MethodGet, "/api/portal/rdma-validations/validation-a?workspace=research", nil)
	request.Header.Set(defaultViewerUserHeader, "viewer@example.com")
	_, _, err := server.fetchRDMAArtifact(request.Context(), request, scope, portalvalidation.ArtifactMetadata{
		ValidationID: "validation-a", RunID: "run-a", WorkspaceID: "research",
		URI: "az://expected/result.json", SHA256: sha256Digest(raw),
	})
	if !errors.Is(err, portalvalidation.ErrArtifactIntegrity) || artifactRequests != 0 {
		t.Fatalf("error=%v artifactRequests=%d", err, artifactRequests)
	}
}

func TestValidatedRDMAFetchURLRejectsExternalOrMismatchedTargets(t *testing.T) {
	scope := WorkspaceScope{WorkspaceID: "research", Source: "kusto"}
	for _, raw := range []string{
		"https://attacker.example/artifact?target=run-a&artifact=validation-a-result",
		"/api/v2/stellar/artifact?target=run-b&artifact=validation-a-result",
		"/api/v2/stellar/artifact?target=run-a&artifact=other",
		"/api/v2/stellar/snapshot?target=run-a&artifact=validation-a-result",
	} {
		if _, err := validatedRDMAFetchURL(raw, scope, "run-a", "validation-a-result"); !errors.Is(err, portalvalidation.ErrArtifactIntegrity) {
			t.Fatalf("validatedRDMAFetchURL(%q) error = %v", raw, err)
		}
	}
}

func newRemoteRDMAArtifactTestServer(t *testing.T, backendURL string) (*Server, WorkspaceScope) {
	t.Helper()
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "research", Cluster: "cluster-a", Namespace: "workspace-namespace",
			Source: "kusto", ExperimentsBackend: &ExperimentsBackend{URL: backendURL},
			Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Users: []string{"viewer@example.com"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto", Workspace: "research"},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := directory.Resolve(context.Background(), Viewer{ID: "viewer@example.com"}, "research")
	if err != nil {
		t.Fatal(err)
	}
	return server, scope
}

func sha256Digest(raw []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}
