package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/taugrid/tests/e2e/bundle"
)

func TestRayJobTerminalFailureIncludesDeploymentFailure(t *testing.T) {
	terminal, reason := rayJobTerminalFailure("", "Failed")
	if !terminal {
		t.Fatal("expected RayJob deployment failure to be treated as terminal")
	}
	if reason != "jobDeploymentStatus=Failed" {
		t.Fatalf("unexpected terminal reason: %q", reason)
	}
}

func TestRayJobTerminalFailureIncludesJobFailure(t *testing.T) {
	terminal, reason := rayJobTerminalFailure("FAILED", "")
	if !terminal {
		t.Fatal("expected RayJob jobStatus failure to be treated as terminal")
	}
	if reason != "jobStatus=FAILED" {
		t.Fatalf("unexpected terminal reason: %q", reason)
	}
}

func TestRayJobTerminalFailureIgnoresNonTerminalStatuses(t *testing.T) {
	for _, tc := range []struct {
		name             string
		jobStatus        string
		deploymentStatus string
	}{
		{name: "empty"},
		{name: "running", jobStatus: "RUNNING", deploymentStatus: "Running"},
		{name: "succeeded", jobStatus: "SUCCEEDED", deploymentStatus: "Complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminal, reason := rayJobTerminalFailure(tc.jobStatus, tc.deploymentStatus)
			if terminal {
				t.Fatalf("expected non-terminal status, got terminal reason %q", reason)
			}
		})
	}
}

func TestWaitForRayJobDeletedIgnoresUnrelatedNamespaceResourcesAndNeedsNoPodClient(t *testing.T) {
	dynamicClient, requests := managerDynamicClient(t)
	tc := &TestContext{
		T:             t,
		ctx:           context.Background(),
		dynamicClient: dynamicClient,
	}

	if err := tc.WaitForRayJobDeleted("shared", "e2e-nanogpt-large-gpu", 100*time.Millisecond); err != nil {
		t.Fatalf("wait for fixed RayJob absence: %v", err)
	}
	if got := requests(); len(got) != 1 || got[0] != "GET /apis/ray.io/v1/namespaces/shared/rayjobs/e2e-nanogpt-large-gpu" {
		t.Fatalf("fixed RayJob deletion wait made unexpected requests: %#v", got)
	}
}

func TestManagerRayJobWaitFailuresNeverUsePodAPI(t *testing.T) {
	t.Setenv("AI_RUNTIME_E2E_MANAGER_WORKLOAD_ONLY", "1")
	dynamicClient, requests := managerDynamicClient(t)
	tc := &TestContext{
		T:             t,
		ctx:           context.Background(),
		dynamicClient: dynamicClient,
		bundle:        bundle.New(t),
	}

	if err := tc.WaitForRayJobStatus("shared", "e2e-nanogpt-large-gpu", "SUCCEEDED", 10*time.Millisecond); err == nil {
		t.Fatal("expected timeout while the manager-visible RayJob is absent")
	}
	if _, err := tc.WaitForWorkloadAdmittedByRayJob("shared", "e2e-nanogpt-large-gpu", 10*time.Millisecond); err == nil {
		t.Fatal("expected timeout while the manager-visible Workload is absent")
	}
	for _, request := range requests() {
		if !strings.Contains(request, "/rayjobs/") && !strings.HasSuffix(request, "/workloads") {
			t.Fatalf("manager RayJob waits unexpectedly used non-manager API: %s", request)
		}
	}
}

func TestWaitForNoPodsByLabelIgnoresUnrelatedSharedNamespacePods(t *testing.T) {
	selector := "ray.io/originated-from-cr-name=e2e-nanogpt-large-gpu,ray.io/originated-from-crd=RayJob"
	var gotSelector string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/shared/pods" {
			t.Errorf("expected scoped Pod list, got %s %s", r.Method, r.URL.Path)
		}
		gotSelector = r.URL.Query().Get("labelSelector")
		writeJSONResponse(w, http.StatusOK, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
	}))
	t.Cleanup(server.Close)
	kubeClient, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	tc := &TestContext{
		T:          t,
		ctx:        context.Background(),
		kubeClient: kubeClient,
	}

	if err := tc.WaitForNoPodsByLabel("shared", selector, 100*time.Millisecond); err != nil {
		t.Fatalf("unrelated shared namespace pod blocked scoped cleanup: %v", err)
	}
	if gotSelector != selector {
		t.Fatalf("Pod list selector = %q, want %q", gotSelector, selector)
	}
}

func managerDynamicClient(t *testing.T) (dynamic.Interface, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/rayjobs/"):
			writeJSONResponse(w, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		case strings.HasSuffix(r.URL.Path, "/workloads"):
			writeJSONResponse(w, http.StatusOK, `{"kind":"WorkloadList","apiVersion":"kueue.x-k8s.io/v1beta1","items":[]}`)
		default:
			t.Errorf("unexpected manager API request: %s %s", r.Method, r.URL.Path)
			writeJSONResponse(w, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		}
	}))
	t.Cleanup(server.Close)

	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}
