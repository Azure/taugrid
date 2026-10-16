// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/taugrid/tests/e2e/bundle"
)

func TestGVRFromObjectMapsTopology(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("kueue.x-k8s.io/v1beta2")
	obj.SetKind("Topology")
	obj.SetName("test-topology")
	obj.SetCreationTimestamp(metav1.Now())

	got, err := gvrFromObject(obj)
	if err != nil {
		t.Fatalf("gvrFromObject: %v", err)
	}
	if got.Group != "kueue.x-k8s.io" || got.Version != "v1beta2" || got.Resource != "topologies" {
		t.Fatalf("gvrFromObject = %s, want kueue.x-k8s.io/v1beta2, Resource=topologies", got.String())
	}
}

func TestDeleteYAMLWithClientAndWaitDeletesInReverseAndWaitsForUID(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: e2e-stack
  uid: namespace-uid
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: e2e-stack-queue
  namespace: e2e-stack
  uid: queue-uid
`)

	var mu sync.Mutex
	var deletes []string
	deleted := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := r.URL.Path
		switch r.Method {
		case http.MethodGet:
			if deleted[path] {
				writeJSONResponse(w, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
				return
			}
			uid := "namespace-uid"
			kind := "Namespace"
			apiVersion := "v1"
			name := "e2e-stack"
			if strings.Contains(path, "/localqueues/") {
				uid = "queue-uid"
				kind = "LocalQueue"
				apiVersion = "kueue.x-k8s.io/v1beta2"
				name = "e2e-stack-queue"
			}
			writeJSONResponse(w, http.StatusOK, fmt.Sprintf(`{"kind":%q,"apiVersion":%q,"metadata":{"name":%q,"uid":%q}}`, kind, apiVersion, name, uid))
		case http.MethodDelete:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"propagationPolicy":"Foreground"`) {
				t.Errorf("DELETE %s did not request Foreground propagation: %s", path, body)
			}
			wantUID := "namespace-uid"
			if strings.Contains(path, "/localqueues/") {
				wantUID = "queue-uid"
			}
			if !strings.Contains(string(body), `"uid":"`+wantUID+`"`) {
				t.Errorf("DELETE %s did not require UID %s: %s", path, wantUID, body)
			}
			deletes = append(deletes, path)
			deleted[path] = true
			writeJSONResponse(w, http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, path)
			writeJSONResponse(w, http.StatusMethodNotAllowed, `{}`)
		}
	}))
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}

	if err := deleteYAMLWithClientAndWait(context.Background(), client, manifest, 100*time.Millisecond); err != nil {
		t.Fatalf("delete fixture and wait: %v", err)
	}
	want := []string{
		"/apis/kueue.x-k8s.io/v1beta2/namespaces/e2e-stack/localqueues/e2e-stack-queue",
		"/api/v1/namespaces/e2e-stack",
	}
	if fmt.Sprint(deletes) != fmt.Sprint(want) {
		t.Fatalf("delete order = %v, want %v", deletes, want)
	}
}

func TestDeleteYAMLWithClientAndWaitAcceptsUIDConflict(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: e2e-stack
`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResponse(w, http.StatusOK, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"e2e-stack","uid":"old-uid"}}`)
		case http.MethodDelete:
			writeJSONResponse(w, http.StatusConflict, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409}`)
		}
	}))
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}

	if err := deleteYAMLWithClientAndWait(context.Background(), client, manifest, 100*time.Millisecond); err != nil {
		t.Fatalf("UID conflict should leave the replacement object alone: %v", err)
	}
}

func TestDeleteYAMLWithClientAndWaitAcceptsRecreatedObject(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: e2e-stack
`)
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets++
			uid := "old-uid"
			if gets > 1 {
				uid = "new-uid"
			}
			writeJSONResponse(w, http.StatusOK, fmt.Sprintf(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"e2e-stack","uid":%q}}`, uid))
		case http.MethodDelete:
			writeJSONResponse(w, http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		}
	}))
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}

	if err := deleteYAMLWithClientAndWait(context.Background(), client, manifest, 100*time.Millisecond); err != nil {
		t.Fatalf("recreated object should satisfy deletion wait: %v", err)
	}
}

func TestDeleteYAMLWithClientAndWaitTimesOut(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: e2e-stack
`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResponse(w, http.StatusOK, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"e2e-stack","uid":"same-uid"}}`)
		case http.MethodDelete:
			writeJSONResponse(w, http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		}
	}))
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}

	err = deleteYAMLWithClientAndWait(context.Background(), client, manifest, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deletion timeout, got %v", err)
	}
}

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
