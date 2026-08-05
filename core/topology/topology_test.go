package topology

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func topologyProfile() profile.Profile {
	return profile.Profile{
		Name: "ai-train-a100-nvlink",
		Spec: map[string]any{
			"lane": "train",
			"policy": map[string]any{
				"preemptable":         true,
				"checkpointOnPreempt": true,
			},
			"topology": map[string]any{
				"team":                      "research",
				"lane":                      "training",
				"mode":                      "fixed",
				"placement":                 "single-node-nvlink",
				"gpuClass":                  "a100-nvlink-80gb",
				"shape":                     "8xa100-80gb",
				"workloadPriorityClassName": "taugrid-batch",
			},
		},
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
	if len(plan.NodeSelector) != 0 {
		t.Fatalf("Tau should not add node selectors: %v", plan.NodeSelector)
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
		if strings.HasPrefix(key, workloadmeta.Domain) {
			t.Fatalf("Tau-specific topology labels should be omitted: %v", plan.Labels)
		}
	}
	if plan.Labels[workloadPriorityLabel] != "taugrid-batch" {
		t.Fatalf("missing workload priority label: %v", plan.Labels)
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
	if _, ok := plan.Labels[LabelGPUClass]; ok {
		t.Fatalf("Tau-specific gpu class label should be omitted: %v", plan.Labels)
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
	delete(p.Spec["topology"].(map[string]any), "workloadPriorityClassName")
	plan, err := Build(p, Options{
		Team:      "Experimental",
		Lane:      "elastic",
		Mode:      "elastic",
		Placement: "independent",
		GPUClass:  "h100-standalone-95gb",
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

func TestBuild_ElasticRequiresCheckpoint(t *testing.T) {
	p := topologyProfile()
	p.Spec["policy"] = map[string]any{"preemptable": true}
	_, err := Build(p, Options{
		Team:      "research",
		Lane:      "elastic",
		Mode:      "elastic",
		Placement: "independent",
		GPUClass:  "h100-standalone-95gb",
	})
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("expected checkpoint error, got %v", err)
	}
}

func TestBuild_DeniesH200Elastic(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		Team:            "research",
		Lane:            "elastic",
		Mode:            "elastic",
		Placement:       "independent",
		GPUClass:        "h200-nvlink-141gb",
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
		GPUClass:  "h200-nvlink-141gb",
		Shape:     "8xh200-141gb",
	})
	if err == nil || !strings.Contains(err.Error(), "lane=large-memory") {
		t.Fatalf("expected large-memory reservation error, got %v", err)
	}
}

func TestBuild_DeniesStandaloneNVLink(t *testing.T) {
	_, err := Build(topologyProfile(), Options{
		Team:      "research",
		Lane:      "training",
		Mode:      "fixed",
		Placement: "single-node-nvlink",
		GPUClass:  "h100-standalone-95gb",
	})
	if err == nil || !strings.Contains(err.Error(), "single-node-nvlink") {
		t.Fatalf("expected topology mismatch error, got %v", err)
	}
}

func TestBuild_NoTopologyIntentNoops(t *testing.T) {
	plan, err := Build(profile.Profile{Name: "plain", Spec: map[string]any{}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 0 || len(plan.Annotations) != 0 || plan.QueueName != "" {
		t.Fatalf("plain profile should produce empty plan: %#v", plan)
	}
}
