package queueresolve

import (
	"context"
	"fmt"
	"github.com/Azure/taugrid/core/kueueapi"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/topology"
)

type validationFakeRunner struct {
	outputs map[string]string
	errors  map[string]error
	calls   [][]string
}

func (f *validationFakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	k := validationKey(args...)
	if err := f.errors[k]; err != nil {
		return "", err
	}
	out, ok := f.outputs[k]
	if !ok {
		return "", fmt.Errorf("unexpected kubectl args: %v", args)
	}
	return out, nil
}

func validationKey(args ...string) string {
	return strings.Join(args, "\x00")
}

func g5A100Preset() *topology.ResolvedPreset {
	return &topology.ResolvedPreset{
		Preset: topology.Preset{
			Name:           "azure.research.training.l",
			Namespace:      "ray",
			ClusterQueue:   "tau-cq",
			ResourceFlavor: "gpu",
		},
		Options: topology.Options{
			Team:      "training",
			Lane:      "training",
			GPUClass:  "a100-nvlink-80gb",
			QueueName: "team-b",
		},
	}
}

func g5H200Preset() *topology.ResolvedPreset {
	return &topology.ResolvedPreset{
		Preset: topology.Preset{
			Name:           "azure.research.large-memory.xl",
			Namespace:      "ray",
			ClusterQueue:   "tau-cq",
			ResourceFlavor: "gpu",
		},
		Options: topology.Options{
			Team:      "research",
			Lane:      "large-memory",
			GPUClass:  "h200-nvlink-141gb",
			QueueName: "team-a",
		},
	}
}

func g5ValidationRunner() *validationFakeRunner {
	return &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-b", "-o", "json"): localQueueObject("team-b", "tau-cq", map[string]string{
				topology.LabelTeam: "training",
				topology.LabelLane: "training",
			}),
			validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-a", "-o", "json"): localQueueObject("team-a", "tau-cq", map[string]string{
				topology.LabelTeam: "research",
				topology.LabelLane: "large-memory",
			}),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): clusterQueueObject("tau-cq", "gpu", nil),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "gpu", "-o", "json"):  resourceFlavorObject("gpu", "", "", "sku-specific-taint"),
		},
		errors: map[string]error{},
	}
}

func TestValidateSelectionAcceptsG5GenericFlavorWithRenderedGPUSelector(t *testing.T) {
	runner := g5ValidationRunner()

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5H200Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: "h200-nvlink-141gb"},
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.Namespace != "ray" ||
		report.QueueName != "team-a" ||
		report.ClusterQueue != "tau-cq" ||
		report.ResourceFlavor != "gpu" ||
		report.Preset != "azure.research.large-memory.xl" {
		t.Fatalf("report did not preserve resolved queue topology: %+v", report)
	}
	for _, call := range runner.calls {
		for _, forbidden := range []string{"apply", "create", "patch", "label", "delete"} {
			if containsString(call, forbidden) {
				t.Fatalf("validator must be read-only; got kubectl args %v", call)
			}
		}
	}
	wantCalls := [][]string{
		{"-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-a", "-o", "json"},
		{"get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"},
		{"get", "resourceflavor.kueue.x-k8s.io", "gpu", "-o", "json"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestValidateSelectionChecksA100AndH200QueuesIndependently(t *testing.T) {
	runner := g5ValidationRunner()

	for _, tc := range []struct {
		name         string
		preset       *topology.ResolvedPreset
		gpuClass     string
		queue        string
		clusterQ     string
		expectedTeam string
	}{
		{
			name:         "a100-training",
			preset:       g5A100Preset(),
			gpuClass:     "a100-nvlink-80gb",
			queue:        "team-b",
			clusterQ:     "tau-cq",
			expectedTeam: "training",
		},
		{
			name:         "h200-research",
			preset:       g5H200Preset(),
			gpuClass:     "h200-nvlink-141gb",
			queue:        "team-a",
			clusterQ:     "tau-cq",
			expectedTeam: "research",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
				Preset:       tc.preset,
				NodeSelector: map[string]string{topology.NodeLabelGPUClass: tc.gpuClass},
			})
			if err != nil {
				t.Fatalf("ValidateSelection: %v", err)
			}
			if report.QueueName != tc.queue || report.ClusterQueue != tc.clusterQ {
				t.Fatalf("report = %+v, want queue=%s clusterQueue=%s", report, tc.queue, tc.clusterQ)
			}
			if tc.preset.Options.Team != tc.expectedTeam {
				t.Fatalf("preset team = %q, want %q", tc.preset.Options.Team, tc.expectedTeam)
			}
		})
	}
}

func TestValidateSelectionRejectsClusterQueueDrift(t *testing.T) {
	runner := g5ValidationRunner()
	runner.outputs[validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-b", "-o", "json")] = localQueueObject("team-b", "other-cq", nil)

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5A100Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: "a100-nvlink-80gb"},
	})
	if err == nil || !strings.Contains(err.Error(), `points to ClusterQueue "other-cq"`) || !strings.Contains(err.Error(), `expects "tau-cq"`) {
		t.Fatalf("expected ClusterQueue drift error, got %v", err)
	}
	if !strings.Contains(err.Error(), "inspect that LocalQueue on the workspace cluster") {
		t.Fatalf("ClusterQueue drift error lacks workspace-safe remediation: %v", err)
	}
}

func TestValidateSelectionRejectsTeamLabelDrift(t *testing.T) {
	runner := g5ValidationRunner()
	runner.outputs[validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-b", "-o", "json")] = localQueueObject("team-b", "tau-cq", map[string]string{
		topology.LabelTeam: "research",
	})

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5A100Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: "a100-nvlink-80gb"},
	})
	if err == nil || !strings.Contains(err.Error(), topology.LabelTeam) || !strings.Contains(err.Error(), "research") {
		t.Fatalf("expected team label drift error, got %v", err)
	}
}

func TestValidateSelectionReportsMissingLocalQueueWithPresetContext(t *testing.T) {
	runner := g5ValidationRunner()
	key := validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-b", "-o", "json")
	delete(runner.outputs, key)
	runner.errors[key] = fmt.Errorf("not found")

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5A100Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: "a100-nvlink-80gb"},
	})
	if err == nil {
		t.Fatal("expected missing LocalQueue error")
	}
	for _, want := range []string{"azure.research.training.l", `LocalQueue "team-b"`, `namespace "ray"`, "ask the platform owner to validate preset azure.research.training.l"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing LocalQueue error missing %q: %v", want, err)
		}
	}
}

func TestValidateSelectionRejectsQueueTooSmallForGPUCount(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "team-b", "-o", "json"): localQueueObject("team-b", "tau-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"):            clusterQueueObjectWithGPU("tau-cq", "gpu", 2, 2, nil),
		},
		errors: map[string]error{},
	}
	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:    "ray",
		QueueName:    "team-b",
		GPUCount:     8,
		NodeSelector: map[string]string{"agentpool": "h200pool"},
	})
	if err == nil {
		t.Fatal("expected queue capacity error")
	}
	for _, want := range []string{"team-b", "tau-cq", "at most 4", "requests 8", "policy.queue: auto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("capacity error missing %q: %v", want, err)
		}
	}
}

func TestValidateSelectionRejectsTopologyFreeWorkspaceQueue(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              clusterQueueObjectWithGPU("workspace-cq", "workspace-gpu", 8, 0, nil),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), `does not provide topology "default-node-topology"`) {
		t.Fatalf("expected topology-free workspace queue rejection, got %v", err)
	}
}

func TestValidateSelectionRejectsMissingTopologyWhenAllGPUFlavorsRequireTAS(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]},
					{"name":"tau-system","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): resourceFlavorObject("tau-system", "", "default-node-topology", ""),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "workspace",
		QueueName:       "jobqueue",
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil {
		t.Fatal("expected missing topology intent to be rejected for a TAS-only GPU queue")
	}
	for _, want := range []string{"policy.topology", "TopologyAwareScheduling", "nd-h200-v5", "tau-system"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("topology preflight error missing %q: %v", want, err)
		}
	}
}

func TestValidateSelectionAcceptsMissingTopologyWithCompatibleNonTASFlavor(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]},
					{"name":"legacy-gpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "legacy-gpu", "-o", "json"): resourceFlavorObject("legacy-gpu", "", "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
		},
		errors: map[string]error{},
	}

	if _, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "workspace",
		QueueName:       "jobqueue",
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	}); err != nil {
		t.Fatalf("mixed TAS/non-TAS queue should accept missing topology: %v", err)
	}
}

func TestValidateSelectionRejectsUntoleratedNonTASFlavor(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]},
					{"name":"untolerated-legacy","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):         resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "untolerated-legacy", "-o", "json"): resourceFlavorObject("untolerated-legacy", "", "", "legacy-only"),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "workspace",
		QueueName:       "jobqueue",
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
		PodTolerations: [][]kueueapi.Toleration{{
			{Key: "sku", Operator: "Equal", Value: "gpu", Effect: "NoSchedule"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "TopologyAwareScheduling (nd-h200-v5)") {
		t.Fatalf("untolerated non-TAS flavor should not bypass TAS-only rejection: %v", err)
	}
}

func TestResourceFlavorTaintMatchingUsesPodAndFlavorTolerations(t *testing.T) {
	var rf kueueapi.ResourceFlavor
	rf.Spec.NodeTaints = []kueueapi.Taint{{Key: "sku", Value: "legacy", Effect: "NoSchedule"}}
	pods := [][]kueueapi.Toleration{{{Key: "sku", Operator: "Equal", Value: "gpu", Effect: "NoSchedule"}}}
	if resourceFlavorMatchesPodTolerations(rf, pods) {
		t.Fatal("mismatched PodSet toleration accepted ResourceFlavor nodeTaint")
	}

	rf.Spec.Tolerations = []kueueapi.Toleration{{Key: "sku", Operator: "Exists", Effect: "NoSchedule"}}
	if !resourceFlavorMatchesPodTolerations(rf, pods) {
		t.Fatal("ResourceFlavor toleration did not supplement PodSet tolerations")
	}

	rf.Spec.Tolerations = nil
	rf.Spec.NodeTaints = []kueueapi.Taint{{Key: "soft", Effect: "PreferNoSchedule"}}
	if !resourceFlavorMatchesPodTolerations(rf, nil) {
		t.Fatal("PreferNoSchedule taint should not affect Kueue flavor admission")
	}
}

func TestPodToleratesTaintComparisonOperators(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toleration kueueapi.Toleration
		taint      kueueapi.Taint
		want       bool
	}{
		{name: "less than", toleration: kueueapi.Toleration{Key: "rank", Operator: "Lt", Value: "8"}, taint: kueueapi.Taint{Key: "rank", Value: "4"}, want: true},
		{name: "greater than", toleration: kueueapi.Toleration{Key: "rank", Operator: "Gt", Value: "2"}, taint: kueueapi.Taint{Key: "rank", Value: "4"}, want: true},
		{name: "wrong direction", toleration: kueueapi.Toleration{Key: "rank", Operator: "Lt", Value: "2"}, taint: kueueapi.Taint{Key: "rank", Value: "4"}},
		{name: "non-numeric", toleration: kueueapi.Toleration{Key: "rank", Operator: "Gt", Value: "small"}, taint: kueueapi.Taint{Key: "rank", Value: "4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podToleratesTaint([]kueueapi.Toleration{tc.toleration}, tc.taint); got != tc.want {
				t.Fatalf("podToleratesTaint() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestValidateSelectionSkipsTopologyCapabilityForCPUOnlyConfig(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              clusterQueueObjectWithGPU("workspace-cq", "nd-h200-v5", 16, 0, nil),
		},
		errors: map[string]error{},
	}

	if _, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace: "workspace",
		QueueName: "jobqueue",
		GPUCount:  0,
	}); err != nil {
		t.Fatalf("CPU-only selection should not require GPU topology intent: %v", err)
	}
	for _, call := range runner.calls {
		if containsString(call, "resourceflavor.kueue.x-k8s.io") {
			t.Fatalf("CPU-only validation read ResourceFlavor capabilities: %v", call)
		}
	}
}

func TestValidateSelectionAcceptsTopologyCapableWorkspaceQueue(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              clusterQueueObjectWithGPU("workspace-cq", "nd-h200-v5", 8, 0, nil),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.ClusterQueue != "workspace-cq" || report.ResourceFlavor != "nd-h200-v5" || report.TopologyName != "default-node-topology" {
		t.Fatalf("report = %+v", report)
	}
}

func TestValidateSelectionChoosesTopologyFlavorThatFits(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"ndm-a100-v4","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}
				]}]}
			}`,
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUCount:                16,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.ResourceFlavor != "ndm-a100-v4" || report.GPUMax != 16 {
		t.Fatalf("report = %+v, want fitting ndm-a100-v4 flavor with 16 GPUs", report)
	}
}

func TestValidateSelectionReportsLargestTopologyFlavorWhenNoneFit(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"ndm-a100-v4","resources":[{"name":"nvidia.com/gpu","nominalQuota":"4"}]}
				]}]}
			}`,
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUCount:                16,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), "at most 8") || !strings.Contains(err.Error(), "requests 16") {
		t.Fatalf("expected largest compatible flavor capacity error, got %v", err)
	}
}

func TestValidateSelectionRejectsTopologyFlavorWithConflictingNodeLabels(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              clusterQueueObjectWithGPU("workspace-cq", "nd-h200-v5", 8, 0, nil),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		NodeSelector:            map[string]string{topology.ManagedGPUSeriesLabel: "ndm-a100-v4"},
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), "does not provide topology") {
		t.Fatalf("expected conflicting TAS flavor rejection, got %v", err)
	}
}

func TestSelectQueueChoosesVisibleQueueThatFitsShape(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
					{"metadata":{"name":"team-b","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}},
					{"metadata":{"name":"dev","namespace":"ray"},"spec":{"clusterQueue":"gpu-cq"}}
				]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): clusterQueueObjectWithGPU("tau-cq", "gpu", 2, 2, nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "gpu-cq", "-o", "json"): clusterQueueObjectWithGPU("gpu-cq", "h200-managed", 16, 0, nil),
		},
		errors: map[string]error{},
	}
	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:    "ray",
		GPUCount:     8,
		GPUClass:     topology.GPUClassAny,
		NodeSelector: map[string]string{"agentpool": "h200pool"},
	})
	if err != nil {
		t.Fatalf("SelectQueue: %v", err)
	}
	if selected.QueueName != "dev" || selected.ClusterQueue != "gpu-cq" || selected.ResourceFlavor != "h200-managed" {
		t.Fatalf("selected=%+v, want dev/gpu-cq/h200-managed; candidates=%+v", selected, candidates)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestSelectQueueFiltersByGPUResourceMode(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}},
				{"metadata":{"name":"jobqueue-dra","namespace":"ray"},"spec":{"clusterQueue":"tau-dra-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}]}]}
			}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-dra-cq", "-o", "json"): `{
				"metadata":{"name":"tau-dra-cq"},
				"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5-dra","resources":[{"name":"gpu.nvidia.com","nominalQuota":"16"}]}]}]}
			}`,
		},
		errors: map[string]error{},
	}

	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        8,
		GPUClass:        topology.GPUClassAny,
		GPUResourceName: kueueapi.GPUResource,
	})
	if err != nil {
		t.Fatalf("SelectQueue: %v; candidates=%+v", err, candidates)
	}
	if selected.QueueName != "jobqueue-dra" || selected.ClusterQueue != "tau-dra-cq" {
		t.Fatalf("selected=%+v, want DRA queue", selected)
	}
	for _, candidate := range candidates {
		if candidate.QueueName == "jobqueue" && candidate.Reason != "no matching GPU quota found" {
			t.Fatalf("device-plugin queue reason = %q", candidate.Reason)
		}
	}
}

func localQueueObject(name, clusterQueue string, labels map[string]string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"ray","labels":%s},"spec":{"clusterQueue":%q}}`, name, labelsJSON(labels), clusterQueue)
}

func clusterQueueObject(name, flavor string, labels map[string]string) string {
	return clusterQueueObjectWithGPU(name, flavor, 8, 0, labels)
}

func clusterQueueObjectWithGPU(name, flavor string, nominal, borrowing int64, labels map[string]string) string {
	return fmt.Sprintf(`{
  "metadata": {"name": %q, "labels": %s},
  "spec": {
    "resourceGroups": [
      {"flavors": [{"name": %q, "resources": [{"name": "nvidia.com/gpu", "nominalQuota": "%d", "borrowingLimit": "%d"}]}]}
    ]
  }
}`, name, labelsJSON(labels), flavor, nominal, borrowing)
}

func resourceFlavorObject(name, gpuClass, topologyName, taintKey string) string {
	nodeLabel := ""
	if gpuClass != "" {
		nodeLabel = fmt.Sprintf(`"%s":%q`, topology.NodeLabelGPUClass, gpuClass)
	}
	topologyField := ""
	if topologyName != "" {
		topologyField = fmt.Sprintf(`,"topologyName":%q`, topologyName)
	}
	taints := ""
	if taintKey != "" {
		taints = fmt.Sprintf(`,"nodeTaints":[{"key":%q,"effect":"NoSchedule"}]`, taintKey)
	}
	return fmt.Sprintf(`{"metadata":{"name":%q},"spec":{"nodeLabels":{%s}%s%s}}`, name, nodeLabel, topologyField, taints)
}

func labelsJSON(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
