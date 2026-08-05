package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"
	runtopology "github.com/Azure/taugrid/core/topology"
)

func newClusterValidateTopologyCmd() *cobra.Command {
	var (
		kubeContext  string
		preset       string
		clusterQueue string
	)
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Verify cluster topology matches Kueue ResourceFlavor expectations",
		Long: `Verify that the live cluster topology matches Kueue ResourceFlavor expectations.

For each GPU ResourceFlavor referenced by the ClusterQueue, this command checks:
  - nodes matching the flavor's nodeLabels exist
  - matched GPU nodes report nvidia.com/gpu allocatable > 0
  - GPU nodes have topology.kubernetes.io/zone populated
  - IB-capable SKUs (H200, A100) have Ready nodes

Use --preset to validate a single preset's full chain (Kueue objects + node match).
Without --preset, validates all ResourceFlavors in the ClusterQueue.

Examples:
  tau cluster validate topology
  tau cluster validate topology --preset azure.research.training.l`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := kube.New(kubeContext)
			cq := clusterQueue
			if cq == "" {
				cq = runtopology.SharedGPUClusterQueueName
			}

			if preset != "" {
				if clusterQueue != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "note: --cluster-queue is ignored when --preset is set\n")
				}
				return validatePresetTopology(cmd.Context(), cmd.OutOrStdout(), r, preset, r.Context)
			}
			return validateClusterTopology(cmd.Context(), cmd.OutOrStdout(), r, r.Context, cq)
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&preset, "preset", "", "validate a single preset's full chain (Kueue objects + node match)")
	cmd.Flags().StringVar(&clusterQueue, "cluster-queue", "", "ClusterQueue name (default: "+runtopology.SharedGPUClusterQueueName+")")
	return cmd
}

type checkStatus string

const (
	checkOK    checkStatus = "ok"
	checkWarn  checkStatus = "warn"
	checkError checkStatus = "error"
)

type topologyCheckResult struct {
	label   string
	status  checkStatus
	message string
}

func writeResults(w io.Writer, results []topologyCheckResult, passed, warnings, errors *int) {
	for _, r := range results {
		fmt.Fprintf(w, "  %-5s %s: %s\n", r.status, r.label, r.message)
		switch r.status {
		case checkOK:
			*passed++
		case checkWarn:
			*warnings++
		case checkError:
			*errors++
		}
	}
}

func writeSummary(w io.Writer, passed, warnings, errors int) error {
	fmt.Fprintf(w, "\nsummary: %d passed, %d warnings, %d errors\n", passed, warnings, errors)
	if errors > 0 {
		return fmt.Errorf("topology validation failed: %d error(s)", errors)
	}
	return nil
}

func validateClusterTopology(ctx context.Context, w io.Writer, r validateNodesRunner, ctxName, cqName string) error {
	cq, err := fetchClusterQueue(ctx, r, cqName)
	if err != nil {
		return fmt.Errorf("fetch clusterqueue %s: %w", cqName, err)
	}

	fmt.Fprintf(w, "cluster: %s\n", dash(ctxName))
	fmt.Fprintf(w, "clusterqueue: %s\n\n", cqName)

	flavorNames := cq.flavorNames()
	if len(flavorNames) == 0 {
		fmt.Fprintf(w, "warning: clusterqueue %s has no resource flavors\n", cqName)
		return fmt.Errorf("clusterqueue %s has no resource flavors", cqName)
	}

	var passed, warnings, errors int
	for _, name := range flavorNames {
		results := validateFlavor(ctx, r, name)
		fmt.Fprintf(w, "resourceflavor %s:\n", name)
		writeResults(w, results, &passed, &warnings, &errors)
		fmt.Fprintln(w)
	}

	return writeSummary(w, passed, warnings, errors)
}

func validatePresetTopology(ctx context.Context, w io.Writer, r validateNodesRunner, presetName, ctxName string) error {
	resolved, err := runtopology.ResolvePreset("", presetName)
	if err != nil {
		return err
	}
	p := resolved.Preset

	fmt.Fprintf(w, "cluster: %s\n", dash(ctxName))
	fmt.Fprintf(w, "preset: %s\n", p.Name)
	fmt.Fprintf(w, "source: %s\n\n", resolved.SourceFile)

	var passed, warnings, errors int
	var results []topologyCheckResult

	objChecks := presetObjectChecks(p)
	for _, check := range objChecks {
		out, err := r.Raw(ctx, check.args, nil)
		if err != nil || strings.TrimSpace(out) == "" {
			detail := "not found"
			if err != nil {
				detail = err.Error()
			}
			results = append(results, topologyCheckResult{check.label, checkError, detail})
		} else {
			results = append(results, topologyCheckResult{check.label, checkOK, "exists"})
		}
	}

	if p.ResourceFlavor != "" {
		flavorResults := validateFlavor(ctx, r, p.ResourceFlavor)
		results = append(results, flavorResults...)
	}

	fmt.Fprintf(w, "preset %s:\n", p.Name)
	writeResults(w, results, &passed, &warnings, &errors)

	return writeSummary(w, passed, warnings, errors)
}

type objectCheck struct {
	label string
	args  []string
}

func presetObjectChecks(p runtopology.Preset) []objectCheck {
	ns := p.Namespace
	if ns == "" {
		ns = runtopology.DefaultLocalQueueNamespace
	}
	checks := []objectCheck{
		{"localqueue", []string{"-n", ns, "get", "localqueue.kueue.x-k8s.io", p.QueueName, "-o", "name"}},
	}
	if p.ClusterQueue != "" {
		checks = append(checks, objectCheck{"clusterqueue", []string{"get", "clusterqueue.kueue.x-k8s.io", p.ClusterQueue, "-o", "name"}})
	}
	if p.TopologyName != "" {
		checks = append(checks, objectCheck{"topology", []string{"get", "topology.kueue.x-k8s.io", p.TopologyName, "-o", "name"}})
	}
	if p.WorkloadPriorityClassName != "" {
		checks = append(checks, objectCheck{"workloadpriorityclass", []string{"get", "workloadpriorityclass.kueue.x-k8s.io", p.WorkloadPriorityClassName, "-o", "name"}})
	}
	if p.PodPriorityClassName != "" {
		checks = append(checks, objectCheck{"priorityclass", []string{"get", "priorityclass.scheduling.k8s.io", p.PodPriorityClassName, "-o", "name"}})
	}
	return checks
}

func validateFlavor(ctx context.Context, r validateNodesRunner, name string) []topologyCheckResult {
	rf, err := fetchResourceFlavor(ctx, r, name)
	if err != nil {
		return []topologyCheckResult{{label: "fetch", status: checkError, message: fmt.Sprintf("cannot fetch resourceflavor: %v", err)}}
	}

	if len(rf.Spec.NodeLabels) == 0 {
		return []topologyCheckResult{{label: "accounting-only", status: checkOK, message: "no nodeLabels (CPU/memory accounting flavor)"}}
	}

	nodes, err := fetchNodesByLabels(ctx, r, rf.Spec.NodeLabels)
	if err != nil {
		return []topologyCheckResult{{label: "node-match", status: checkError, message: fmt.Sprintf("cannot list nodes: %v", err)}}
	}

	var results []topologyCheckResult

	instanceType := rf.Spec.NodeLabels["node.kubernetes.io/instance-type"]
	selectorDesc := formatNodeSelector(rf.Spec.NodeLabels)
	if len(nodes) == 0 {
		results = append(results, topologyCheckResult{"node-match", checkError,
			fmt.Sprintf("0 nodes match %s — check instance-type label or node pool availability", selectorDesc)})
		return results
	}
	results = append(results, topologyCheckResult{"node-match", checkOK,
		fmt.Sprintf("%d node(s) match %s", len(nodes), selectorDesc)})

	gpuReady, zoneReady, readyCount, gpuTotal := 0, 0, 0, len(nodes)
	for _, n := range nodes {
		if gpuAllocatable(n) > 0 {
			gpuReady++
		}
		if n.Metadata.Labels["topology.kubernetes.io/zone"] != "" {
			zoneReady++
		}
		if nodeIsReady(n) {
			readyCount++
		}
	}

	if gpuReady == gpuTotal {
		results = append(results, topologyCheckResult{"gpu-allocatable", checkOK,
			fmt.Sprintf("%d/%d nodes report nvidia.com/gpu > 0", gpuReady, gpuTotal)})
	} else if gpuReady > 0 {
		results = append(results, topologyCheckResult{"gpu-allocatable", checkWarn,
			fmt.Sprintf("%d/%d nodes report nvidia.com/gpu > 0 — device plugin may be starting on remaining nodes", gpuReady, gpuTotal)})
	} else {
		results = append(results, topologyCheckResult{"gpu-allocatable", checkError,
			fmt.Sprintf("0/%d nodes report nvidia.com/gpu — device plugin not running or GPUs not detected", gpuTotal)})
	}

	if zoneReady == gpuTotal {
		results = append(results, topologyCheckResult{"topology-zone", checkOK,
			fmt.Sprintf("%d/%d nodes have topology.kubernetes.io/zone", zoneReady, gpuTotal)})
	} else {
		results = append(results, topologyCheckResult{"topology-zone", checkWarn,
			fmt.Sprintf("%d/%d nodes have topology.kubernetes.io/zone — Kueue Topology scheduling requires this label", zoneReady, gpuTotal)})
	}

	if sku, ok := ibCapableSKUs[strings.ToLower(instanceType)]; ok {
		if readyCount == gpuTotal {
			results = append(results, topologyCheckResult{"ib-capable", checkOK,
				fmt.Sprintf("%s SKU, %d/%d matched nodes are Ready", sku, readyCount, gpuTotal)})
		} else {
			results = append(results, topologyCheckResult{"ib-capable", checkWarn,
				fmt.Sprintf("%s SKU, %d/%d matched nodes are Ready — IB fabric requires all nodes healthy", sku, readyCount, gpuTotal)})
		}
	}

	return results
}

type topologyNodeDoc struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type topologyNodeListDoc struct {
	Items []topologyNodeDoc `json:"items"`
}

type topologyClusterQueueDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ResourceGroups []struct {
			Flavors []struct {
				Name string `json:"name"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
}

func (cq topologyClusterQueueDoc) flavorNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name != "" && !seen[f.Name] {
				seen[f.Name] = true
				names = append(names, f.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

type topologyResourceFlavorDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeLabels map[string]string `json:"nodeLabels"`
	} `json:"spec"`
}

func fetchJSON[T any](ctx context.Context, r validateNodesRunner, args []string) (T, error) {
	var zero T
	out, err := r.Raw(ctx, args, nil)
	if err != nil {
		return zero, err
	}
	var doc T
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return zero, fmt.Errorf("parse json: %w", err)
	}
	return doc, nil
}

func fetchClusterQueue(ctx context.Context, r validateNodesRunner, name string) (topologyClusterQueueDoc, error) {
	return fetchJSON[topologyClusterQueueDoc](ctx, r, []string{"get", "clusterqueue.kueue.x-k8s.io", name, "-o", "json"})
}

func fetchResourceFlavor(ctx context.Context, r validateNodesRunner, name string) (topologyResourceFlavorDoc, error) {
	return fetchJSON[topologyResourceFlavorDoc](ctx, r, []string{"get", "resourceflavor.kueue.x-k8s.io", name, "-o", "json"})
}

func fetchNodesByLabels(ctx context.Context, r validateNodesRunner, labels map[string]string) ([]topologyNodeDoc, error) {
	selector := formatNodeSelector(labels)
	doc, err := fetchJSON[topologyNodeListDoc](ctx, r, []string{"get", "nodes", "-l", selector, "-o", "json"})
	if err != nil {
		return nil, err
	}
	return doc.Items, nil
}

func gpuAllocatable(n topologyNodeDoc) int {
	count, err := strconv.Atoi(n.Status.Allocatable["nvidia.com/gpu"])
	if err != nil {
		return 0
	}
	return count
}

func nodeIsReady(n topologyNodeDoc) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return strings.EqualFold(c.Status, "True")
		}
	}
	return false
}

var ibCapableSKUs = map[string]string{
	"standard_nd96isr_h200_v5":  "H200",
	"standard_nd96isr_h100_v5":  "H100",
	"standard_nd96amsr_a100_v4": "A100",
	"standard_nd96isr_gb200_v6": "GB200",
}
