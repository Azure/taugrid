package topology

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPolicyAndResolvePreset(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.training.xl:
      description: Sample XL
      profile: azure-gpu-8x
      team: research
      lane: training
      mode: fixed
      placement: single-node-nvlink
      shape: 8xa100-80gb
      gpuClass: a100-80gb
      queue: research-training
      clusterQueue: team-research-reserved-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb
      topologyName: tau-region-host
      workloadPriorityClassName: taugrid-train-custom
      podPriorityClassName: taugrid-train-custom
      explain: protected A100 island
`)
	got, err := ResolvePreset(path, "azure.research.training.xl")
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicyName != "test-azure" {
		t.Fatalf("policy name=%q", got.PolicyName)
	}
	if got.Preset.Profile != "azure-gpu-8x" {
		t.Fatalf("profile=%q", got.Preset.Profile)
	}
	if got.Options.QueueName != "research-training" || got.Options.Placement != "single-node-nvlink" {
		t.Fatalf("options not mapped: %#v", got.Options)
	}
	if got.Options.WorkloadPriorityClassName != "taugrid-train-custom" || got.Options.PodPriorityClassName != "taugrid-train-custom" {
		t.Fatalf("priority options not mapped: %#v", got.Options)
	}
	if got.Labels[LabelPreset] != "azure.research.training.xl" {
		t.Fatalf("preset label missing: %v", got.Labels)
	}
	if got.Annotations[workloadmeta.AnnotationPresetExplain] != "protected A100 island" {
		t.Fatalf("explain annotation missing: %v", got.Annotations)
	}
	if got.Annotations[workloadmeta.AnnotationWorkloadPriorityClass] != "taugrid-train-custom" {
		t.Fatalf("priority annotation missing: %v", got.Annotations)
	}
	if got.Options.DisableKueueTopologyAnnotations {
		t.Fatalf("topologyName should keep Kueue TAS annotations enabled: %#v", got.Options)
	}
}

func TestLoadPolicyNormalizesLegacyGPUClassAlias(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.training.l:
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xa100-80gb
      gpuClass: a100-nvlink-80gb
      queue: jobqueue
      clusterQueue: tau-cq
`)
	got, err := ResolvePreset(path, "azure.research.training.l")
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.GPUClass != GPUClassA10080GB {
		t.Fatalf("GPUClass=%q want %q", got.Options.GPUClass, GPUClassA10080GB)
	}
}

func TestLoadPolicyDefaultsPriorityByLane(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.eval.gpu:
      profile: azure-gpu-full
      team: research
      lane: eval
      mode: fixed
      placement: independent
      shape: 1xa100-80gb
      gpuClass: a100-80gb
      queue: research-eval
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-a100-nvlink-80gb
`)
	got, err := ResolvePreset(path, "azure.research.eval.gpu")
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.WorkloadPriorityClassName != defaultEvalWorkloadPrio {
		t.Fatalf("workload priority=%q want %q", got.Options.WorkloadPriorityClassName, defaultEvalWorkloadPrio)
	}
	if got.Options.PodPriorityClassName != defaultEvalPodPriority {
		t.Fatalf("pod priority=%q want %q", got.Options.PodPriorityClassName, defaultEvalPodPriority)
	}
}

func TestLoadPolicyReclaimablePresetsUseLowPriority(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.training.l:
      profile: azure-gpu-full
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xa100-80gb
      gpuClass: a100-80gb
      queue: research-training
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-a100-nvlink-80gb-dra
      reclaimable: true
`)
	got, err := ResolvePreset(path, "azure.research.training.l")
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.WorkloadPriorityClassName != DefaultElasticWorkloadPrio {
		t.Fatalf("workload priority=%q want %q", got.Options.WorkloadPriorityClassName, DefaultElasticWorkloadPrio)
	}
	if got.Options.PodPriorityClassName != defaultElasticPodPriority {
		t.Fatalf("pod priority=%q want %q", got.Options.PodPriorityClassName, defaultElasticPodPriority)
	}
	if got.Labels[workloadmeta.LabelReclaimable] != "true" || got.Labels[workloadmeta.LabelPreemptible] != "true" {
		t.Fatalf("reclaimable labels missing: %v", got.Labels)
	}
	if got.Annotations[workloadmeta.LabelReclaimable] != "true" {
		t.Fatalf("reclaimable annotation missing: %v", got.Annotations)
	}
}

func TestResolvePresetDisablesKueueTASWithoutTopologyName(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.training.dra:
      profile: azure-gpu-full
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xa100-80gb
      gpuClass: a100-80gb
      queue: research-training
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-a100-nvlink-80gb-dra
    azure.research.large-memory.h200:
      profile: azure-gpu-full
      team: research
      lane: large-memory
      mode: fixed
      placement: single-node-nvlink
      shape: 8xh200-141gb
      gpuClass: h200-141gb
      queue: research-large-memory
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-h200-nvlink-141gb
`)
	for _, preset := range []string{"azure.research.training.dra", "azure.research.large-memory.h200"} {
		t.Run(preset, func(t *testing.T) {
			got, err := ResolvePreset(path, preset)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Options.DisableKueueTopologyAnnotations {
				t.Fatalf("flavor without topologyName should disable Kueue TAS annotations: %#v", got.Options)
			}
			plan, err := Build(profile.Profile{Name: "test"}, got.Options)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := plan.Annotations[requiredTopologyAnnotation]; ok {
				t.Fatalf("Kueue required topology annotation should be omitted for ResourceFlavor without topologyName: %v", plan.Annotations)
			}
			if _, ok := plan.Annotations[preferredTopologyAnnotation]; ok {
				t.Fatalf("Kueue preferred topology annotation should be omitted for ResourceFlavor without topologyName: %v", plan.Annotations)
			}
		})
	}
}

func TestResolvePresetDisabledErrors(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.large-memory.xl:
      profile: azure-gpu-8x
      team: research
      lane: large-memory
      mode: fixed
      placement: single-node-nvlink
      shape: 8xh200-141gb
      gpuClass: h200-141gb
      queue: research-large-memory
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-h200-nvlink-141gb
      disabled: true
      disabledReason: h200 nodes are not ready
`)
	_, err := ResolvePreset(path, "azure.research.large-memory.xl")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error, got %v", err)
	}
}

func TestLoadPolicyRejectsMIGPreset(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.eval.small:
      profile: eval-small
      team: research
      lane: eval
      mode: fixed
      placement: independent
      shape: mig-1g
      gpuClass: a100-mig-1g
      queue: research-eval
      clusterQueue: team-research-reserved-cq
      resourceFlavor: gpu-a100-mig1g
`)
	_, err := LoadPolicy(path)
	if err == nil || !strings.Contains(err.Error(), "MIG") {
		t.Fatalf("expected MIG rejection, got %v", err)
	}
}

func TestResolvePolicyPathHonorsEnv(t *testing.T) {
	path := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-azure }
spec:
  presets:
    azure.research.training.l:
      profile: azure-gpu-full
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xa100-80gb
      gpuClass: a100-80gb
      queue: research-training
      clusterQueue: team-research-reserved-cq
`)
	t.Setenv(defaultPolicyEnv, path)
	got, err := resolvePolicyPath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path=%q want %q", got, path)
	}
}

func TestResolvePresetUsesEmbeddedDefaultPolicy(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	got, err := ResolvePreset("", "azure.research.training.l")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceFile != embeddedPolicySource {
		t.Fatalf("source=%q want %q", got.SourceFile, embeddedPolicySource)
	}
	if got.Preset.Profile != "" || got.Options.QueueName != SharedGPUQueueName {
		t.Fatalf("embedded preset not resolved correctly: %#v", got)
	}
}

func TestResolvePresetEmbeddedMultiGPUShapes(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	cases := map[string]struct {
		shape    string
		queue    string
		flavor   string
		topology string
	}{
		"azure.research.training.2x":     {"2xgpu", SharedGPUQueueName, "", "independent"},
		"azure.research.training.4x":     {"4xgpu", SharedGPUQueueName, "", "independent"},
		"azure.experimental.training.2x": {"2xgpu", SharedGPUQueueName, "", "independent"},
		"azure.experimental.training.4x": {"4xgpu", SharedGPUQueueName, "", "independent"},
		"azure.research.large-memory.2x": {"2xh200-141gb", SharedGPUQueueName, "nd-h200-v5", "single-node-nvlink"},
		"azure.research.large-memory.4x": {"4xh200-141gb", SharedGPUQueueName, "nd-h200-v5", "single-node-nvlink"},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ResolvePreset("", name)
			if err != nil {
				t.Fatalf("ResolvePreset(%q): %v", name, err)
			}
			if got.SourceFile != embeddedPolicySource {
				t.Errorf("source=%q want %q", got.SourceFile, embeddedPolicySource)
			}
			if got.Options.Shape != want.shape {
				t.Errorf("shape=%q want %q", got.Options.Shape, want.shape)
			}
			if got.Options.QueueName != want.queue {
				t.Errorf("queue=%q want %q", got.Options.QueueName, want.queue)
			}
			if got.Preset.ResourceFlavor != want.flavor {
				t.Errorf("flavor=%q want %q", got.Preset.ResourceFlavor, want.flavor)
			}
			if got.Options.Placement != want.topology {
				t.Errorf("placement=%q want %q", got.Options.Placement, want.topology)
			}
		})
	}
}

func TestWithDRAQueuePreservesManagedSeriesContract(t *testing.T) {
	resolved, err := ResolvePreset(embeddedPolicySource, "azure.research.large-memory.2x")
	if err != nil {
		t.Fatal(err)
	}
	dra := WithDRAQueue(resolved)

	if dra.Options.QueueName != SharedDRAQueueName || dra.Preset.QueueName != SharedDRAQueueName {
		t.Fatalf("DRA queue = %q/%q, want %q", dra.Options.QueueName, dra.Preset.QueueName, SharedDRAQueueName)
	}
	if dra.Preset.ClusterQueue != sharedDRAClusterQueueName {
		t.Fatalf("DRA ClusterQueue = %q, want %q", dra.Preset.ClusterQueue, sharedDRAClusterQueueName)
	}
	if dra.Preset.ResourceFlavor != "nd-h200-v5-dra" {
		t.Fatalf("DRA ResourceFlavor = %q, want nd-h200-v5-dra", dra.Preset.ResourceFlavor)
	}
	if !dra.Options.DisableKueueTopologyAnnotations || dra.Preset.TopologyName != "" {
		t.Fatalf("DRA preset must disable TAS annotations: %+v", dra)
	}
	if got, ok := ManagedGPUSeriesForFlavor(dra.Preset.ResourceFlavor); !ok || got != "nd-h200-v5" {
		t.Fatalf("managed series = %q, %v; want nd-h200-v5, true", got, ok)
	}
}

func TestWithDRAQueuePreservesCustomPolicyContract(t *testing.T) {
	resolved := ResolvedPreset{
		Preset: Preset{
			QueueName:      "custom-dra-queue",
			ClusterQueue:   "custom-dra-cq",
			ResourceFlavor: "custom-dra-flavor",
			TopologyName:   "custom-topology",
		},
		Options: Options{
			QueueName: "custom-dra-queue",
		},
		Annotations: map[string]string{
			AnnotationTopologyQueue:               "custom-dra-queue",
			workloadmeta.AnnotationClusterQueue:   "custom-dra-cq",
			workloadmeta.AnnotationResourceFlavor: "custom-dra-flavor",
			workloadmeta.AnnotationKueueTopology:  "custom-topology",
		},
	}

	got := WithDRAQueue(resolved)
	if got.Preset.QueueName != "custom-dra-queue" ||
		got.Preset.ClusterQueue != "custom-dra-cq" ||
		got.Preset.ResourceFlavor != "custom-dra-flavor" ||
		got.Preset.TopologyName != "custom-topology" {
		t.Fatalf("custom preset was rewritten: %+v", got.Preset)
	}
	if got.Options.QueueName != "custom-dra-queue" || got.Options.DisableKueueTopologyAnnotations {
		t.Fatalf("custom options were rewritten: %+v", got.Options)
	}
	if got.Annotations[AnnotationTopologyQueue] != "custom-dra-queue" ||
		got.Annotations[workloadmeta.AnnotationClusterQueue] != "custom-dra-cq" ||
		got.Annotations[workloadmeta.AnnotationResourceFlavor] != "custom-dra-flavor" ||
		got.Annotations[workloadmeta.AnnotationKueueTopology] != "custom-topology" {
		t.Fatalf("custom annotations were rewritten: %+v", got.Annotations)
	}
}

func TestSuggestPresetEmbedded(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	cases := []struct {
		team string
		lane string
		gpus int
		want string
	}{
		{"research", "training", 1, "azure.research.training.l"},
		{"research", "training", 2, "azure.research.training.2x"},
		{"research", "training", 4, "azure.research.training.4x"},
		{"research", "training", 8, "azure.research.training.xl"},
		{"experimental", "training", 2, "azure.experimental.training.2x"},
		{"experimental", "training", 4, "azure.experimental.training.4x"},
		{"research", "large-memory", 8, "azure.research.large-memory.xl"},
	}
	for _, tc := range cases {
		got, err := SuggestPreset("", tc.team, tc.lane, tc.gpus, 1)
		if err != nil {
			t.Errorf("SuggestPreset(team=%s lane=%s gpus=%d): %v", tc.team, tc.lane, tc.gpus, err)
			continue
		}
		if got.Preset.Name != tc.want {
			t.Errorf("SuggestPreset(team=%s lane=%s gpus=%d) = %s, want %s", tc.team, tc.lane, tc.gpus, got.Preset.Name, tc.want)
		}
	}
}

// TestSuggestPresetMultiNodeMatchesWorkers confirms the workers parameter
// filters presets correctly: workers=1 only matches single-node presets,
// workers=2 only matches the multi-node 2node preset.
func TestSuggestPresetMultiNodeMatchesWorkers(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	// workers=1 + (research, large-memory, 8) → single-node xl preset.
	got, err := SuggestPreset("", "research", "large-memory", 8, 1)
	if err != nil {
		t.Fatalf("workers=1 lookup: %v", err)
	}
	if got.Preset.Name != "azure.research.large-memory.xl" {
		t.Errorf("workers=1: want azure.research.large-memory.xl, got %s", got.Preset.Name)
	}
	if got.Preset.Workers != 1 {
		t.Errorf("workers=1: resolved preset Workers=%d, want 1", got.Preset.Workers)
	}

	// workers=2 + same intent → 2node multi-node preset.
	got2, err := SuggestPreset("", "research", "large-memory", 8, 2)
	if err != nil {
		t.Fatalf("workers=2 lookup: %v", err)
	}
	if got2.Preset.Name != "azure.research.large-memory.2node" {
		t.Errorf("workers=2: want azure.research.large-memory.2node, got %s", got2.Preset.Name)
	}
	if got2.Preset.Workers != 2 {
		t.Errorf("workers=2: resolved preset Workers=%d, want 2", got2.Preset.Workers)
	}
	if got2.Preset.Placement != "multi-node-nccl" {
		t.Errorf("workers=2 preset placement=%q, want multi-node-nccl", got2.Preset.Placement)
	}

	// workers=3 + same intent → no match (we only ship 2node).
	_, err = SuggestPreset("", "research", "large-memory", 8, 3)
	if !errors.Is(err, errNoPresetMatch) {
		t.Errorf("workers=3: want errNoPresetMatch, got %v", err)
	}
}

// TestPolicyRejectsWorkersWithoutMultiNodePlacement confirms a policy that
// declares workers > 1 with non-multi-node placement is rejected at load
// time. This catches the "researcher copies an xl preset and adds workers: 2
// without changing placement" mistake.
func TestPolicyRejectsWorkersWithoutMultiNodePlacement(t *testing.T) {
	policyPath := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test }
spec:
  presets:
    azure.test.large-memory.bad:
      profile: azure-gpu-8x
      team: test
      lane: large-memory
      mode: fixed
      placement: single-node-nvlink
      shape: 8xh200-141gb
      workers: 2
      gpuClass: h200-141gb
      queue: test-large-memory
      clusterQueue: team-test-reserved-cq
      namespace: ray
      resourceFlavor: gpu-h200-nvlink-141gb-dra
`)
	_, err := LoadPolicy(policyPath)
	if err == nil {
		t.Fatal("expected LoadPolicy to reject workers > 1 with single-node-nvlink placement")
	}
	if !strings.Contains(err.Error(), "multi-node-nccl") {
		t.Errorf("error should name multi-node-nccl placement; got %v", err)
	}
}

// TestSuggestPresetReturnsErrNoPresetMatch confirms callers can errors.Is the
// sentinel and fall through to the no-preset path. Sample training has no
// 3-GPU preset (the catalog jumps 1 → 2 → 4 → 8).
func TestSuggestPresetReturnsErrNoPresetMatch(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	_, err := SuggestPreset("", "research", "training", 3, 1)
	if err == nil {
		t.Fatal("expected error for 3-GPU research intent, got nil")
	}
	if !errors.Is(err, errNoPresetMatch) {
		t.Fatalf("expected errors.Is(err, errNoPresetMatch); err=%v", err)
	}
	if !strings.Contains(err.Error(), "team=research") || !strings.Contains(err.Error(), "gpus=3") {
		t.Errorf("error should name the failed intent; got %v", err)
	}
}

// TestSuggestPresetUnknownTeamErrors confirms an unknown team is treated as
// a clean miss (sentinel error) rather than a panic or a silent fallback to
// some other team's preset.
func TestSuggestPresetUnknownTeamErrors(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	_, err := SuggestPreset("", "fakeshop", "training", 2, 1)
	if !errors.Is(err, errNoPresetMatch) {
		t.Fatalf("expected errNoPresetMatch for unknown team; got %v", err)
	}
}

// TestSuggestPresetRejectsBadInputs confirms input validation fires before
// policy lookup so callers see clear errors.
func TestSuggestPresetRejectsBadInputs(t *testing.T) {
	t.Setenv(defaultPolicyEnv, "")
	t.Chdir(t.TempDir())

	cases := []struct {
		team string
		lane string
		gpus int
	}{
		{"", "training", 1},
		{"research", "", 1},
		{"research", "training", 0},
		{"research", "training", -1},
	}
	for _, tc := range cases {
		_, err := SuggestPreset("", tc.team, tc.lane, tc.gpus, 1)
		if err == nil {
			t.Errorf("SuggestPreset(team=%q lane=%q gpus=%d) returned nil, want validation error", tc.team, tc.lane, tc.gpus)
			continue
		}
		if errors.Is(err, errNoPresetMatch) {
			t.Errorf("bad-input error should NOT be errNoPresetMatch (callers should not fall through silently); got %v", err)
		}
	}
}

// TestSuggestPresetPrefersNonReclaimable confirms that when both a
// non-reclaimable and a reclaimable preset match the intent, the
// non-reclaimable one wins. Constructed via a custom policy because the
// embedded catalog deliberately keeps team/lane partitions disjoint.
func TestSuggestPresetPrefersNonReclaimable(t *testing.T) {
	policyPath := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test }
spec:
  presets:
    azure.test.training.2x:
      profile: azure-gpu-full
      team: test
      lane: training
      mode: fixed
      placement: independent
      shape: 2xa100-80gb
      gpuClass: a100-80gb
      queue: test-training
      clusterQueue: team-test-reserved-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb-dra
      reclaimable: false
    azure.test.training.2x-reclaim:
      profile: azure-gpu-full
      team: test
      lane: training
      mode: fixed
      placement: independent
      shape: 2xa100-80gb
      gpuClass: a100-80gb
      queue: test-training-reclaim
      clusterQueue: team-test-reclaim-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb-dra
      reclaimable: true
`)
	got, err := SuggestPreset(policyPath, "test", "training", 2, 1)
	if err != nil {
		t.Fatalf("SuggestPreset: %v", err)
	}
	if got.Preset.Name != "azure.test.training.2x" {
		t.Errorf("expected non-reclaimable winner azure.test.training.2x; got %s", got.Preset.Name)
	}
}

// TestSuggestPresetSkipsDisabled confirms a disabled preset is invisible to
// inference even when team/lane/gpus all match. The remaining (enabled)
// preset wins.
func TestSuggestPresetSkipsDisabled(t *testing.T) {
	policyPath := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test }
spec:
  presets:
    azure.test.training.2x:
      profile: azure-gpu-full
      team: test
      lane: training
      mode: fixed
      placement: independent
      shape: 2xa100-80gb
      gpuClass: a100-80gb
      queue: test-training
      clusterQueue: team-test-reserved-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb-dra
      disabled: true
      disabledReason: "decommissioned"
`)
	_, err := SuggestPreset(policyPath, "test", "training", 2, 1)
	if !errors.Is(err, errNoPresetMatch) {
		t.Fatalf("disabled preset should be invisible to suggester; err=%v", err)
	}
}

// TestSuggestPresetAmbiguityReportsCandidates confirms that when two
// non-reclaimable presets match and there's no tiebreaker, the user gets a
// clear error listing both names so they can pass --preset explicitly.
func TestSuggestPresetAmbiguityReportsCandidates(t *testing.T) {
	policyPath := writePolicy(t, `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test }
spec:
  presets:
    azure.test.training.2x-a:
      profile: azure-gpu-full
      team: test
      lane: training
      mode: fixed
      placement: independent
      shape: 2xa100-80gb
      gpuClass: a100-80gb
      queue: test-training
      clusterQueue: team-test-reserved-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb-dra
    azure.test.training.2x-b:
      profile: azure-gpu-full
      team: test
      lane: training
      mode: fixed
      placement: independent
      shape: 2xa100-80gb
      gpuClass: a100-80gb
      queue: test-training
      clusterQueue: team-test-reserved-cq
      namespace: ray
      resourceFlavor: gpu-a100-nvlink-80gb-dra
`)
	_, err := SuggestPreset(policyPath, "test", "training", 2, 1)
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if errors.Is(err, errNoPresetMatch) {
		t.Errorf("ambiguity should NOT be errNoPresetMatch (caller must not silently fall through); err=%v", err)
	}
	for _, want := range []string{"azure.test.training.2x-a", "azure.test.training.2x-b", "pass --preset explicitly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error should mention %q; got %v", want, err)
		}
	}
}

func TestGPUCountFromShape(t *testing.T) {
	cases := []struct {
		shape   string
		want    int
		wantOk  bool
		wantErr bool
	}{
		{"1xa100-80gb", 1, true, false},
		{"2xa100-80gb", 2, true, false},
		{"4xa100-80gb", 4, true, false},
		{"8xh200-141gb", 8, true, false},
		{"a100-80gb", 0, false, false},
		{"", 0, false, false},
		{"abcxa100", 0, false, true},
		{"-2xa100", 0, false, true},
	}
	for _, tc := range cases {
		got, ok, err := GPUCountFromShape(tc.shape)
		if (err != nil) != tc.wantErr {
			t.Errorf("GPUCountFromShape(%q): err=%v wantErr=%v", tc.shape, err, tc.wantErr)
			continue
		}
		if ok != tc.wantOk {
			t.Errorf("GPUCountFromShape(%q): ok=%v want=%v", tc.shape, ok, tc.wantOk)
		}
		if got != tc.want {
			t.Errorf("GPUCountFromShape(%q): got=%d want=%d", tc.shape, got, tc.want)
		}
	}
}
