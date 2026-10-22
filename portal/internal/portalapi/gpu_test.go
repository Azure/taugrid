// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/gpuinventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type gpuAPIReader struct {
	mu            sync.Mutex
	podsNamespace []string
	pods          []byte
	jobs          []byte
	workloads     []byte
	podsErr       error
	rayJobsErr    error
}

func (r *gpuAPIReader) recordPods(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.podsNamespace = append(r.podsNamespace, namespace)
}

func (r *gpuAPIReader) ListPods(_ context.Context, namespace string) ([]byte, error) {
	r.recordPods(namespace)
	if r.podsErr != nil {
		return nil, r.podsErr
	}
	return defaultList(r.pods), nil
}

func (r *gpuAPIReader) ListJobs(context.Context, string) ([]byte, error) {
	return defaultList(r.jobs), nil
}

func (r *gpuAPIReader) ListRayJobs(context.Context, string) ([]byte, error) {
	if r.rayJobsErr != nil {
		return nil, r.rayJobsErr
	}
	return emptyList(), nil
}

func (*gpuAPIReader) ListRayServices(context.Context, string) ([]byte, error) {
	return emptyList(), nil
}

func (*gpuAPIReader) ListRayClusters(context.Context, string) ([]byte, error) {
	return emptyList(), nil
}

func (*gpuAPIReader) ListReplicaSets(context.Context, string) ([]byte, error) {
	return emptyList(), nil
}

func (*gpuAPIReader) ListDeployments(context.Context, string) ([]byte, error) {
	return emptyList(), nil
}

func (*gpuAPIReader) ListStatefulSets(context.Context, string) ([]byte, error) {
	return emptyList(), nil
}

func (*gpuAPIReader) ListDaemonSets(context.Context) ([]byte, error) {
	return emptyList(), nil
}

func (r *gpuAPIReader) ListWorkloads(context.Context, string) ([]byte, error) {
	return defaultList(r.workloads), nil
}

func TestGPUConsumersDisabledByDefault(t *testing.T) {
	reader := &gpuAPIReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		GPU:     GPUOptions{Reader: reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/gpu-consumers", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.podsNamespace) != 0 {
		t.Fatalf("disabled endpoint read Pods: %#v", reader.podsNamespace)
	}
}

func TestGPUConsumersAreClusterWideWithoutViewerIdentity(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "alpha", Cluster: "cluster-a", Namespace: "team-a", LocalQueue: "queue-a", Source: "kubernetes",
			Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"alpha-group"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &gpuAPIReader{
		pods: apiList(
			apiPod("alpha", "pod-alpha", "team-a", "Running", "node-a", nil, "1"),
			apiPod("beta", "pod-beta", "team-b", "Pending", "", nil, "2"),
		),
	}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		WorkspaceDirectory: directory,
		GPU:                GPUOptions{Reader: reader, FleetEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/gpu-consumers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var snapshot gpuinventory.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Summary.Pods != 2 || snapshot.Summary.RequestedFullGPUs != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.podsNamespace) != 1 || reader.podsNamespace[0] != "" {
		t.Fatalf("Pod namespaces = %#v, want cluster-wide empty namespace", reader.podsNamespace)
	}
}

func TestWorkspaceGPUWorkloadsUseResolvedNamespaceAndQueue(t *testing.T) {
	controller := true
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Cluster: "cluster-a", Namespace: "shared", LocalQueue: "alpha-queue", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
			{
				ID: "beta", Cluster: "cluster-a", Namespace: "shared", LocalQueue: "beta-queue", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &gpuAPIReader{
		jobs: apiList(
			apiOwner("Job", "alpha", "job-alpha", "shared", nil, apiTauLabels("alpha-queue")),
			apiOwner("Job", "beta", "job-beta", "shared", nil, apiTauLabels("beta-queue")),
			apiOwner("Job", "external", "job-external", "shared", nil, map[string]string{queueLabelAPI: "alpha-queue"}),
		),
		pods: apiList(
			apiPod("alpha-pod", "pod-alpha", "shared", "Running", "node-a", apiOwnerRef("Job", "alpha", "job-alpha", &controller), "1"),
			apiPod("beta-pod", "pod-beta", "shared", "Running", "node-b", apiOwnerRef("Job", "beta", "job-beta", &controller), "2"),
			apiPod("external-pod", "pod-external", "shared", "Running", "node-c", apiOwnerRef("Job", "external", "job-external", &controller), "3"),
		),
	}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		WorkspaceDirectory: directory,
		GPU:                GPUOptions{Reader: reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/portal/workspace-gpu-workloads?workspace=alpha", nil)
	req.Header.Set(defaultViewerUserHeader, "user@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "researchers")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		State     string                  `json:"state"`
		Workloads []gpuinventory.Workload `json:"workloads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "ready" || len(response.Workloads) != 1 ||
		response.Workloads[0].Owner.Name != "alpha" ||
		response.Workloads[0].Queue != "alpha-queue" {
		t.Fatalf("response = %+v", response)
	}
	override := httptest.NewRequest(http.MethodGet, "/api/portal/workspace-gpu-workloads?workspace=alpha&namespace=other", nil)
	override.Header.Set(defaultViewerUserHeader, "user@example.com")
	override.Header.Set(defaultViewerGroupsHeader, "researchers")
	overrideRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(overrideRec, override)
	if overrideRec.Code != http.StatusBadRequest {
		t.Fatalf("override status = %d, want %d: %s", overrideRec.Code, http.StatusBadRequest, overrideRec.Body.String())
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.podsNamespace) != 1 || reader.podsNamespace[0] != "shared" {
		t.Fatalf("Pod namespaces = %#v", reader.podsNamespace)
	}
}

func TestWorkspaceGPUWorkloadsRequireCompleteScope(t *testing.T) {
	reader := &gpuAPIReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Runs:    RunsOptions{Namespace: "team-a"},
		GPU:     GPUOptions{Reader: reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/workspace-gpu-workloads", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.podsNamespace) != 0 {
		t.Fatalf("incomplete scope read Pods: %#v", reader.podsNamespace)
	}
}

func TestGPUConsumerOwnerFailureIsPartialAndPodFailureIsNonSuccess(t *testing.T) {
	reader := &gpuAPIReader{
		pods: apiList(apiPod("standalone", "pod-1", "team-a", "Running", "node-a", nil, "1")),
		rayJobsErr: apierrors.NewForbidden(
			schema.GroupResource{Group: "ray.io", Resource: "rayjobs"},
			"",
			errors.New("forbidden internal detail"),
		),
	}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		GPU:     GPUOptions{Reader: reader, FleetEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/gpu-consumers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d: %s", rec.Code, rec.Body.String())
	}
	var snapshot gpuinventory.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Partial || snapshot.Summary.Pods != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	sourceStates := map[string]string{}
	for _, source := range snapshot.Sources {
		sourceStates[source.Source] = source.State
	}
	if sourceStates["rayJobs"] != "forbidden" {
		t.Fatalf("source states = %#v", sourceStates)
	}

	reader.podsErr = errors.New("forbidden secret backend detail")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/gpu-consumers", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("Pod failure status = %d, want %d: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if body := rec.Body.String(); containsAny(body, "secret backend", "forbidden internal") {
		t.Fatalf("response leaked raw error: %s", body)
	}
}

func TestGPUPodListForbiddenUsesSafeHTTP403(t *testing.T) {
	cause := apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"},
		"",
		errors.New("sensitive authorization detail"),
	)
	reader := &gpuAPIReader{podsErr: cause}
	fleet, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		GPU:     GPUOptions{Reader: reader, FleetEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	fleet.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/gpu-consumers", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("fleet status = %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var fleetBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fleetBody); err != nil {
		t.Fatal(err)
	}
	if fleetBody["state"] != "forbidden" ||
		fleetBody["reason"] != "fleet GPU consumer inventory is not authorized to list Pods" ||
		containsAny(rec.Body.String(), "sensitive authorization detail") {
		t.Fatalf("fleet body = %s", rec.Body.String())
	}

	workspace, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Runs:    RunsOptions{Namespace: "team-a", LocalQueue: "queue-a"},
		GPU:     GPUOptions{Reader: reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	workspace.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/workspace-gpu-workloads", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("workspace status = %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	var workspaceBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &workspaceBody); err != nil {
		t.Fatal(err)
	}
	if workspaceBody["state"] != "forbidden" ||
		workspaceBody["reason"] != "workspace GPU workload inventory is not authorized to list Pods" ||
		workspaceBody["scope"] == nil ||
		containsAny(rec.Body.String(), "sensitive authorization detail") {
		t.Fatalf("workspace body = %s", rec.Body.String())
	}
}

const queueLabelAPI = "kueue.x-k8s.io/queue-name"

func emptyList() []byte {
	return []byte(`{"items":[]}`)
}

func defaultList(value []byte) []byte {
	if value == nil {
		return emptyList()
	}
	return value
}

func apiList(values ...map[string]any) []byte {
	data, err := json.Marshal(map[string]any{"items": values})
	if err != nil {
		panic(err)
	}
	return data
}

func apiOwner(kind, name, uid, namespace string, refs []map[string]any, labels map[string]string) map[string]any {
	return map[string]any{
		"kind": kind,
		"metadata": map[string]any{
			"name":            name,
			"uid":             uid,
			"namespace":       namespace,
			"labels":          labels,
			"ownerReferences": refs,
		},
	}
}

func apiOwnerRef(kind, name, uid string, controller *bool) map[string]any {
	return map[string]any{
		"kind":       kind,
		"name":       name,
		"uid":        uid,
		"controller": controller,
	}
}

func apiPod(name, uid, namespace, phase, node string, owner map[string]any, gpuCount string) map[string]any {
	refs := []map[string]any{}
	if owner != nil {
		refs = append(refs, owner)
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"uid":             uid,
			"namespace":       namespace,
			"ownerReferences": refs,
		},
		"spec": map[string]any{
			"nodeName": node,
			"containers": []map[string]any{{
				"resources": map[string]any{
					"requests": map[string]string{"nvidia.com/gpu": gpuCount},
				},
			}},
		},
		"status": map[string]any{"phase": phase},
	}
}

func apiTauLabels(queue string) map[string]string {
	return map[string]string{
		workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
		queueLabelAPI:               queue,
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
