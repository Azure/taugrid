// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type quotaReader struct {
	clusterQueueName string
	cohortName       string
	localNamespace   string
	localQueueName   string
}

func (r *quotaReader) GetLocalQueue(_ context.Context, namespace, name string) ([]byte, error) {
	r.localNamespace = namespace
	r.localQueueName = name
	return []byte(`{"spec":{"clusterQueue":"tau-cq"}}`), nil
}
func (r *quotaReader) GetClusterQueue(_ context.Context, name string) ([]byte, error) {
	r.clusterQueueName = name
	return []byte(`{
		"metadata":{"name":"tau-ws-vision"},
		"spec":{"cohortName":"tau-team-research","resourceGroups":[]},
		"status":{"pendingWorkloads":1,"reservingWorkloads":2,"admittedWorkloads":2}
	}`), nil
}
func (r *quotaReader) GetCohort(_ context.Context, name string) ([]byte, error) {
	r.cohortName = name
	return []byte(`{"metadata":{"name":"tau-team-research"},"spec":{"resourceGroups":[]}}`), nil
}

func TestQuotaEndpointUsesResolvedWorkspaceScope(t *testing.T) {
	server := newTestServer(t)
	reader := &quotaReader{}
	server.quota.Reader = reader
	server.singleWorkspaceScope = WorkspaceScope{
		WorkspaceID: "vision", Team: "research", Namespace: "vision", LocalQueue: "jobqueue",
		AuthorizationMode: workspaceAuthorizationClusterWide,
		Availability:      workspaceAvailabilityAvailable,
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if reader.localNamespace != "vision" || reader.localQueueName != "jobqueue" ||
		reader.clusterQueueName != "tau-cq" || reader.cohortName != "tau-team-research" {
		t.Fatalf(
			"reads = LocalQueue %q/%q, ClusterQueue %q, Cohort %q",
			reader.localNamespace,
			reader.localQueueName,
			reader.clusterQueueName,
			reader.cohortName,
		)
	}
	var body struct {
		Scope     WorkspaceScope `json:"scope"`
		Workspace struct {
			Name string `json:"name"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Scope.WorkspaceID != "vision" || body.Workspace.Name != "tau-ws-vision" {
		t.Fatalf("body = %+v", body)
	}
}

func TestQuotaEndpointIsReadOnly(t *testing.T) {
	server := newTestServer(t)
	server.quota.Reader = &quotaReader{}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/portal/quota", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaEndpointFailsClosedBeforeKubernetesRead(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "vision", Cluster: "cluster-a", Team: "research", Namespace: "vision", LocalQueue: "jobqueue",
			Source:        "kubernetes",
			Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	reader := &quotaReader{}
	server.workspaceDirectory = directory
	server.quota.Reader = reader

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/quota?workspace=vision", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if reader.clusterQueueName != "" || reader.cohortName != "" {
		t.Fatalf("unauthorized request reached Kubernetes: ClusterQueue %q, Cohort %q", reader.clusterQueueName, reader.cohortName)
	}
}
