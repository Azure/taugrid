// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"strings"
	"testing"
)

func TestParseAndRenderStatus(t *testing.T) {
	w, err := Parse([]byte(`{
	  "apiVersion": "tau.azure.com/v1alpha1",
	  "kind": "TauWorkspace",
	  "metadata": {"name": "sample", "namespace": "tau-system", "generation": 7},
	  "spec": {
	    "queue": "sample",
	    "defaults": {"outputRoot": "/data/projects/sample/runs"}
	  },
	  "status": {
	    "phase": "Ready",
	    "observedGeneration": 7,
	    "target": {"resolvedNamespace": "sample"},
	    "queue": {"localQueue": "sample", "clusterQueue": "sample-cq"},
	    "quota": [{"resource": "gpu", "flavor": "h200", "nominal": 16, "admitted": 8, "pending": 4}],
	    "conditions": [{"type": "RBACReady", "status": "True", "reason": "Allowed"}]
	  }
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := RenderStatus(w)
	for _, want := range []string{"Workspace: sample", "phase:      Ready", "clusterQ:   sample-cq", "RBACReady", "gpu", "h200"} {
		if !strings.Contains(out, want) {
			t.Fatalf("RenderStatus missing %q:\n%s", want, out)
		}
	}
	if !Ready(w) {
		t.Fatalf("Ready() = false, want true")
	}
	w.Status.ObservedGeneration--
	if Ready(w) {
		t.Fatal("Ready() = true for stale observedGeneration")
	}
}

func TestRenderListSortsByName(t *testing.T) {
	out := RenderList(WorkspaceList{Items: []Workspace{
		{Metadata: ObjectMeta{Name: "zeta"}, Status: WorkspaceStatus{Phase: "Ready"}},
		{Metadata: ObjectMeta{Name: "sample"}, Status: WorkspaceStatus{Phase: "Degraded"}},
	}})
	if strings.Index(out, "sample") > strings.Index(out, "zeta") {
		t.Fatalf("RenderList did not sort by name:\n%s", out)
	}
}

func TestNewQuotaRequestDefaultsNamespaceAndMode(t *testing.T) {
	req := NewQuotaRequest("sample-h200-burst", "", QuotaRequestSpec{
		Workspace: "sample",
		Resource:  "h200",
		Requested: 32,
		Reason:    "checkpoint sweep",
	})
	if req.Metadata.Namespace != SystemNamespace {
		t.Fatalf("namespace = %q, want %q", req.Metadata.Namespace, SystemNamespace)
	}
	if req.Spec.MutationMode != "ReportOnly" {
		t.Fatalf("mutation mode = %q, want ReportOnly", req.Spec.MutationMode)
	}
}
