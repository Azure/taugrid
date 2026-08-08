// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGPUAllocatable(t *testing.T) {
	tests := []struct {
		name string
		node topologyNodeDoc
		want int
	}{
		{"8 GPUs", makeNode("n1", "8", true, "westus2-1"), 8},
		{"0 GPUs", makeNode("n2", "0", true, ""), 0},
		{"no key", makeNode("n3", "", true, ""), 0},
		{"non-numeric", makeNode("n4", "abc", true, ""), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gpuAllocatable(tt.node); got != tt.want {
				t.Fatalf("gpuAllocatable() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNodeIsReady(t *testing.T) {
	tests := []struct {
		name string
		node topologyNodeDoc
		want bool
	}{
		{"ready", makeNode("n1", "8", true, ""), true},
		{"not ready", makeNode("n2", "8", false, ""), false},
		{"cordoned", func() topologyNodeDoc {
			n := makeNode("n3", "8", true, "")
			n.Spec.Unschedulable = true
			return n
		}(), false},
		{"no conditions", func() topologyNodeDoc {
			var n topologyNodeDoc
			n.Metadata.Name = "n3"
			return n
		}(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeIsReady(tt.node); got != tt.want {
				t.Fatalf("nodeIsReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIBCapableSKUs(t *testing.T) {
	tests := []struct {
		instanceType string
		wantOK       bool
		wantSKU      string
	}{
		{"Standard_ND96isr_H200_v5", true, "H200"},
		{"standard_nd96isr_h200_v5", true, "H200"},
		{"Standard_ND96amsr_A100_v4", true, "A100"},
		{"Standard_ND96isr_H100_v5", true, "H100"},
		{"Standard_ND96isr_GB200_v6", true, "GB200"},
		{"Standard_NC96ads_H100_v5", false, ""},
		{"Standard_DS2_v2", false, ""},
		{"", false, ""},
	}
	for _, tt := range tests {
		name := tt.instanceType
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			sku, ok := ibCapableSKUs[strings.ToLower(tt.instanceType)]
			if ok != tt.wantOK {
				t.Fatalf("ibCapableSKUs[%q]: got ok=%v, want %v", tt.instanceType, ok, tt.wantOK)
			}
			if ok && sku != tt.wantSKU {
				t.Fatalf("ibCapableSKUs[%q] = %q, want %q", tt.instanceType, sku, tt.wantSKU)
			}
		})
	}
}

func TestClusterQueueFlavorNames(t *testing.T) {
	cq := topologyClusterQueueDoc{}
	cq.Spec.ResourceGroups = []struct {
		Flavors []struct {
			Name string `json:"name"`
		} `json:"flavors"`
	}{
		{Flavors: []struct {
			Name string `json:"name"`
		}{
			{Name: "gpu-h200"},
			{Name: "gpu-a100"},
			{Name: "gpu-h200"}, // duplicate
		}},
		{Flavors: []struct {
			Name string `json:"name"`
		}{
			{Name: "taugrid-default"},
			{Name: ""},
		}},
	}

	names := cq.flavorNames()
	want := []string{"gpu-a100", "gpu-h200", "taugrid-default"}
	if len(names) != len(want) {
		t.Fatalf("flavorNames() = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Fatalf("flavorNames()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestValidateFlavorAccountingOnly(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"): `{"metadata":{"name":"taugrid-default"},"spec":{}}`,
		},
	}
	results := validateFlavor(context.Background(), runner, "taugrid-default")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].status != checkOK || results[0].label != "accounting-only" {
		t.Fatalf("unexpected result: %+v", results[0])
	}
}

func TestValidateFlavorHealthyH200(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-h200", "-o", "json"): `{
				"metadata":{"name":"gpu-h200"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_ND96isr_H200_v5"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_ND96isr_H200_v5", "-o", "json"): makeNodeListJSON(
				makeNode("gpu-0", "8", true, "westus2-1"),
				makeNode("gpu-1", "8", true, "westus2-2"),
			),
		},
	}

	results := validateFlavor(context.Background(), runner, "gpu-h200")

	wantChecks := map[string]checkStatus{
		"node-match":      checkOK,
		"gpu-allocatable": checkOK,
		"topology-zone":   checkOK,
		"ib-capable":      checkOK,
	}
	for _, r := range results {
		want, ok := wantChecks[r.label]
		if !ok {
			t.Fatalf("unexpected check label %q", r.label)
		}
		if r.status != want {
			t.Fatalf("check %q: got status %q, want %q (message: %s)", r.label, r.status, want, r.message)
		}
		delete(wantChecks, r.label)
	}
	if len(wantChecks) > 0 {
		t.Fatalf("missing checks: %v", wantChecks)
	}
}

func TestValidateFlavorNoNodes(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-h100", "-o", "json"): `{
				"metadata":{"name":"gpu-h100"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_NC96ads_H100_v5"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_NC96ads_H100_v5", "-o", "json"): `{"items":[]}`,
		},
	}

	results := validateFlavor(context.Background(), runner, "gpu-h100")
	if len(results) != 1 || results[0].status != checkError || results[0].label != "node-match" {
		t.Fatalf("expected single node-match error, got %+v", results)
	}
}

func TestValidateFlavorPartialGPU(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-a100", "-o", "json"): `{
				"metadata":{"name":"gpu-a100"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_ND96amsr_A100_v4"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_ND96amsr_A100_v4", "-o", "json"): makeNodeListJSON(
				makeNode("gpu-0", "8", true, "eastus2-1"),
				makeNode("gpu-1", "0", true, "eastus2-2"),
			),
		},
	}

	results := validateFlavor(context.Background(), runner, "gpu-a100")
	statusByLabel := map[string]checkStatus{}
	for _, r := range results {
		statusByLabel[r.label] = r.status
	}
	if statusByLabel["gpu-allocatable"] != checkWarn {
		t.Fatalf("expected gpu-allocatable=warn, got %q", statusByLabel["gpu-allocatable"])
	}
	if statusByLabel["ib-capable"] != checkOK {
		t.Fatalf("expected ib-capable=ok for A100 SKU, got %q", statusByLabel["ib-capable"])
	}
}

func TestValidateFlavorMissingZone(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-h100", "-o", "json"): `{
				"metadata":{"name":"gpu-h100"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_NC96ads_H100_v5"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_NC96ads_H100_v5", "-o", "json"): makeNodeListJSON(
				makeNode("gpu-0", "4", true, ""),
			),
		},
	}

	results := validateFlavor(context.Background(), runner, "gpu-h100")
	statusByLabel := map[string]checkStatus{}
	for _, r := range results {
		statusByLabel[r.label] = r.status
	}
	if statusByLabel["topology-zone"] != checkWarn {
		t.Fatalf("expected topology-zone=warn, got %q", statusByLabel["topology-zone"])
	}
	if _, ok := statusByLabel["ib-capable"]; ok {
		t.Fatal("H100 should not have ib-capable check")
	}
}

func TestValidateClusterTopologyOutput(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "taugrid-cq", "-o", "json"): `{
				"metadata":{"name":"taugrid-cq"},
				"spec":{"resourceGroups":[
					{"flavors":[{"name":"gpu-h200"},{"name":"taugrid-default"}]}
				]}
			}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"): `{"metadata":{"name":"taugrid-default"},"spec":{}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-h200", "-o", "json"): `{
				"metadata":{"name":"gpu-h200"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_ND96isr_H200_v5"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_ND96isr_H200_v5", "-o", "json"): makeNodeListJSON(
				makeNode("gpu-0", "8", true, "westus2-1"),
			),
		},
	}

	var buf bytes.Buffer
	err := validateClusterTopology(context.Background(), &buf, runner, "test-ctx", "taugrid-cq")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	for _, want := range []string{
		"cluster: test-ctx",
		"clusterqueue: taugrid-cq",
		"resourceflavor taugrid-default:",
		"resourceflavor gpu-h200:",
		"ok    node-match:",
		"ok    gpu-allocatable:",
		"ok    topology-zone:",
		"ok    ib-capable:",
		"ok    accounting-only:",
		"summary: 5 passed, 0 warnings, 0 errors",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, output)
		}
	}
}

func TestValidateClusterTopologyErrors(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "taugrid-cq", "-o", "json"): `{
				"metadata":{"name":"taugrid-cq"},
				"spec":{"resourceGroups":[{"flavors":[{"name":"gpu-missing"}]}]}
			}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "gpu-missing", "-o", "json"): `{
				"metadata":{"name":"gpu-missing"},
				"spec":{"nodeLabels":{"node.kubernetes.io/instance-type":"Standard_Missing_v1"}}
			}`,
			fakeRawKey("get", "nodes", "-l", "node.kubernetes.io/instance-type=Standard_Missing_v1", "-o", "json"): `{"items":[]}`,
		},
	}

	var buf bytes.Buffer
	err := validateClusterTopology(context.Background(), &buf, runner, "test", "taugrid-cq")
	if err == nil {
		t.Fatal("expected error for 0-node flavor")
	}
	if !strings.Contains(err.Error(), "1 error") {
		t.Fatalf("error should mention 1 error: %v", err)
	}
	if !strings.Contains(buf.String(), "error node-match:") {
		t.Fatalf("output should contain error line:\n%s", buf.String())
	}
}

func makeNode(name, gpuCount string, ready bool, zone string) topologyNodeDoc {
	var n topologyNodeDoc
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{}
	if zone != "" {
		n.Metadata.Labels["topology.kubernetes.io/zone"] = zone
	}
	n.Status.Allocatable = map[string]string{}
	if gpuCount != "" {
		n.Status.Allocatable["nvidia.com/gpu"] = gpuCount
	}
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	n.Status.Conditions = []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}{
		{Type: "Ready", Status: readyStatus},
	}
	return n
}

func makeNodeListJSON(nodes ...topologyNodeDoc) string {
	data, _ := json.Marshal(topologyNodeListDoc{Items: nodes})
	return string(data)
}

func TestValidateTopologyCLIHelp(t *testing.T) {
	out, err := runCluster(t, "validate", "topology", "--help")
	if err != nil {
		t.Fatalf("--help failed: %v\n%s", err, out)
	}
	for _, want := range []string{"preset", "cluster-queue", "context", "ResourceFlavor"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
}
