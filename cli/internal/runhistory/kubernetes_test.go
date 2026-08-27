// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestKubernetesSourceListsV1Beta2Workloads(t *testing.T) {
	const namespace = "research"
	const expectedPath = "/apis/kueue.x-k8s.io/v1beta2/namespaces/" + namespace + "/workloads"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != expectedPath {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, expectedPath)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "apiVersion": "kueue.x-k8s.io/v1beta2",
  "kind": "WorkloadList",
  "items": [{
    "apiVersion": "kueue.x-k8s.io/v1beta2",
    "kind": "Workload",
    "metadata": {
      "name": "rayjob-run-abc",
      "namespace": "research",
      "uid": "workload-uid",
      "resourceVersion": "7",
      "generation": 3,
      "creationTimestamp": "2026-08-27T10:00:00Z",
      "labels": {"tau.azure.com/run-id": "run-abc"}
    },
    "spec": {"queueName": "research-queue"},
    "status": {
      "phase": "Finished",
      "admission": {"clusterQueue": "research-cluster-queue"},
      "conditions": [
        {"type": "Admitted", "status": "True", "lastTransitionTime": "2026-08-27T10:01:00Z"},
        {"type": "Finished", "status": "True", "lastTransitionTime": "2026-08-27T10:02:00Z"}
      ]
    }
  }]
}`))
	}))
	defer server.Close()

	dynamicClient, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	source := &KubernetesSource{dynamic: dynamicClient}

	workloads, err := source.ListWorkloads(context.Background(), namespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) != 1 {
		t.Fatalf("workload count = %d, want 1", len(workloads))
	}
	workload := workloads[0]
	if workload.Name != "rayjob-run-abc" || workload.Namespace != namespace || workload.UID != "workload-uid" {
		t.Fatalf("metadata = %+v", workload.Metadata)
	}
	if workload.Queue != "research-queue" || workload.ClusterQueue != "research-cluster-queue" || workload.Phase != "Finished" {
		t.Fatalf("workload routing = %+v", workload)
	}
	if !workload.Admitted || workload.AdmittedAt.IsZero() || workload.FinishedAt.IsZero() {
		t.Fatalf("workload lifecycle = %+v", workload)
	}
}
