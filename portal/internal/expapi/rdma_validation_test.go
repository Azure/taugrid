// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestRDMAValidationArtifactFetchRequiresWorkspaceScopedRun(t *testing.T) {
	result := readRDMAAPIGolden(t)
	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(context.Background(), root, expstore.InitOptions{
		Name: result.ExperimentID, Project: result.ProjectID, Group: result.RunGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactURI := filepath.ToSlash(filepath.Join("artifacts", result.ValidationID+".json"))
	artifactPath := filepath.Join(root, filepath.FromSlash(artifactURI))
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	link := rdmavalidation.ArtifactLink{
		URI:         artifactURI,
		SHA256:      "sha256:" + hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	projection, err := expstore.ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunData(context.Background(), expstore.RecordRunDataOptions{
		Run: projection.Run, Tags: projection.Tags,
		Artifacts:      []expstore.ArtifactRecord{*projection.Artifact},
		IdempotencyKey: projection.IdempotencyKey,
		Command:        "rdma validation artifact fixture",
		RequestHash:    projection.RequestHash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := NewServer(Options{StorePath: root, Workspace: result.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(
		http.MethodGet,
		"/api/stellar/artifacts?run="+result.RunID+"&type="+rdmavalidation.ArtifactType,
		nil,
	))
	if list.Code != http.StatusOK {
		t.Fatalf("artifact list status = %d, body=%s", list.Code, list.Body.String())
	}
	var response struct {
		Count     int                 `json:"count"`
		Artifacts []apiArtifactRecord `json:"artifacts"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Count != 1 || len(response.Artifacts) != 1 {
		t.Fatalf("artifact list = %+v", response)
	}
	artifact := response.Artifacts[0]
	if artifact.ArtifactID != result.WorkspaceID+"-"+result.ValidationID+"-attempt-1-result" ||
		artifact.Type != rdmavalidation.ArtifactType ||
		artifact.Digest != link.SHA256 ||
		artifact.URI != artifactURI ||
		artifact.FetchURL == "" {
		t.Fatalf("artifact contract = %+v", artifact)
	}
	fetch := httptest.NewRecorder()
	server.Handler().ServeHTTP(fetch, httptest.NewRequest(http.MethodGet, artifact.FetchURL, nil))
	if fetch.Code != http.StatusOK || fetch.Body.String() != string(raw) {
		t.Fatalf("artifact fetch status = %d, body=%q", fetch.Code, fetch.Body.String())
	}

	otherWorkspace, err := NewServer(Options{StorePath: root, Workspace: "research"})
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	otherWorkspace.Handler().ServeHTTP(denied, httptest.NewRequest(http.MethodGet, artifact.FetchURL, nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace artifact fetch status = %d, want 404; body=%s", denied.Code, denied.Body.String())
	}
}

func readRDMAAPIGolden(t *testing.T) rdmavalidation.Result {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "core", "rdmavalidation", "testdata", "pass.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result rdmavalidation.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if err := rdmavalidation.Validate(result); err != nil {
		t.Fatal(err)
	}
	return result
}
