// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kueueapi"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
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

func TestClusterQueueFlavorContractsUseAvailableGPUCapacity(t *testing.T) {
	var cq topologyClusterQueueDoc
	if err := json.Unmarshal([]byte(`{
		"spec":{"cohort":"research","resourceGroups":[{"flavors":[
			{"name":"cpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"0","borrowingLimit":"0"}]},
			{"name":"gpu-dp","resources":[{"name":"nvidia.com/gpu","nominalQuota":"2"}]},
			{"name":"gpu-dra","resources":[{"name":"gpu.nvidia.com","nominalQuota":"0","borrowingLimit":"1"}]},
			{"name":"gpu-mig","resources":[{"name":"nvidia.com/mig-1g.18gb","nominalQuota":"7"}]},
			{"name":"gpu-unlimited","resources":[{"name":"nvidia.com/gpu","nominalQuota":"0"}]}
		]}]}
	}`), &cq); err != nil {
		t.Fatal(err)
	}

	contracts := clusterQueueFlavorContracts(cq)
	if len(contracts) != 5 {
		t.Fatalf("contracts = %+v, want 5 flavors", contracts)
	}
	want := []topologyFlavorContract{
		{name: "cpu"},
		{name: "gpu-dp", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}},
		{name: "gpu-dra", gpuResources: []string{kueueapi.GPUResource}},
		{name: "gpu-mig", gpuResources: []string{"nvidia.com/mig-1g.18gb"}},
		{name: "gpu-unlimited", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}},
	}
	for i := range want {
		if contracts[i].name != want[i].name || strings.Join(contracts[i].gpuResources, ",") != strings.Join(want[i].gpuResources, ",") {
			t.Fatalf("contracts[%d] = %+v, want %+v", i, contracts[i], want[i])
		}
	}
}

func TestClusterQueueFlavorContractsRequireCohortForUnlimitedBorrowing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		spec         string
		wantResource bool
	}{
		{name: "no cohort", spec: `"resourceGroups"`, wantResource: false},
		{name: "v1beta1 cohort", spec: `"cohort":"research","resourceGroups"`, wantResource: true},
		{name: "v1beta2 cohort", spec: `"cohortName":"research","resourceGroups"`, wantResource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cq topologyClusterQueueDoc
			raw := `{"spec":{` + tc.spec + `:[{"flavors":[{"name":"gpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"0"}]}]}]}}`
			if err := json.Unmarshal([]byte(raw), &cq); err != nil {
				t.Fatal(err)
			}

			contracts := clusterQueueFlavorContracts(cq)
			if len(contracts) != 1 {
				t.Fatalf("contracts = %+v, want one flavor", contracts)
			}
			gotResource := len(contracts[0].gpuResources) == 1
			if gotResource != tc.wantResource {
				t.Fatalf("gpuResources = %v, want resource=%v", contracts[0].gpuResources, tc.wantResource)
			}
		})
	}
}

func TestValidateFlavorSkipsCPUFlavorWithLinuxLabelAndZeroGPUQuota(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"): `{"metadata":{"name":"taugrid-default"},"spec":{"nodeLabels":{"kubernetes.io/os":"linux"}}}`,
		},
	}
	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "taugrid-default"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].status != checkOK || results[0].label != "gpu-contract" || !strings.Contains(results[0].message, "skipped") {
		t.Fatalf("unexpected result: %+v", results[0])
	}
	if len(runner.calls) != 1 {
		t.Fatalf("CPU flavor should not list nodes or ResourceSlices, calls=%v", runner.calls)
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

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "gpu-h200", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})

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

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "gpu-h100", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
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

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "gpu-a100", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
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

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "gpu-h100", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
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

func TestValidateFlavorGenericSelectorUsesOnlyGPUCandidateNodes(t *testing.T) {
	cpu := makeNode("cpu-0", "", true, "westus2-1")
	gpu := makeNode("gpu-0", "1", true, "westus2-1")
	gpu.Spec.Taints = []topologyTaintDoc{{Key: "sku", Value: "gpu", Effect: "NoSchedule"}}
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "generic-gpu", "-o", "json"): `{
				"metadata":{"name":"generic-gpu"},
				"spec":{"nodeLabels":{"kubernetes.io/os":"linux"},"nodeTaints":[{"key":"sku","value":"gpu","effect":"NoSchedule"}]}
			}`,
			fakeRawKey("get", "nodes", "-l", "kubernetes.io/os=linux", "-o", "json"): makeNodeListJSON(cpu, gpu),
		},
	}

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "generic-gpu", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
	statusByLabel := make(map[string]checkStatus)
	messageByLabel := make(map[string]string)
	for _, result := range results {
		statusByLabel[result.label] = result.status
		messageByLabel[result.label] = result.message
	}
	if statusByLabel["gpu-allocatable"] != checkOK {
		t.Fatalf("generic flavor should validate 1/1 GPU nodes, results=%+v", results)
	}
	if !strings.Contains(messageByLabel["node-match"], "1 GPU-capable node(s) selected from 2") {
		t.Fatalf("node-match should exclude the CPU node: %q", messageByLabel["node-match"])
	}
}

func TestValidateFlavorTaintKeepsUnregisteredGPUNodeInDenominator(t *testing.T) {
	node := makeNode("gpu-0", "", true, "westus2-1")
	node.Spec.Taints = []topologyTaintDoc{{Key: "sku", Value: "gpu", Effect: "NoSchedule"}}
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "generic-gpu", "-o", "json"): `{
				"metadata":{"name":"generic-gpu"},
				"spec":{"nodeLabels":{"kubernetes.io/os":"linux"},"nodeTaints":[{"key":"sku","value":"gpu","effect":"NoSchedule"}]}
			}`,
			fakeRawKey("get", "nodes", "-l", "kubernetes.io/os=linux", "-o", "json"): makeNodeListJSON(node),
		},
	}

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "generic-gpu", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
	statusByLabel := make(map[string]checkStatus)
	for _, result := range results {
		statusByLabel[result.label] = result.status
	}
	if statusByLabel["node-match"] != checkOK || statusByLabel["gpu-allocatable"] != checkError {
		t.Fatalf("tainted GPU node should be selected and fail registration, results=%+v", results)
	}
}

func TestValidateFlavorWithoutNodeLabelsUsesAllNodes(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "resource-only-gpu", "-o", "json"): `{"metadata":{"name":"resource-only-gpu"},"spec":{}}`,
			fakeRawKey("get", "nodes", "-o", "json"): makeNodeListJSON(
				makeNode("cpu-0", "", true, "westus2-1"),
				makeNode("gpu-0", "1", true, "westus2-1"),
			),
		},
	}

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "resource-only-gpu", gpuResources: []string{kueueapi.GPUResourceDevicePlugin}})
	for _, result := range results {
		if result.label == "node-match" {
			if result.status != checkOK || !strings.Contains(result.message, "<all nodes>") || !strings.Contains(result.message, "1 GPU-capable node(s) selected from 2") {
				t.Fatalf("unexpected all-node match result: %+v", result)
			}
			return
		}
	}
	t.Fatalf("missing node-match result: %+v", results)
}

func TestFlavorSelectsGPURecognizesSelectors(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "managed GPU series", key: runtopology.ManagedGPUSeriesLabel, value: "nc-h100-v5"},
		{name: "GPU class", key: workloadmeta.NodeLabelGPUClass, value: "h100-95gb"},
		{name: "NVIDIA product", key: "nvidia.com/gpu.product", value: "NVIDIA-H100-80GB-HBM3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !flavorSelectsGPU(map[string]string{test.key: test.value}) {
				t.Fatalf("%s should identify a GPU-specific ResourceFlavor selector", test.key)
			}
		})
	}
}

func TestValidateFlavorMIGUsesConfiguredProfileResource(t *testing.T) {
	const migResource = "nvidia.com/mig-1g.18gb"
	for _, tc := range []struct {
		name       string
		alloc      map[string]string
		wantStatus checkStatus
	}{
		{name: "profile registered", alloc: map[string]string{migResource: "7"}, wantStatus: checkOK},
		{name: "only whole GPU and another profile", alloc: map[string]string{"nvidia.com/gpu": "1", "nvidia.com/mig-3g.71gb": "1"}, wantStatus: checkError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const gpuClass = "a100-80gb"
			node := makeNode("mig-0", "", true, "westus2-1")
			node.Metadata.Labels[workloadmeta.NodeLabelGPUClass] = gpuClass
			for name, quantity := range tc.alloc {
				node.Status.Allocatable[name] = quantity
			}
			resourceFlavorJSON := fmt.Sprintf(`{"metadata":{"name":"mig-a100"},"spec":{"nodeLabels":{%q:%q}}}`, workloadmeta.NodeLabelGPUClass, gpuClass)
			selector := workloadmeta.NodeLabelGPUClass + "=" + gpuClass
			runner := &fakeRawRunner{
				outputs: map[string]string{
					fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "mig-a100", "-o", "json"): resourceFlavorJSON,
					fakeRawKey("get", "nodes", "-l", selector, "-o", "json"):                     makeNodeListJSON(node),
				},
			}

			results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "mig-a100", gpuResources: []string{migResource}})
			for _, result := range results {
				if result.label == "gpu-allocatable" {
					if result.status != tc.wantStatus || !strings.Contains(result.message, migResource) {
						t.Fatalf("MIG registration result = %+v, want status %s for %s", result, tc.wantStatus, migResource)
					}
					return
				}
			}
			t.Fatalf("missing MIG gpu-allocatable result: %+v", results)
		})
	}
}

func TestValidateFlavorDRAUsesResourceSlices(t *testing.T) {
	cpu := makeNode("cpu-0", "", true, "westus2-1")
	gpu := makeNode("dra-0", "", true, "westus2-1")
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "dra-gpu", "-o", "json"): `{"metadata":{"name":"dra-gpu"},"spec":{"nodeLabels":{"kubernetes.io/os":"linux"}}}`,
			fakeRawKey("get", "nodes", "-l", "kubernetes.io/os=linux", "-o", "json"):    makeNodeListJSON(cpu, gpu),
			fakeRawKey("get", "resourceslices", "-o", "json"):                           `{"items":[{"spec":{"driver":"gpu.nvidia.com","nodeName":"dra-0","pool":{"name":"dra-0"},"devices":[{"name":"gpu-0"},{"name":"gpu-1"}]}}]}`,
		},
	}

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "dra-gpu", gpuResources: []string{kueueapi.GPUResource}})
	statusByLabel := make(map[string]checkStatus)
	messageByLabel := make(map[string]string)
	for _, result := range results {
		statusByLabel[result.label] = result.status
		messageByLabel[result.label] = result.message
	}
	if statusByLabel["gpu-resourceslices"] != checkOK {
		t.Fatalf("DRA flavor should use ResourceSlices, results=%+v", results)
	}
	if !strings.Contains(messageByLabel["node-match"], "1 GPU-capable node(s) selected from 2") {
		t.Fatalf("DRA node-match should exclude the CPU node: %q", messageByLabel["node-match"])
	}
}

func TestValidateFlavorDRAReportsResourceSliceAccessError(t *testing.T) {
	const gpuClass = "h200-141gb"
	node := makeNode("dra-0", "", true, "westus2-1")
	node.Metadata.Labels[workloadmeta.NodeLabelGPUClass] = gpuClass
	resourceFlavorJSON := fmt.Sprintf(`{"metadata":{"name":"dra-gpu"},"spec":{"nodeLabels":{%q:%q}}}`, workloadmeta.NodeLabelGPUClass, gpuClass)
	selector := workloadmeta.NodeLabelGPUClass + "=" + gpuClass
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "dra-gpu", "-o", "json"): resourceFlavorJSON,
			fakeRawKey("get", "nodes", "-l", selector, "-o", "json"):                    makeNodeListJSON(node),
		},
		errors: map[string]error{
			fakeRawKey("get", "resourceslices", "-o", "json"): errors.New("forbidden"),
		},
	}

	results := validateFlavor(context.Background(), runner, topologyFlavorContract{name: "dra-gpu", gpuResources: []string{kueueapi.GPUResource}})
	if len(results) != 1 || results[0].label != "gpu-resourceslices" || results[0].status != checkError || !strings.Contains(results[0].message, "forbidden") {
		t.Fatalf("DRA API error should be explicit, results=%+v", results)
	}
}

func TestValidatePresetTopologyUsesClusterQueueGPUContracts(t *testing.T) {
	gpu := makeNode("gpu-0", "1", true, "westus2-1")
	gpu.Spec.Taints = []topologyTaintDoc{{Key: "sku", Value: "gpu", Effect: "NoSchedule"}}
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "taugrid-default", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "name"): "localqueue.kueue.x-k8s.io/jobqueue",
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "name"):                          "clusterqueue.kueue.x-k8s.io/tau-cq",
			fakeRawKey("get", "topology.kueue.x-k8s.io", "default-node-topology", "-o", "name"):               "topology.kueue.x-k8s.io/default-node-topology",
			fakeRawKey("get", "workloadpriorityclass.kueue.x-k8s.io", "taugrid-default", "-o", "name"):        "workloadpriorityclass.kueue.x-k8s.io/taugrid-default",
			fakeRawKey("get", "priorityclass.scheduling.k8s.io", "taugrid-default", "-o", "name"):             "priorityclass.scheduling.k8s.io/taugrid-default",
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"):                          `{"spec":{"resourceGroups":[{"flavors":[{"name":"cpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"0"}]},{"name":"generic-gpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"1"}]}]}]}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "generic-gpu", "-o", "json"):                   `{"metadata":{"name":"generic-gpu"},"spec":{"nodeLabels":{"kubernetes.io/os":"linux"},"nodeTaints":[{"key":"sku","value":"gpu","effect":"NoSchedule"}]}}`,
			fakeRawKey("get", "nodes", "-l", "kubernetes.io/os=linux", "-o", "json"):                          makeNodeListJSON(gpu),
		},
	}

	var buf bytes.Buffer
	if err := validatePresetTopology(context.Background(), &buf, runner, "azure.research.training.l", "test"); err != nil {
		t.Fatalf("preset validation failed: %v\n%s", err, buf.String())
	}
	output := buf.String()
	if !strings.Contains(output, "resourceflavor generic-gpu:") || strings.Contains(output, "resourceflavor cpu:") {
		t.Fatalf("preset should validate only positive GPU contracts:\n%s", output)
	}
	if !strings.Contains(output, "summary: 8 passed, 0 warnings, 0 errors") {
		t.Fatalf("unexpected preset summary:\n%s", output)
	}
}

func TestValidateClusterTopologyOutput(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "taugrid-cq", "-o", "json"): `{
				"metadata":{"name":"taugrid-cq"},
				"spec":{"resourceGroups":[
					{"flavors":[
						{"name":"gpu-h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
						{"name":"taugrid-default","resources":[{"name":"nvidia.com/gpu","nominalQuota":"0"}]}
					]}
				]}
			}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"): `{"metadata":{"name":"taugrid-default"},"spec":{"nodeLabels":{"kubernetes.io/os":"linux"}}}`,
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
		"ok    gpu-contract: no GPU capacity in ClusterQueue contract; GPU topology checks skipped",
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
				"spec":{"resourceGroups":[{"flavors":[{"name":"gpu-missing","resources":[{"name":"nvidia.com/gpu","nominalQuota":"1"}]}]}]}
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
