package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFetchGPURuntime_ActiveRun(t *testing.T) {
	var calls []string
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "get pods -A -l app.kubernetes.io/name=dcgm-exporter -o json":
			return `{"items":[
				{"metadata":{"name":"dcgm-node-a","namespace":"gpu-system"},"spec":{"nodeName":"node-a","containers":[{"ports":[{"name":"metrics","containerPort":9400}]}]},"status":{"phase":"Running"}},
				{"metadata":{"name":"dcgm-node-a-old","namespace":"gpu-system"},"spec":{"nodeName":"node-a"},"status":{"phase":"Succeeded"}},
				{"metadata":{"name":"dcgm-node-b","namespace":"gpu-system"},"spec":{"nodeName":"node-b"},"status":{"phase":"Running"}}
			]}`, nil
		case "get --raw /api/v1/namespaces/gpu-system/pods/dcgm-node-a:9400/proxy/metrics":
			return `
# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a",container="trainer",namespace="ray",pod="train-a"} 63
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-a",container="trainer",namespace="ray",pod="train-a"} 9216
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-b",container="trainer",namespace="ray",pod="train-b"} 100
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-b",container="trainer",namespace="ray",pod="train-b"} 10240
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-other",container="trainer",namespace="ray",pod="other-run"} 99
`, nil
		default:
			t.Fatalf("unexpected kubectl call: %s", call)
			return "", nil
		}
	})
	snap := Snapshot{
		Name:      "train",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 2,
		Pods: []Pod{
			{Name: "train-a", Node: "node-a", Phase: "Running"},
			{Name: "train-b", Node: "node-a", Phase: "Running"},
		},
		ResourceClaims: []ResourceClaim{{Name: "train-gpu", Allocated: true, Allocation: "pool-a/gpu-0,pool-a/gpu-1"}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeObserved {
		t.Fatalf("state = %q, reason = %q", evidence.State, evidence.Reason)
	}
	if len(evidence.Devices) != 2 {
		t.Fatalf("devices = %+v, want 2 matching devices", evidence.Devices)
	}
	if got := gpuTelemetrySummary(evidence); got != "dcgm-exporter live snapshot (1/1 workload nodes)" {
		t.Fatalf("telemetry summary = %q", got)
	}
	if got := gpuUtilizationSummary(evidence); got != "81.5% avg, 100.0% max across 2 GPU(s)" {
		t.Fatalf("utilization summary = %q", got)
	}
	if got := gpuMemorySummary(evidence); got != "9.50 GiB avg, 10.00 GiB max across 2 GPU(s)" {
		t.Fatalf("memory summary = %q", got)
	}
	if got := gpuActivitySummary(evidence); got != "active (DCGM utilization > 0% on 2/2 GPU(s); device-level evidence only)" {
		t.Fatalf("activity summary = %q", got)
	}
	if got := gpuDevicesSummary(evidence); got != "train-a/trainer gpu=0 uuid=GPU-a, train-b/trainer gpu=1 uuid=GPU-b" {
		t.Fatalf("devices summary = %q", got)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want one discovery and one scrape for the shared node", calls)
	}
}

func TestFetchGPURuntime_AllocatedButIdle(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "get pods -A -l app.kubernetes.io/name=dcgm-exporter -o json":
			return `{"items":[]}`, nil
		case "get pods -A -l app.kubernetes.io/name=gpu-monitoring -o json":
			return `{"items":[{"metadata":{"name":"gpu-monitoring-h200-a","namespace":"gpu-monitoring"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}]}`, nil
		case "get --raw /api/v1/namespaces/gpu-monitoring/pods/gpu-monitoring-h200-a:19400/proxy/metrics":
			return `
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-idle",container="trainer",namespace="ray",pod="idle-run"} 0
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-idle",container="trainer",namespace="ray",pod="idle-run"} 4096
`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})
	snap := Snapshot{
		Name:      "idle-run",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
		Pods:      []Pod{{Name: "idle-run", Node: "node-a", Phase: "Running"}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeObserved {
		t.Fatalf("state = %q, reason = %q", evidence.State, evidence.Reason)
	}
	if got := gpuUtilizationSummary(evidence); got != "0.0% avg, 0.0% max across 1 GPU(s)" {
		t.Fatalf("utilization summary = %q", got)
	}
	if got := gpuActivitySummary(evidence); got != "idle now (DCGM utilization observed at 0% on 1 GPU(s); 4.00 GiB framebuffer used across 1 GPU(s))" {
		t.Fatalf("activity summary = %q", got)
	}
}

func TestFetchGPURuntime_CompletedRunDoesNotReportFalseZero(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		t.Fatalf("completed runs must not query live exporter telemetry: %v", args)
		return "", nil
	})
	snap := Snapshot{
		Name:          "complete-run",
		Namespace:     "ray",
		JobFound:      true,
		JobSucceeded:  1,
		JobFinishedAt: time.Now(),
		Pods:          []Pod{{Name: "complete-run", Node: "node-a", Phase: "Succeeded"}},
		ResourceClaims: []ResourceClaim{{
			Name:       "complete-run-gpu",
			Allocated:  true,
			Allocation: "pool-a/gpu-0",
		}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeUnavailable {
		t.Fatalf("state = %q, want unavailable", evidence.State)
	}
	if !strings.Contains(gpuUtilizationSummary(evidence), "run completed") {
		t.Fatalf("completed summary must explain live-data expiry, got %q", gpuUtilizationSummary(evidence))
	}
	if got := gpuAllocationSummary(snap); got != "complete-run-gpu=pool-a/gpu-0" {
		t.Fatalf("allocation summary = %q", got)
	}
}

func TestFetchGPURuntime_TelemetryUnavailable(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		got := strings.Join(args, " ")
		if got != "get pods -A -l app.kubernetes.io/name=dcgm-exporter -o json" &&
			got != "get pods -A -l app.kubernetes.io/name=gpu-monitoring -o json" {
			t.Fatalf("unexpected kubectl call: %s", got)
		}
		return "", errors.New("forbidden")
	})
	snap := Snapshot{
		Name:      "train",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
		Pods:      []Pod{{Name: "train", Node: "node-a", Phase: "Running"}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeUnavailable {
		t.Fatalf("state = %q, want unavailable", evidence.State)
	}
	if got := gpuUtilizationSummary(evidence); got != "not available (cannot discover DCGM telemetry pods)" {
		t.Fatalf("unavailable summary = %q", got)
	}
}

func TestGPURuntime_PartialCoverageIsVisible(t *testing.T) {
	evidence := GPURuntimeEvidence{
		State:         GPURuntimeObserved,
		Source:        "dcgm-exporter",
		NodesExpected: 2,
		NodesScraped:  1,
		Devices: []GPUDeviceEvidence{{
			Pod:                     "train-a",
			UUID:                    "GPU-a",
			UtilizationPercent:      50,
			UtilizationObserved:     true,
			FramebufferUsedMiB:      2048,
			FramebufferUsedObserved: true,
		}},
	}
	if got := gpuTelemetrySummary(evidence); got != "dcgm-exporter live snapshot (1/2 workload nodes); partial coverage" {
		t.Fatalf("partial coverage summary = %q", got)
	}
	if got := gpuUtilizationSummary(evidence); !strings.HasSuffix(got, "(partial coverage)") {
		t.Fatalf("utilization must show partial coverage, got %q", got)
	}
	if got := gpuMemorySummary(evidence); !strings.HasSuffix(got, "(partial coverage)") {
		t.Fatalf("memory must show partial coverage, got %q", got)
	}
}

func TestRunFinished_DoesNotTreatRetryCountsAsTerminal(t *testing.T) {
	snap := Snapshot{
		JobFound:     true,
		JobActive:    1,
		JobSucceeded: 1,
		JobFailed:    1,
	}
	if runFinished(snap) {
		t.Fatal("non-terminal retry counts must not suppress live GPU telemetry")
	}
	snap.JobConditions = []Condition{{Type: "Complete", Status: "True"}}
	if !runFinished(snap) {
		t.Fatal("terminal Job condition must stop live GPU telemetry")
	}
}

func TestRuntimePodScope_SkipsTerminalRetryPods(t *testing.T) {
	pods, nodes := runtimePodScope(Snapshot{Pods: []Pod{
		{Name: "active", Node: "node-a", Phase: "Running"},
		{Name: "failed-retry", Node: "node-b", Phase: "Failed"},
		{Name: "succeeded-retry", Node: "node-c", Phase: "Succeeded"},
	}})
	if len(pods) != 1 || !pods["active"] {
		t.Fatalf("pod scope = %v, want only active pod", pods)
	}
	if len(nodes) != 1 || !nodes["node-a"] {
		t.Fatalf("node scope = %v, want only active node", nodes)
	}
}

func TestFetchGPURuntime_MissingPodLabelsIsUnavailable(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "get pods -A -l app.kubernetes.io/name=dcgm-exporter -o json":
			return `{"items":[{"metadata":{"name":"dcgm-node-a","namespace":"gpu-system"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}]}`, nil
		case "get --raw /api/v1/namespaces/gpu-system/pods/dcgm-node-a:9400/proxy/metrics":
			return `DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a"} 75`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})
	snap := Snapshot{
		Name:      "train",
		Namespace: "ray",
		JobFound:  true,
		Pods:      []Pod{{Name: "train", Node: "node-a", Phase: "Running"}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeUnavailable || evidence.Reason != "DCGM metrics do not include Kubernetes pod ownership labels" {
		t.Fatalf("expected missing pod labels to be unavailable, got %+v", evidence)
	}
}

func TestFetchGPURuntime_ProxyForbiddenIsUnavailable(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "get pods -A -l app.kubernetes.io/name=dcgm-exporter -o json":
			return `{"items":[{"metadata":{"name":"dcgm-node-a","namespace":"gpu-system"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}]}`, nil
		case "get --raw /api/v1/namespaces/gpu-system/pods/dcgm-node-a:9400/proxy/metrics":
			return "", errors.New("forbidden: pods/proxy")
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})
	snap := Snapshot{
		Name:      "train",
		Namespace: "ray",
		JobFound:  true,
		Pods:      []Pod{{Name: "train", Node: "node-a", Phase: "Running"}},
	}

	evidence := FetchGPURuntime(context.Background(), runner, snap)
	if evidence.State != GPURuntimeUnavailable || evidence.Reason != "cannot proxy metrics from dcgm-exporter pods" {
		t.Fatalf("expected proxy failure to be unavailable, got %+v", evidence)
	}
}

func TestRenderRunProfile_ShowsCompleteGPURuntimeEvidence(t *testing.T) {
	snap := Snapshot{
		Name:      "train",
		Namespace: "ray",
		JobFound:  true,
		ResourceClaims: []ResourceClaim{{
			Name:       "train-gpu",
			Allocated:  true,
			Allocation: "pool-a/gpu-0",
		}},
		GPURuntime: GPURuntimeEvidence{
			State:         GPURuntimeObserved,
			Source:        "dcgm-exporter",
			NodesExpected: 1,
			NodesScraped:  1,
			Devices: []GPUDeviceEvidence{{
				Pod:                     "train-a",
				Container:               "trainer",
				GPU:                     "0",
				UUID:                    "GPU-a",
				UtilizationPercent:      63,
				UtilizationObserved:     true,
				FramebufferUsedMiB:      9216,
				FramebufferUsedObserved: true,
			}},
		},
	}

	out := RenderRunProfile(snap, CostProfile{})
	for _, want := range []string{
		"gpu_allocation",
		"train-gpu=pool-a/gpu-0",
		"gpu_telemetry",
		"dcgm-exporter live snapshot (1/1 workload nodes)",
		"gpu_devices",
		"train-a/trainer gpu=0 uuid=GPU-a",
		"gpu_utilization",
		"63.0% avg, 63.0% max",
		"gpu_memory",
		"9.00 GiB avg, 9.00 GiB max",
		"gpu_activity",
		"active (DCGM utilization > 0%",
		"cuda_compute_process",
		"not available (dcgm-exporter exposes device activity",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("run profile missing %q:\n%s", want, out)
		}
	}
}

func TestParseDCGMSamples_PreservesEscapedLabels(t *testing.T) {
	samples := parseDCGMSamples(`
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a",container="train\"er",namespace="ray",pod="run-a"} 42
ignored_metric{pod="run-a"} 1
`)
	if len(samples) != 1 {
		t.Fatalf("samples = %+v, want one", samples)
	}
	if got := samples[0].Labels["container"]; got != `train"er` {
		t.Fatalf("container label = %q", got)
	}
	if samples[0].Value != 42 {
		t.Fatalf("value = %v", samples[0].Value)
	}
}
