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
		Name: "ai-train-a100-host",
		Lane: "training",
		Topology: profile.Topology{
			Team:                      "research",
			Mode:                      "fixed",
			Placement:                 "same-host",
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

func TestBuild_SameHostPlan(t *testing.T) {
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
		Placement:                       "same-host",
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

func TestBuild_SameNetworkDomainPlacement(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "managed-gpu"}, Options{
		QueueName: SharedGPUQueueName,
		Placement: profile.PlacementSameNetworkDomain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Annotations[RequiredTopologyAnnotation]; got != networkDomainTopology {
		t.Fatalf("required topology annotation=%q, want %q", got, networkDomainTopology)
	}
}

func TestWithNetworkDomainRequirement(t *testing.T) {
	tests := []struct {
		name    string
		profile profile.Profile
		options Options
		want    string
		wantTAS bool
	}{
		{
			name:    "defaults to network domain",
			options: Options{},
			want:    profile.PlacementSameNetworkDomain,
			wantTAS: true,
		},
		{
			name: "preserves narrower profile placement",
			profile: profile.Profile{Topology: profile.Topology{
				Placement: profile.PlacementSameAcceleratorDomain,
			}},
			options: Options{},
			want:    "",
			wantTAS: true,
		},
		{
			name: "upgrades unconstrained placement and forces TAS",
			options: Options{
				Placement:                       profile.PlacementUnconstrained,
				DisableKueueTopologyAnnotations: true,
			},
			want:    profile.PlacementSameNetworkDomain,
			wantTAS: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WithNetworkDomainRequirement(tc.profile, tc.options)
			if got.Placement != tc.want {
				t.Fatalf("placement=%q, want %q", got.Placement, tc.want)
			}
			if got.DisableKueueTopologyAnnotations == tc.wantTAS {
				t.Fatalf("DisableKueueTopologyAnnotations=%v, want %v", got.DisableKueueTopologyAnnotations, !tc.wantTAS)
			}
		})
	}
}

func TestBuild_SameAcceleratorDomainPlacement(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "managed-gpu"}, Options{
		QueueName: SharedGPUQueueName,
		Placement: profile.PlacementSameAcceleratorDomain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Annotations[RequiredTopologyAnnotation]; got != acceleratorDomainTopology {
		t.Fatalf("required topology annotation=%q, want %q", got, acceleratorDomainTopology)
	}
}

func TestBuild_SameSitePlacement(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "managed-gpu"}, Options{
		QueueName: SharedGPUQueueName,
		Placement: profile.PlacementSameSite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Annotations[RequiredTopologyAnnotation]; got != siteTopology {
		t.Fatalf("required topology annotation=%q, want %q", got, siteTopology)
	}
}

func TestBuild_AnyGPUClassDoesNotPinNodeSelector(t *testing.T) {
	plan, err := Build(topologyProfile(), Options{
		Placement: "unconstrained",
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

func TestBuild_PriorityTierRejectsExplicitClasses(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		PriorityTier:              "priority",
		WorkloadPriorityClassName: "custom-workload",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("Build() error = %v, want priority/class conflict", err)
	}
	_, err = Build(topologyProfile(), Options{
		PriorityTier:             "priority",
		DisableDefaultPriorities: true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("Build() error = %v, want priority/disable conflict", err)
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
		Team:            "Experimental",
		Lane:            "elastic",
		Mode:            "elastic",
		Placement:       "unconstrained",
		GPUClass:        GPUClassH10095GB,
		Shape:           "1xh100-95gb",
		CheckpointEvery: "15m",
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
		t.Fatalf("elastic unconstrained job should be unconstrained: %v", plan.Annotations)
	}
}

func TestBuild_DeniesH200Elastic(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		Team:            "research",
		Lane:            "elastic",
		Mode:            "elastic",
		Placement:       "unconstrained",
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
		Placement: "same-host",
		GPUClass:  GPUClassH200141GB,
		Shape:     "8xh200-141gb",
	})
	if err == nil || !strings.Contains(err.Error(), "lane=large-memory") {
		t.Fatalf("expected large-memory reservation error, got %v", err)
	}
}

func TestBuild_H100ClassDoesNotConstrainPlacement(t *testing.T) {
	for _, placement := range []string{"same-host", "same-accelerator-domain", "same-network-domain"} {
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

func TestNormalizeGPUClass(t *testing.T) {
	for _, canonical := range SupportedGPUClasses() {
		got := NormalizeGPUClass(canonical)
		if got != canonical {
			t.Errorf("NormalizeGPUClass(%q)=%q, want %q", canonical, got, canonical)
		}
		if !IsSupportedGPUClass(canonical) {
			t.Errorf("IsSupportedGPUClass(%q)=false, want true", canonical)
		}
	}
	for _, unsupported := range []string{"a100", "unsupported-gpu"} {
		if IsSupportedGPUClass(unsupported) {
			t.Errorf("IsSupportedGPUClass(%q)=true, want false", unsupported)
		}
	}
}

func TestBuildRejectsUnsupportedGPUClass(t *testing.T) {
	p := topologyProfile()
	p.Topology.GPUClass = "unsupported-gpu"

	if _, err := Build(p, Options{}); err == nil {
		t.Fatal("Build() accepted unsupported GPU class")
	}
}

func TestResolveGPUClassUsesProfileAndExplicitOverride(t *testing.T) {
	p := profile.Profile{
		Topology: profile.Topology{GPUClass: GPUClassA10080GB},
	}
	if got := ResolveGPUClass(p, ""); got != GPUClassA10080GB {
		t.Fatalf("profile class = %q, want %q", got, GPUClassA10080GB)
	}
	if got := ResolveGPUClass(p, GPUClassAny); got != GPUClassAny {
		t.Fatalf("override class = %q, want %q", got, GPUClassAny)
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
