// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package topology

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func topologyProfile() profile.Profile {
	return profile.Profile{
		Name: "ai-train-a100-nvlink",
		Lane: "training",
		Topology: profile.Topology{
			Team:                      "research",
			Mode:                      "fixed",
			Placement:                 "single-node-nvlink",
			GPUClass:                  GPUClassA10080GB,
			Shape:                     "8xa100-80gb",
			WorkloadPriorityClassName: "taugrid-batch",
		},
	}
}

func TestSystemNodeAffinitySupportsAKSAndPortableClusters(t *testing.T) {
	affinity := SystemNodeAffinity()
	rendered := fmt.Sprint(affinity)
	for _, want := range []string{AKSNodePoolModeLabel, AKSSystemNodePoolMode, "In", "DoesNotExist"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("system affinity missing %q: %v", want, affinity)
		}
	}
}

func TestWithoutKueueTopologyAnnotations(t *testing.T) {
	annotations := map[string]string{
		requiredTopologyAnnotation:         hostnameTopology,
		preferredTopologyAnnotation:        "topology.kubernetes.io/zone",
		unconstrainedTopologyAnnot:         "true",
		workloadmeta.AnnotationWorkspaceID: "workspace-123",
	}
	filtered := WithoutKueueTopologyAnnotations(annotations)
	for _, key := range []string{requiredTopologyAnnotation, preferredTopologyAnnotation, unconstrainedTopologyAnnot} {
		if _, ok := filtered[key]; ok {
			t.Errorf("filtered annotations retained %q: %v", key, filtered)
		}
	}
	if got := filtered[workloadmeta.AnnotationWorkspaceID]; got != "workspace-123" {
		t.Errorf("non-topology annotation=%q, want workspace-123", got)
	}
	if got := annotations[requiredTopologyAnnotation]; got != hostnameTopology {
		t.Errorf("input annotations mutated: %v", annotations)
	}
}

func TestBuild_ProtectedNVLinkPlan(t *testing.T) {
	plan, err := Build(topologyProfile(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.QueueName != SharedGPUQueueName {
		t.Fatalf("queue=%q want %q", plan.QueueName, SharedGPUQueueName)
	}
	if plan.Labels[workloadPriorityLabel] != "taugrid-batch" {
		t.Fatalf("missing workload priority label: %v", plan.Labels)
	}
	if got := plan.NodeSelector[NodeLabelGPUClass]; got != GPUClassA10080GB {
		t.Fatalf("gpu class selector=%q want %q", got, GPUClassA10080GB)
	}
	if got := plan.Labels[LabelGPUClass]; got != GPUClassA10080GB {
		t.Fatalf("gpu class label=%q want %q", got, GPUClassA10080GB)
	}
	if plan.PodPriorityClassName != DefaultTrainPodPriority {
		t.Fatalf("training pod priority=%q want %q", plan.PodPriorityClassName, DefaultTrainPodPriority)
	}
	if plan.Annotations[requiredTopologyAnnotation] != hostnameTopology {
		t.Fatalf("required topology annotation=%q", plan.Annotations[requiredTopologyAnnotation])
	}
}

func TestBuild_DRAPlanCanDisableKueueTASAnnotations(t *testing.T) {
	plan, err := Build(topologyProfile(), Options{
		Placement:                       "single-node-nvlink",
		DisableKueueTopologyAnnotations: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if plan.Annotations[requiredTopologyAnnotation] != "" ||
		plan.Annotations[preferredTopologyAnnotation] != "" ||
		plan.Annotations[unconstrainedTopologyAnnot] != "" {
		t.Fatalf("DRA plan should omit Kueue TAS annotations: %v", plan.Annotations)
	}
	for key := range plan.Labels {
		if strings.HasPrefix(key, workloadmeta.Domain) && key != LabelGPUClass {
			t.Fatalf("Tau-specific topology labels should be omitted: %v", plan.Labels)
		}
	}

	if got := plan.NodeSelector[NodeLabelGPUClass]; got != GPUClassA10080GB {
		t.Fatalf("DRA gpu class selector=%q want %q", got, GPUClassA10080GB)
	}
	if plan.Labels[workloadPriorityLabel] != "taugrid-batch" {
		t.Fatalf("missing workload priority label: %v", plan.Labels)
	}
}

func TestBuild_ResourceFlavorRequiredTopology(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "managed-gpu"}, Options{
		QueueName:        SharedGPUQueueName,
		RequiredTopology: hostnameTopology,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Annotations[RequiredTopologyAnnotation]; got != hostnameTopology {
		t.Fatalf("required topology annotation=%q, want %q", got, hostnameTopology)
	}
}

func TestBuild_ResourceFlavorRequiredTopologyRejectsConflictingPlacement(t *testing.T) {
	_, err := Build(profile.Profile{Name: "managed-gpu"}, Options{
		QueueName:        SharedGPUQueueName,
		Placement:        "independent",
		RequiredTopology: hostnameTopology,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts with placement=independent") {
		t.Fatalf("expected managed topology conflict, got %v", err)
	}
}

func TestBuild_AnyGPUClassDoesNotPinNodeSelector(t *testing.T) {
	plan, err := Build(topologyProfile(), Options{
		Placement: "independent",
		GPUClass:  GPUClassAny,
		Shape:     "1xgpu",
		QueueName: SharedGPUQueueName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.QueueName != SharedGPUQueueName {
		t.Fatalf("queue=%q want %q", plan.QueueName, SharedGPUQueueName)
	}
	if got := plan.Labels[LabelGPUClass]; got != GPUClassAny {
		t.Fatalf("gpu class label=%q want %q", got, GPUClassAny)
	}
	if len(plan.NodeSelector) != 0 {
		t.Fatalf("gpuClass=any should not add node selector: %v", plan.NodeSelector)
	}
}

func TestBuild_PriorityTierSelectsManagedClasses(t *testing.T) {
	p := topologyProfile()
	plan, err := Build(p, Options{PriorityTier: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Labels[workloadPriorityLabel] != priorityTrainWorkloadPrio {
		t.Fatalf("workload priority=%q want %q", plan.Labels[workloadPriorityLabel], priorityTrainWorkloadPrio)
	}
	if plan.PodPriorityClassName != priorityTrainPodPriority {
		t.Fatalf("pod priority=%q want %q", plan.PodPriorityClassName, priorityTrainPodPriority)
	}
}

func TestBuild_PriorityTierDefaultsToTrainingWithoutLane(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "adhoc"}, Options{PriorityTier: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Labels[workloadPriorityLabel] != priorityTrainWorkloadPrio {
		t.Fatalf("workload priority=%q want %q", plan.Labels[workloadPriorityLabel], priorityTrainWorkloadPrio)
	}
	if plan.PodPriorityClassName != priorityTrainPodPriority {
		t.Fatalf("pod priority=%q want %q", plan.PodPriorityClassName, priorityTrainPodPriority)
	}
}

func TestBuild_OverridesRouteTeamLaneQueue(t *testing.T) {
	p := topologyProfile()
	p.Topology.WorkloadPriorityClassName = ""
	plan, err := Build(p, Options{
		Team:      "Experimental",
		Lane:      "elastic",
		Mode:      "elastic",
		Placement: "independent",
		GPUClass:  GPUClassH10095GB,
		Shape:     "1xh100-95gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.QueueName != SharedGPUQueueName {
		t.Fatalf("queue=%q want %q", plan.QueueName, SharedGPUQueueName)
	}
	if plan.Labels[workloadPriorityLabel] != DefaultElasticWorkloadPrio {
		t.Fatalf("elastic workload priority=%q", plan.Labels[workloadPriorityLabel])
	}
	if plan.PodPriorityClassName != defaultElasticPodPriority {
		t.Fatalf("elastic pod priority=%q", plan.PodPriorityClassName)
	}
	if plan.Annotations[unconstrainedTopologyAnnot] != "true" {
		t.Fatalf("elastic independent job should be unconstrained: %v", plan.Annotations)
	}
}

func TestBuild_DeniesH200Elastic(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		Team:            "research",
		Lane:            "elastic",
		Mode:            "elastic",
		Placement:       "independent",
		GPUClass:        GPUClassH200141GB,
		CheckpointEvery: "15m",
	})
	if err == nil || !strings.Contains(err.Error(), "h200") {
		t.Fatalf("expected h200 denial, got %v", err)
	}
}

func TestBuild_DeniesH200OutsideLargeMemory(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		Team:      "research",
		Lane:      "training",
		Mode:      "fixed",
		Placement: "single-node-nvlink",
		GPUClass:  GPUClassH200141GB,
		Shape:     "8xh200-141gb",
	})
	if err == nil || !strings.Contains(err.Error(), "lane=large-memory") {
		t.Fatalf("expected large-memory reservation error, got %v", err)
	}
}

func TestBuild_H100ClassDoesNotConstrainPlacement(t *testing.T) {
	for _, placement := range []string{"single-node-nvlink", "multi-node-nccl"} {
		t.Run(placement, func(t *testing.T) {
			if _, err := Build(topologyProfile(), Options{
				Team:      "research",
				Lane:      "training",
				Mode:      "fixed",
				Placement: placement,
				GPUClass:  GPUClassH10095GB,
			}); err != nil {
				t.Fatalf("Build() error = %v", err)
			}
		})
	}
}

func TestNormalizeGPUClassLegacyAliases(t *testing.T) {
	for legacy, want := range map[string]string{
		"a100-nvlink-80gb":     GPUClassA10080GB,
		"h100-standalone-95gb": GPUClassH10095GB,
		"h200-nvlink-141gb":    GPUClassH200141GB,
	} {
		got, deprecated := NormalizeGPUClass(legacy)
		if got != want || !deprecated {
			t.Errorf("NormalizeGPUClass(%q)=(%q,%t), want (%q,true)", legacy, got, deprecated, want)
		}
	}
	for _, canonical := range []string{GPUClassAny, GPUClassA10080GB, GPUClassH10095GB, GPUClassH200141GB} {
		got, deprecated := NormalizeGPUClass(canonical)
		if got != canonical || deprecated {
			t.Errorf("NormalizeGPUClass(%q)=(%q,%t), want (%q,false)", canonical, got, deprecated, canonical)
		}
	}
}

func TestBuildNormalizesLegacyGPUClassBeforeRendering(t *testing.T) {
	p := topologyProfile()
	p.Topology.GPUClass = "a100-nvlink-80gb"

	plan, err := Build(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.NodeSelector[NodeLabelGPUClass]; got != GPUClassA10080GB {
		t.Fatalf("legacy alias rendered selector %q, want %q", got, GPUClassA10080GB)
	}
	if got := plan.Labels[LabelGPUClass]; got != GPUClassA10080GB {
		t.Fatalf("legacy alias rendered label %q, want %q", got, GPUClassA10080GB)
	}
}

func TestResolveGPUClassUsesProfileAndExplicitOverride(t *testing.T) {
	p := profile.Profile{
		Topology: profile.Topology{GPUClass: "a100-nvlink-80gb"},
	}
	if got, deprecated := ResolveGPUClass(p, ""); got != GPUClassA10080GB || !deprecated {
		t.Fatalf("profile class = %q deprecated=%v, want %q true", got, deprecated, GPUClassA10080GB)
	}
	if got, deprecated := ResolveGPUClass(p, GPUClassAny); got != GPUClassAny || deprecated {
		t.Fatalf("override class = %q deprecated=%v, want %q false", got, deprecated, GPUClassAny)
	}
}

func TestBuild_NoTopologyIntentNoops(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "plain"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 0 || len(plan.Annotations) != 0 || plan.QueueName != "" {
		t.Fatalf("plain profile should produce empty plan: %#v", plan)
	}
}
