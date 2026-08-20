// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAndRenderStatus(t *testing.T) {
	w, err := Parse([]byte(`{
	  "apiVersion": "tau.azure.com/v1alpha1",
	  "kind": "TauWorkspace",
	  "metadata": {"name": "sample", "namespace": "tau-platform", "generation": 7},
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
	for _, want := range []string{"Workspace: sample", "phase:      Ready", "serviceAccount: default", "clusterQ:   sample-cq", "RBACReady", "gpu", "h200"} {
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

func TestWorkspaceCLIViewResolvesWorkloadValues(t *testing.T) {
	w := Workspace{
		Spec: WorkspaceSpec{
			Target: WorkspaceTarget{Namespace: "declared-ns"},
			Queue:  "declared-queue",
			WorkloadIdentity: &WorkspaceWorkloadIdentity{
				ServiceAccountName: "workspace-sa",
			},
		},
		Status: WorkspaceStatus{
			Target: WorkspaceTargetStatus{ResolvedNamespace: "resolved-ns"},
			Queue:  WorkspaceQueueStatus{LocalQueue: "resolved-queue"},
		},
	}
	view := CLIView(w)
	if view.Resolved.Namespace != "resolved-ns" || view.Resolved.LocalQueue != "resolved-queue" || view.Resolved.ServiceAccount != "workspace-sa" {
		t.Fatalf("resolved view = %#v", view.Resolved)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"resolved":`, `"serviceAccount":"workspace-sa"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("CLI view missing %s: %s", want, raw)
		}
	}
}

func TestEffectiveServiceAccountDefaultsToKubernetesDefault(t *testing.T) {
	if got := EffectiveServiceAccount(Workspace{}); got != "default" {
		t.Fatalf("EffectiveServiceAccount = %q, want default", got)
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
	if req.Metadata.Namespace != PlatformNamespace {
		t.Fatalf("namespace = %q, want %q", req.Metadata.Namespace, PlatformNamespace)
	}
	if req.Spec.MutationMode != "ReportOnly" {
		t.Fatalf("mutation mode = %q, want ReportOnly", req.Spec.MutationMode)
	}
}
