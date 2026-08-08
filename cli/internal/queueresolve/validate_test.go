// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queueresolve

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kueueapi"
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
			ResourceFlavor: "ndm-a100-v4",
		},
		Options: topology.Options{
			Team:      "training",
			Lane:      "training",
			GPUClass:  topology.GPUClassA10080GB,
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
			ResourceFlavor: "nd-h200-v5",
		},
		Options: topology.Options{
			Team:      "research",
			Lane:      "large-memory",
			GPUClass:  topology.GPUClassH200141GB,
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
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"ndm-a100-v4","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "ndm-a100-v4", "-o", "json"): resourceFlavorObject("ndm-a100-v4", topology.GPUClassA10080GB, "", "sku-specific-taint"),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):  resourceFlavorObject("nd-h200-v5", topology.GPUClassH200141GB, "", "sku-specific-taint"),
		},
		errors: map[string]error{},
	}
}

func TestValidateSelectionAcceptsExactGPUClassFlavorLabel(t *testing.T) {
	runner := g5ValidationRunner()

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5H200Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassH200141GB},
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.Namespace != "ray" ||
		report.QueueName != "team-a" ||
		report.ClusterQueue != "tau-cq" ||
		report.ResourceFlavor != "nd-h200-v5" ||
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
		{"get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestValidateSelectionRejectsUnlabeledFlavorForSpecificGPUClass(t *testing.T) {
	runner := g5ValidationRunner()
	runner.outputs[validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json")] =
		resourceFlavorObject("nd-h200-v5", "", "", "")

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Preset:       g5H200Preset(),
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassH200141GB},
	})
	if err == nil || !strings.Contains(err.Error(), topology.NodeLabelGPUClass) || !strings.Contains(err.Error(), "exact node-label match") {
		t.Fatalf("expected exact gpu class label error, got %v", err)
	}
}

func TestValidateSelectionExplicitQueueResolvesExactGPUClassFlavor(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "tau-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"looks-like-h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"64"}]},
					{"name":"opaque-a100","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "looks-like-h200", "-o", "json"): resourceFlavorObject("looks-like-h200", topology.GPUClassH10095GB, "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "opaque-a100", "-o", "json"):     resourceFlavorObject("opaque-a100", topology.GPUClassA10080GB, "", ""),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "ray",
		QueueName:       "jobqueue",
		GPUClass:        topology.GPUClassA10080GB,
		NodeSelector:    map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.ResourceFlavor != "opaque-a100" {
		t.Fatalf("ResourceFlavor=%q, want opaque-a100 exact-label match", report.ResourceFlavor)
	}
}

func TestValidateSelectionExplicitQueueRejectsUnavailableGPUClass(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "tau-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"):              clusterQueueObjectWithGPU("tau-cq", "misleading-a100-name", 8, 0, nil),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "misleading-a100-name", "-o", "json"): resourceFlavorObject(
				"misleading-a100-name", topology.GPUClassH10095GB, "", ""),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "ray",
		QueueName:       "jobqueue",
		GPUClass:        topology.GPUClassA10080GB,
		NodeSelector:    map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), "no compatible GPU quota flavor with exact node label") {
		t.Fatalf("expected explicit queue unavailable-class error, got %v", err)
	}
}

func TestValidateSelectionExplicitQueueRejectsFlavorConstraintConflicts(t *testing.T) {
	tests := []struct {
		name          string
		flavor        string
		nodeSelector  map[string]string
		podToleration [][]kueueapi.Toleration
	}{
		{
			name: "conflicting node label",
			flavor: resourceFlavorObjectWithLabels(
				"a100-pool",
				map[string]string{
					topology.NodeLabelGPUClass: topology.GPUClassA10080GB,
					"agentpool":                "gpu",
				},
				"",
				"",
			),
			nodeSelector: map[string]string{
				topology.NodeLabelGPUClass: topology.GPUClassA10080GB,
				"agentpool":                "system",
			},
		},
		{
			name:         "untolerated node taint",
			flavor:       resourceFlavorObject("a100-pool", topology.GPUClassA10080GB, "", "dedicated"),
			nodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &validationFakeRunner{
				outputs: map[string]string{
					validationKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "tau-cq", nil),
					validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"):              clusterQueueObjectWithGPU("tau-cq", "a100-pool", 8, 0, nil),
					validationKey("get", "resourceflavor.kueue.x-k8s.io", "a100-pool", "-o", "json"):         tc.flavor,
				},
				errors: map[string]error{},
			}

			_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
				Namespace:       "ray",
				QueueName:       "jobqueue",
				GPUClass:        topology.GPUClassA10080GB,
				NodeSelector:    tc.nodeSelector,
				PodTolerations:  tc.podToleration,
				GPUCount:        1,
				GPUResourceName: kueueapi.GPUResourceDevicePlugin,
			})
			if err == nil || !strings.Contains(err.Error(), "rendered pod constraints") {
				t.Fatalf("expected incompatible flavor rejection, got %v", err)
			}
		})
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
			gpuClass:     topology.GPUClassA10080GB,
			queue:        "team-b",
			clusterQ:     "tau-cq",
			expectedTeam: "training",
		},
		{
			name:         "h200-research",
			preset:       g5H200Preset(),
			gpuClass:     topology.GPUClassH200141GB,
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
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
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
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
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
		NodeSelector: map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "workspace-gpu", "-o", "json"):           resourceFlavorObject("workspace-gpu", "", "", ""),
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

func TestValidateSelectionReturnsManagedRequiredTopology(t *testing.T) {
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithRequiredTopology("nd-h200-v5", "", "default-node-topology", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): resourceFlavorObjectWithRequiredTopology("tau-system", "", "default-node-topology", "kubernetes.io/hostname", ""),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "workspace",
		QueueName:       "jobqueue",
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RequiredTopology != "kubernetes.io/hostname" || report.ResourceFlavor != "nd-h200-v5" {
		t.Fatalf("managed topology report=%+v", report)
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
	if err == nil || !strings.Contains(err.Error(), topology.RequiredTopologyAnnotation) || !strings.Contains(err.Error(), "nd-h200-v5") {
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):              resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
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

func TestValidateSelectionCatalogTopologyReturnsConsistentManagedRequirement(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"tau-system","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"nd-h200-v5", "", "default-node-topology", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"tau-system", "", "default-node-topology", "kubernetes.io/hostname", ""),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RequiredTopology != "kubernetes.io/hostname" {
		t.Fatalf("report=%+v", report)
	}
}

func TestValidateSelectionCatalogTopologyRejectsConflictingManagedRequirements(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"tau-system","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"nd-h200-v5", "", "default-node-topology", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"tau-system", "", "default-node-topology", "topology.kubernetes.io/zone", ""),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") || !strings.Contains(err.Error(), topology.RequiredTopologyAnnotation) {
		t.Fatalf("expected conflicting catalog topology rejection, got %v", err)
	}
}

func TestValidateSelectionCatalogTopologyChecksAssignableFlavorsWithOtherTopologies(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"default-topology","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"rack-topology","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "default-topology", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"default-topology", "", "default-node-topology", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "rack-topology", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"rack-topology", "", "rack-node-topology", "topology.example.com/rack", ""),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") || !strings.Contains(err.Error(), topology.RequiredTopologyAnnotation) {
		t.Fatalf("expected cross-topology flavor conflict rejection, got %v", err)
	}
}

func TestValidateSelectionCatalogTopologyRejectsMissingManagedRequirement(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"tau-system","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"nd-h200-v5", "", "default-node-topology", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): resourceFlavorObject(
				"tau-system", "", "default-node-topology", ""),
		},
		errors: map[string]error{},
	}

	_, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), topology.RequiredTopologyAnnotation) || !strings.Contains(err.Error(), "tau-system") {
		t.Fatalf("expected missing catalog topology metadata rejection, got %v", err)
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):  resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "ndm-a100-v4", "-o", "json"): resourceFlavorObject("ndm-a100-v4", "", "default-node-topology", ""),
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):  resourceFlavorObject("nd-h200-v5", "", "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "ndm-a100-v4", "-o", "json"): resourceFlavorObject("ndm-a100-v4", "", "default-node-topology", ""),
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithLabels(
				"nd-h200-v5",
				map[string]string{topology.ManagedGPUSeriesLabel: "nd-h200-v5"},
				"default-node-topology",
				"",
			),
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

func TestValidateSelectionMatchesTopologyFlavorByExactGPUClassLabel(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"looks-like-a100","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"arbitrary-pool","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "looks-like-a100", "-o", "json"): resourceFlavorObject("looks-like-a100", topology.GPUClassH200141GB, "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "arbitrary-pool", "-o", "json"):  resourceFlavorObject("arbitrary-pool", topology.GPUClassA10080GB, "default-node-topology", ""),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUClass:                topology.GPUClassA10080GB,
		NodeSelector:            map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB},
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.ResourceFlavor != "arbitrary-pool" {
		t.Fatalf("ResourceFlavor = %q, want exact-label arbitrary-pool", report.ResourceFlavor)
	}
}

func TestValidateSelectionAllowsGenericNamedTopologyFlavorForAny(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              clusterQueueObjectWithGPU("workspace-cq", "taugrid-default", 8, 0, nil),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"):         resourceFlavorObject("taugrid-default", "", "default-node-topology", ""),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:               "workspace",
		QueueName:               "jobqueue",
		TopologyName:            "default-node-topology",
		CatalogTopologyContract: true,
		TopologyRequest:         true,
		GPUClass:                topology.GPUClassAny,
		GPUCount:                1,
		GPUResourceName:         kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection: %v", err)
	}
	if report.ResourceFlavor != "taugrid-default" {
		t.Fatalf("ResourceFlavor = %q, want taugrid-default", report.ResourceFlavor)
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "gpu", "-o", "json"): resourceFlavorObjectWithLabels(
				"gpu", map[string]string{"agentpool": "otherpool"}, "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "h200-managed", "-o", "json"): resourceFlavorObjectWithLabels(
				"h200-managed", map[string]string{"agentpool": "h200pool"}, "", ""),
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
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):     resourceFlavorObject("nd-h200-v5", "", "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5-dra", "-o", "json"): resourceFlavorObject("nd-h200-v5-dra", "", "", ""),
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

func TestSelectQueueCarriesManagedRequiredTopology(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"nd-h200-v5", topology.GPUClassH200141GB, "default-node-topology", "kubernetes.io/hostname", ""),
		},
		errors: map[string]error{},
	}

	selected, _, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        1,
		GPUClass:        topology.GPUClassH200141GB,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.RequiredTopology != "kubernetes.io/hostname" {
		t.Fatalf("selected managed topology=%+v", selected)
	}
}

func TestSelectQueueRejectsConflictingManagedRequiredTopology(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"h200-a","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]},
					{"name":"h200-b","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "h200-a", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"h200-a", topology.GPUClassH200141GB, "topology-a", "kubernetes.io/hostname", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "h200-b", "-o", "json"): resourceFlavorObjectWithRequiredTopology(
				"h200-b", topology.GPUClassH200141GB, "topology-b", "cloud.provider.com/rack", ""),
		},
		errors: map[string]error{},
	}

	_, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        1,
		GPUClass:        topology.GPUClassH200141GB,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil {
		t.Fatal("expected automatic queue selection to reject conflicting managed topology")
	}
	if len(candidates) != 1 ||
		!strings.Contains(candidates[0].Reason, "conflicting") ||
		!strings.Contains(candidates[0].Reason, topology.RequiredTopologyAnnotation) {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestSelectQueueMatchesGPUClassByExactResourceFlavorLabel(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"misleading-h200-pool","resources":[{"name":"nvidia.com/gpu","nominalQuota":"64"}]},
					{"name":"opaque-platform-flavor","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "misleading-h200-pool", "-o", "json"):   resourceFlavorObject("misleading-h200-pool", topology.GPUClassA10080GB, "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "opaque-platform-flavor", "-o", "json"): resourceFlavorObject("opaque-platform-flavor", topology.GPUClassH200141GB, "", ""),
		},
		errors: map[string]error{},
	}

	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        8,
		GPUClass:        topology.GPUClassH200141GB,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("SelectQueue: %v; candidates=%+v", err, candidates)
	}
	if selected.ResourceFlavor != "opaque-platform-flavor" {
		t.Fatalf("selected=%+v, want exact-label flavor opaque-platform-flavor", selected)
	}
}

func TestSelectQueueRejectsMisleadingAndUnlabeledFlavors(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"h100-95gb-by-name-only","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
					{"name":"taugrid-default","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "h100-95gb-by-name-only", "-o", "json"): resourceFlavorObject("h100-95gb-by-name-only", topology.GPUClassA10080GB, "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "taugrid-default", "-o", "json"):        resourceFlavorObject("taugrid-default", "", "", ""),
		},
		errors: map[string]error{},
	}

	_, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        1,
		GPUClass:        topology.GPUClassH10095GB,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err == nil || !strings.Contains(err.Error(), `gpu_class "h100-95gb"`) {
		t.Fatalf("expected unavailable gpu_class error, got %v; candidates=%+v", err, candidates)
	}
	if len(candidates) != 1 || !strings.Contains(candidates[0].Reason, topology.NodeLabelGPUClass) {
		t.Fatalf("candidate reason should report exact label miss: %+v", candidates)
	}
}

func TestSelectQueueNormalizesLegacyGPUClassAliasBeforeMatching(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"):        clusterQueueObjectWithGPU("tau-cq", "ndm-a100-v4", 8, 0, nil),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "ndm-a100-v4", "-o", "json"): resourceFlavorObject("ndm-a100-v4", topology.GPUClassA10080GB, "", ""),
		},
		errors: map[string]error{},
	}

	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        1,
		GPUClass:        "a100-nvlink-80gb",
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("SelectQueue legacy alias: %v; candidates=%+v", err, candidates)
	}
	if selected.ResourceFlavor != "ndm-a100-v4" {
		t.Fatalf("selected=%+v, want canonical-label flavor ndm-a100-v4", selected)
	}
}

func TestSelectQueueUsesCompatibleFlavorWhenAnotherIsUnreadable(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"stale-reference","resources":[{"name":"nvidia.com/gpu","nominalQuota":"64"}]},
					{"name":"usable-a100","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "usable-a100", "-o", "json"): resourceFlavorObject(
				"usable-a100", topology.GPUClassA10080GB, "", ""),
		},
		errors: map[string]error{
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "stale-reference", "-o", "json"): fmt.Errorf("not found"),
		},
	}

	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        1,
		GPUClass:        topology.GPUClassA10080GB,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("SelectQueue: %v; candidates=%+v", err, candidates)
	}
	if selected.ResourceFlavor != "usable-a100" {
		t.Fatalf("selected=%+v, want usable-a100", selected)
	}
}

func TestValidateSelectionAnyChoosesCompatibleFlavor(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): localQueueObject("jobqueue", "workspace-cq", nil),
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{
				"metadata":{"name":"workspace-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"misleading-h200-pool","resources":[{"name":"nvidia.com/gpu","nominalQuota":"64"}]},
					{"name":"opaque-compatible","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "misleading-h200-pool", "-o", "json"): resourceFlavorObjectWithLabels(
				"misleading-h200-pool", map[string]string{"agentpool": "wrongpool"}, "default-node-topology", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "opaque-compatible", "-o", "json"): resourceFlavorObjectWithLabels(
				"opaque-compatible", map[string]string{"agentpool": "gpupool"}, "default-node-topology", "nvidia.com/gpu"),
		},
		errors: map[string]error{},
	}

	report, err := ValidateSelection(context.Background(), runner, ValidationOptions{
		Namespace:       "workspace",
		QueueName:       "jobqueue",
		TopologyRequest: true,
		GPUClass:        topology.GPUClassAny,
		NodeSelector:    map[string]string{"agentpool": "gpupool"},
		PodTolerations: [][]kueueapi.Toleration{{
			{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
		}},
		GPUCount:        1,
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("ValidateSelection any: %v", err)
	}
	if report.ResourceFlavor != "opaque-compatible" {
		t.Fatalf("ResourceFlavor = %q, want opaque-compatible", report.ResourceFlavor)
	}
}

func TestSelectQueueDoesNotRankFlavorNames(t *testing.T) {
	runner := &validationFakeRunner{
		outputs: map[string]string{
			validationKey("-n", "ray", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[
				{"metadata":{"name":"jobqueue","namespace":"ray"},"spec":{"clusterQueue":"tau-cq"}}
			]}`,
			validationKey("get", "clusterqueue.kueue.x-k8s.io", "tau-cq", "-o", "json"): `{
				"metadata":{"name":"tau-cq"},
				"spec":{"resourceGroups":[{"flavors":[
					{"name":"east-small","resources":[{"name":"nvidia.com/gpu","nominalQuota":"4"}]},
					{"name":"opaque-large","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}
				]}]}
			}`,
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "east-small", "-o", "json"): resourceFlavorObjectWithLabels(
				"east-small", map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB, "topology.kubernetes.io/zone": "east"}, "", ""),
			validationKey("get", "resourceflavor.kueue.x-k8s.io", "opaque-large", "-o", "json"): resourceFlavorObjectWithLabels(
				"opaque-large", map[string]string{topology.NodeLabelGPUClass: topology.GPUClassA10080GB, "topology.kubernetes.io/zone": "east"}, "", ""),
		},
		errors: map[string]error{},
	}

	selected, candidates, err := SelectQueue(context.Background(), runner, AutoSelectOptions{
		Namespace:       "ray",
		GPUCount:        8,
		GPUClass:        topology.GPUClassA10080GB,
		NodeSelector:    map[string]string{"topology.kubernetes.io/zone": "east"},
		GPUResourceName: kueueapi.GPUResourceDevicePlugin,
	})
	if err != nil {
		t.Fatalf("SelectQueue: %v; candidates=%+v", err, candidates)
	}
	if selected.ResourceFlavor != "opaque-large" {
		t.Fatalf("selected=%+v, want highest-capacity compatible opaque-large", selected)
	}
}

func localQueueObject(name, clusterQueue string, labels map[string]string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"ray","labels":%s},"spec":{"clusterQueue":%q}}`, name, labelsJSON(labels), clusterQueue)
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
	labels := map[string]string{}
	if gpuClass != "" {
		labels[topology.NodeLabelGPUClass] = gpuClass
	}
	return resourceFlavorObjectWithLabels(name, labels, topologyName, taintKey)
}

func resourceFlavorObjectWithLabels(name string, labels map[string]string, topologyName, taintKey string) string {
	return resourceFlavorObjectWithRequiredTopologyAndLabels(name, labels, topologyName, "", taintKey)
}

func resourceFlavorObjectWithRequiredTopology(name, gpuClass, topologyName, requiredTopology, taintKey string) string {
	labels := map[string]string{}
	if gpuClass != "" {
		labels[topology.NodeLabelGPUClass] = gpuClass
	}
	return resourceFlavorObjectWithRequiredTopologyAndLabels(name, labels, topologyName, requiredTopology, taintKey)
}

func resourceFlavorObjectWithRequiredTopologyAndLabels(name string, labels map[string]string, topologyName, requiredTopology, taintKey string) string {
	topologyField := ""
	if topologyName != "" {
		topologyField = fmt.Sprintf(`,"topologyName":%q`, topologyName)
	}
	annotations := ""
	if requiredTopology != "" {
		annotations = fmt.Sprintf(`,"annotations":{%q:%q}`, topology.RequiredTopologyAnnotation, requiredTopology)
	}
	taints := ""
	if taintKey != "" {
		taints = fmt.Sprintf(`,"nodeTaints":[{"key":%q,"effect":"NoSchedule"}]`, taintKey)
	}
	return fmt.Sprintf(`{"metadata":{"name":%q%s},"spec":{"nodeLabels":%s%s%s}}`, name, annotations, labelsJSON(labels), topologyField, taints)
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
