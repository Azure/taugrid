// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	dcgmExporterLabel        = "app.kubernetes.io/name=dcgm-exporter"
	tauGPUMonitoringLabel    = "app.kubernetes.io/name=gpu-monitoring"
	dcgmExporterPort         = 9400
	tauGPUMonitoringDCGMPort = 19400
	dcgmGPUUtilMetric        = "DCGM_FI_DEV_GPU_UTIL"
	dcgmFBUsedMetric         = "DCGM_FI_DEV_FB_USED"
)

type GPURuntimeState string

const (
	GPURuntimeNotRequested GPURuntimeState = ""
	GPURuntimeObserved     GPURuntimeState = "observed"
	GPURuntimeUnavailable  GPURuntimeState = "unavailable"
)

// GPURuntimeEvidence is a current, run-scoped DCGM snapshot. It intentionally
// does not retain samples after a pod releases its GPU allocation.
type GPURuntimeEvidence struct {
	State         GPURuntimeState     `json:"state"`
	Source        string              `json:"source,omitempty"`
	Reason        string              `json:"reason,omitempty"`
	NodesExpected int                 `json:"nodesExpected"`
	NodesScraped  int                 `json:"nodesScraped"`
	Devices       []GPUDeviceEvidence `json:"devices"`
}

// GPUDeviceEvidence joins one DCGM device sample to its Kubernetes owner.
// Observation booleans distinguish a real zero from a missing metric.
type GPUDeviceEvidence struct {
	Pod                     string  `json:"pod"`
	Container               string  `json:"container"`
	GPU                     string  `json:"gpu"`
	UUID                    string  `json:"uuid,omitempty"`
	UtilizationPercent      float64 `json:"utilizationPercent"`
	UtilizationObserved     bool    `json:"utilizationObserved"`
	FramebufferUsedMiB      float64 `json:"framebufferUsedMiB"`
	FramebufferUsedObserved bool    `json:"framebufferUsedObserved"`
}

type dcgmExporterPodList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Ports []struct {
					Name          string `json:"name"`
					ContainerPort int    `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

type dcgmExporterPod struct {
	Name      string
	Namespace string
	Node      string
	Port      int
}

type dcgmSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// FetchGPURuntime fetches a bounded live snapshot from dcgm-exporter through
// Kubernetes pod proxy. It never execs into workload or system containers.
func FetchGPURuntime(ctx context.Context, r rawRunner, s Snapshot) GPURuntimeEvidence {
	evidence := GPURuntimeEvidence{Source: "dcgm-exporter"}
	if runFinished(s) {
		evidence.State = GPURuntimeUnavailable
		evidence.Reason = "run completed; live GPU ownership samples are no longer retained"
		return evidence
	}

	podsByName, nodes := runtimePodScope(s)
	evidence.NodesExpected = len(nodes)
	if len(podsByName) == 0 {
		evidence.State = GPURuntimeUnavailable
		if len(s.Pods) == 0 {
			evidence.Reason = "run has no pods to match against GPU telemetry"
		} else {
			evidence.Reason = "run has no scheduled pods to match against GPU telemetry"
		}
		return evidence
	}

	exporters, discoveryAvailable := discoverDCGMTelemetryPods(ctx, r, nodes)
	if !discoveryAvailable {
		evidence.State = GPURuntimeUnavailable
		evidence.Reason = "cannot discover DCGM telemetry pods"
		return evidence
	}
	if len(exporters) == 0 {
		evidence.State = GPURuntimeUnavailable
		evidence.Reason = "no running DCGM telemetry pod found on the run's nodes"
		return evidence
	}

	devices := map[string]*GPUDeviceEvidence{}
	successfulScrapes := 0
	sawGPUMetric := false
	sawOwnershipLabels := false
	for _, exporter := range exporters {
		path := fmt.Sprintf(
			"/api/v1/namespaces/%s/pods/%s:%d/proxy/metrics",
			exporter.Namespace,
			exporter.Name,
			exporter.Port,
		)
		raw, scrapeErr := r.Raw(ctx, []string{"get", "--raw", path}, nil)
		if scrapeErr != nil {
			continue
		}
		successfulScrapes++
		evidence.NodesScraped++
		for _, sample := range parseDCGMSamples(raw) {
			sawGPUMetric = true
			if sample.Labels["namespace"] != "" && sample.Labels["pod"] != "" {
				sawOwnershipLabels = true
			}
			if sample.Labels["namespace"] != s.Namespace || !podsByName[sample.Labels["pod"]] {
				continue
			}
			key := strings.Join([]string{
				sample.Labels["pod"],
				sample.Labels["container"],
				sample.Labels["UUID"],
				sample.Labels["gpu"],
			}, "\x00")
			device := devices[key]
			if device == nil {
				device = &GPUDeviceEvidence{
					Pod:       sample.Labels["pod"],
					Container: sample.Labels["container"],
					GPU:       sample.Labels["gpu"],
					UUID:      sample.Labels["UUID"],
				}
				devices[key] = device
			}
			switch sample.Name {
			case dcgmGPUUtilMetric:
				device.UtilizationPercent = sample.Value
				device.UtilizationObserved = true
			case dcgmFBUsedMetric:
				device.FramebufferUsedMiB = sample.Value
				device.FramebufferUsedObserved = true
			}
		}
	}

	if len(devices) == 0 {
		evidence.State = GPURuntimeUnavailable
		if successfulScrapes == 0 {
			evidence.Reason = "cannot proxy metrics from dcgm-exporter pods"
		} else if sawGPUMetric && !sawOwnershipLabels {
			evidence.Reason = "DCGM metrics do not include Kubernetes pod ownership labels"
		} else {
			evidence.Reason = "dcgm-exporter returned no samples labeled for this run's pods"
		}
		return evidence
	}

	evidence.State = GPURuntimeObserved
	evidence.Devices = make([]GPUDeviceEvidence, 0, len(devices))
	for _, device := range devices {
		evidence.Devices = append(evidence.Devices, *device)
	}
	sort.Slice(evidence.Devices, func(i, j int) bool {
		left := evidence.Devices[i]
		right := evidence.Devices[j]
		if left.Pod != right.Pod {
			return left.Pod < right.Pod
		}
		if left.Container != right.Container {
			return left.Container < right.Container
		}
		if left.GPU != right.GPU {
			return left.GPU < right.GPU
		}
		return left.UUID < right.UUID
	})
	return evidence
}

func runtimePodScope(s Snapshot) (map[string]bool, map[string]bool) {
	pods := make(map[string]bool)
	nodes := make(map[string]bool)
	for _, pod := range s.Pods {
		if pod.Name == "" || pod.Node == "" {
			continue
		}
		if strings.EqualFold(pod.Phase, "Succeeded") || strings.EqualFold(pod.Phase, "Failed") {
			continue
		}
		pods[pod.Name] = true
		nodes[pod.Node] = true
	}
	return pods, nodes
}

func discoverDCGMTelemetryPods(ctx context.Context, r rawRunner, nodes map[string]bool) ([]dcgmExporterPod, bool) {
	sources := []struct {
		selector string
		port     int
	}{
		{selector: dcgmExporterLabel, port: dcgmExporterPort},
		{selector: tauGPUMonitoringLabel, port: tauGPUMonitoringDCGMPort},
	}
	byNode := make(map[string]dcgmExporterPod)
	discoveryAvailable := false
	for _, source := range sources {
		data, err := r.Raw(ctx, []string{
			"get", "pods", "-A", "-l", source.selector, "-o", "json",
		}, nil)
		if err != nil {
			continue
		}
		pods, err := exporterPodsForNodes([]byte(data), nodes, source.port)
		if err != nil {
			continue
		}
		discoveryAvailable = true
		for _, pod := range pods {
			if _, exists := byNode[pod.Node]; !exists {
				byNode[pod.Node] = pod
			}
		}
		if len(byNode) == len(nodes) {
			break
		}
	}
	out := make([]dcgmExporterPod, 0, len(byNode))
	for _, pod := range byNode {
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Name < out[j].Name
	})
	return out, discoveryAvailable
}

func exporterPodsForNodes(data []byte, nodes map[string]bool, defaultPort int) ([]dcgmExporterPod, error) {
	var list dcgmExporterPodList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	byNode := make(map[string]dcgmExporterPod)
	for _, pod := range list.Items {
		if pod.Status.Phase != "Running" || !nodes[pod.Spec.NodeName] {
			continue
		}
		port := defaultPort
		for _, container := range pod.Spec.Containers {
			for _, candidate := range container.Ports {
				if candidate.Name == "metrics" && candidate.ContainerPort > 0 {
					port = candidate.ContainerPort
				}
			}
		}
		if _, exists := byNode[pod.Spec.NodeName]; !exists {
			byNode[pod.Spec.NodeName] = dcgmExporterPod{
				Name:      pod.Metadata.Name,
				Namespace: pod.Metadata.Namespace,
				Node:      pod.Spec.NodeName,
				Port:      port,
			}
		}
	}
	out := make([]dcgmExporterPod, 0, len(byNode))
	for _, pod := range byNode {
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func parseDCGMSamples(raw string) []dcgmSample {
	var samples []dcgmSample
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, dcgmGPUUtilMetric+"{") &&
			!strings.HasPrefix(line, dcgmFBUsedMetric+"{") {
			continue
		}
		sample, ok := parseDCGMSample(line)
		if ok {
			samples = append(samples, sample)
		}
	}
	return samples
}

func parseDCGMSample(line string) (dcgmSample, bool) {
	open := strings.IndexByte(line, '{')
	close := strings.LastIndexByte(line, '}')
	if open <= 0 || close <= open {
		return dcgmSample{}, false
	}
	valueFields := strings.Fields(strings.TrimSpace(line[close+1:]))
	if len(valueFields) == 0 {
		return dcgmSample{}, false
	}
	value, err := strconv.ParseFloat(valueFields[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return dcgmSample{}, false
	}
	return dcgmSample{
		Name:   line[:open],
		Labels: parsePrometheusLabels(line[open+1 : close]),
		Value:  value,
	}, true
}

func parsePrometheusLabels(raw string) map[string]string {
	labels := make(map[string]string)
	for len(raw) > 0 {
		raw = strings.TrimLeft(raw, " \t,")
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			break
		}
		key := strings.TrimSpace(raw[:eq])
		raw = strings.TrimSpace(raw[eq+1:])
		if len(raw) == 0 || raw[0] != '"' {
			break
		}
		raw = raw[1:]
		var value strings.Builder
		escaped := false
		closed := false
		for i := 0; i < len(raw); i++ {
			ch := raw[i]
			if escaped {
				switch ch {
				case 'n':
					value.WriteByte('\n')
				default:
					value.WriteByte(ch)
				}
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				raw = raw[i+1:]
				closed = true
				break
			}
			value.WriteByte(ch)
		}
		if !closed {
			break
		}
		labels[key] = value.String()
	}
	return labels
}

func runFinished(s Snapshot) bool {
	if !s.JobFinishedAt.IsZero() {
		return true
	}
	for _, condition := range s.JobConditions {
		if condition.Status == "True" && (condition.Type == "Complete" || condition.Type == "Failed") {
			return true
		}
	}
	if s.JobFound {
		return false
	}
	if s.RayJobFound || s.RayJob.Found {
		rayJob := snapshotRayJob(s)
		return rayJobStatusSucceeded(rayJob) ||
			rayJobStatusFailed(rayJob) ||
			!rayJob.FinishedAt.IsZero()
	}
	return false
}
