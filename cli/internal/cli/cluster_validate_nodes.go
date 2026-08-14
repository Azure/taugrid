// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	klabels "k8s.io/apimachinery/pkg/labels"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	validatePodPrefix = "tau-validate-"
	validateNamespace = "default"
	validateImage     = "mcr.microsoft.com/azurelinux/base/core:3.0"
)

type validateNodesSpec struct {
	KubeContext string
	GPUClass    string
	Selector    string
	MinHealthy  int
	Timeout     time.Duration
}

type validateNodesRunner interface {
	Raw(ctx context.Context, args []string, stdin []byte) (string, error)
}

func newClusterValidateNodesCmd() *cobra.Command {
	var spec validateNodesSpec
	var timeoutStr string

	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Validate GPU node health before training",
		Long: `Run health checks on GPU nodes to verify readiness for training workloads.

Creates a short-lived privileged pod on each GPU node, checks nvidia-smi, NVLink,
InfiniBand, and ECC status, then reports results and cleans up.

Requires cluster-admin or equivalent RBAC to create privileged pods.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			spec.Timeout, err = time.ParseDuration(timeoutStr)
			if err != nil {
				return fmt.Errorf("invalid --timeout: %w", err)
			}
			r := kube.New(spec.KubeContext)
			return runClusterValidateNodes(cmd.Context(), r, spec, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	cmd.Flags().StringVar(&spec.KubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&spec.GPUClass, "gpu-class", "", fmt.Sprintf("filter nodes by %s label value (e.g. h200-141gb)", workloadmeta.NodeLabelGPUClass))
	cmd.Flags().StringVar(&spec.Selector, "selector", "", "custom node label selector (alternative to --gpu-class)")
	cmd.Flags().IntVar(&spec.MinHealthy, "min-healthy", 0, "fail if fewer than N nodes are healthy")
	cmd.Flags().StringVar(&timeoutStr, "timeout", "2m", "per-pod validation timeout")
	return cmd
}

func runClusterValidateNodes(ctx context.Context, r validateNodesRunner, spec validateNodesSpec, out, errOut io.Writer) error {
	selector := spec.Selector
	if spec.GPUClass != "" {
		gpuClass, deprecated := topology.NormalizeGPUClass(spec.GPUClass)
		if !topology.IsSupportedGPUClass(gpuClass) {
			return fmt.Errorf("unsupported --gpu-class %q; use one of %s", spec.GPUClass, strings.Join(topology.SupportedGPUClasses(), ", "))
		}
		if deprecated {
			fmt.Fprintf(errOut, "warning: gpu_class %q is deprecated; use %q instead\n", spec.GPUClass, gpuClass)
		}
		if gpuClass == topology.GPUClassAny {
			if selectorReferencesGPUClass(selector) {
				return fmt.Errorf("--gpu-class any is unconstrained and cannot be combined with a --selector that references %s", workloadmeta.NodeLabelGPUClass)
			}
		} else {
			selector = workloadmeta.NodeLabelGPUClass + "=" + gpuClass
		}
	}

	fmt.Fprintf(out, "discovering GPU nodes")
	if selector != "" {
		fmt.Fprintf(out, " (selector: %s)", selector)
	}

	fmt.Fprintln(out, "...")

	// One node fetch feeds both the GPU inventory and the stranded-node report,
	// so the two views can never disagree about what the cluster looks like.
	items, err := fetchNodeItems(ctx, r, selector)
	if err != nil {
		return fmt.Errorf("discover GPU nodes: %w", err)
	}
	draDevices := nodesWithResourceSlices(ctx, r)
	nodes := classifyGPUNodes(items, draDevices)
	// GPUs reach the scheduler either as device-plugin extended resources
	// (nvidia.com/*) or as DRA ResourceSlices. A node offering neither has
	// nothing registered: AKS installs the NVIDIA *driver* automatically but no
	// device plugin, so a perfectly healthy GPU node can report zero capacity.
	// Reported explicitly, because the bare "no GPU nodes found" below sends
	// people looking at quota and node pools instead of at device registration.
	if stranded := classifyStrandedGPUNodes(items, draDevices); len(stranded) > 0 {
		fmt.Fprintf(out, "\n%d node(s) look like GPU hardware but expose no schedulable GPUs:\n", len(stranded))
		for _, n := range stranded {
			fmt.Fprintf(out, "  %s (%s)\n", n.Name, n.InstanceType)
		}
		fmt.Fprintln(out, "  no nvidia.com/* allocatable resource and no DRA ResourceSlice covers them,")
		fmt.Fprintln(out, "  so neither a device plugin nor a DRA driver has registered their GPUs;")
		fmt.Fprintln(out, "  until one does, no GPU workload can be scheduled onto these nodes.")
	}
	if len(nodes) == 0 {
		fmt.Fprintln(out, "no GPU nodes found")
		if spec.MinHealthy > 0 {
			return fmt.Errorf("only 0 healthy nodes, need at least %d", spec.MinHealthy)
		}
		return nil
	}
	fmt.Fprintf(out, "found %d GPU node(s), running health checks (timeout %s)...\n\n", len(nodes), spec.Timeout)

	results := runValidationPods(ctx, r, nodes, spec.Timeout)

	printNodeHealthTable(out, results)

	healthy := 0
	for _, res := range results {
		if res.Status == statusHealthy {
			healthy++
		}
	}
	fmt.Fprintf(out, "\n%d/%d nodes healthy\n", healthy, len(results))

	if spec.MinHealthy > 0 {
		if healthy < spec.MinHealthy {
			return fmt.Errorf("only %d healthy nodes, need at least %d", healthy, spec.MinHealthy)
		}
		return nil
	}
	for _, res := range results {
		if res.Status == statusUnhealthy || res.Status == statusUnknown || res.PodError != "" {
			return fmt.Errorf("node validation failed: %d/%d nodes healthy", healthy, len(results))
		}
	}
	return nil
}

func selectorReferencesGPUClass(selector string) bool {
	parsed, err := klabels.Parse(selector)
	if err != nil {
		return strings.Contains(selector, workloadmeta.NodeLabelGPUClass)
	}
	requirements, selectable := parsed.Requirements()
	if !selectable {
		return false
	}
	for _, requirement := range requirements {
		if requirement.Key() == workloadmeta.NodeLabelGPUClass {
			return true
		}
	}
	return false
}

// gpuSource records how a node's GPUs reach the scheduler. Managed workflows
// can submit against device-plugin, DRA, or MIG resources, so detection has to
// understand all of them.
type gpuSource string

const (
	gpuSourceDevicePlugin gpuSource = "device-plugin"
	gpuSourceMIG          gpuSource = "mig"
	gpuSourceDRA          gpuSource = "dra"
)

type gpuNodeInfo struct {
	Name         string
	GPUClass     string
	InstanceType string
	// AllocGPU is the schedulable whole-GPU count. It is 0 for MIG nodes,
	// where allocatable capacity is measured in profile slices and the
	// physical GPU count is not derivable from it.
	AllocGPU  int
	Source    gpuSource
	MIGSlices int
}

type nodeItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

func fetchNodeItems(ctx context.Context, r validateNodesRunner, selector string) ([]nodeItem, error) {
	args := []string{"get", "nodes", "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	raw, err := r.Raw(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []nodeItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("parse nodes json: %w", err)
	}
	return doc.Items, nil
}

func nodeInstanceType(labels map[string]string) string {
	if t := labels["node.kubernetes.io/instance-type"]; t != "" {
		return t
	}
	return labels["beta.kubernetes.io/instance-type"]
}

// classifyGPUNodes turns raw nodes into the GPU inventory to validate.
//
// Precedence is deliberate and each node matches at most one arm, so a node
// that advertises a device-plugin resource *and* publishes DRA ResourceSlices
// (a driver mid-migration, or a cluster running both) is counted exactly once.
//
//	nvidia.com/gpu: N   -> N whole GPUs, device plugin
//	nvidia.com/mig-*    -> MIG; slice inventory recorded, whole-GPU count unknown
//	ResourceSlices      -> DRA device count
//
// MIG deliberately does not try to infer physical GPUs from slice counts:
// nvidia.com/mig-1g.10gb: 7 is one A100 with seven 1g profiles or several
// partly-partitioned GPUs, and the quantity alone cannot tell them apart. The
// physical count comes from nvidia-smi during validation instead, so the node
// is reported with a real number rather than 0 or a misleading slice total.
func classifyGPUNodes(items []nodeItem, draDevices map[string]int) []gpuNodeInfo {
	var nodes []gpuNodeInfo
	for _, it := range items {
		info := gpuNodeInfo{
			Name:         it.Metadata.Name,
			GPUClass:     it.Metadata.Labels[workloadmeta.NodeLabelGPUClass],
			InstanceType: nodeInstanceType(it.Metadata.Labels),
		}
		switch whole, mig := countNVIDIAResources(it.Status.Allocatable); {
		case whole > 0:
			info.Source = gpuSourceDevicePlugin
			info.AllocGPU = whole
		case mig > 0:
			info.Source = gpuSourceMIG
			info.MIGSlices = mig
		case draDevices[it.Metadata.Name] > 0:
			info.Source = gpuSourceDRA
			info.AllocGPU = draDevices[it.Metadata.Name]
		default:
			continue
		}
		nodes = append(nodes, info)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes
}

// countNVIDIAResources splits allocatable nvidia.com/* capacity into whole GPUs
// (nvidia.com/gpu) and MIG profile slices (nvidia.com/mig-<profile>, summed
// across profiles since a node may expose several). Any other nvidia.com/*
// resource is a resourceName override for whole GPUs and counts as such.
func countNVIDIAResources(allocatable map[string]string) (whole, mig int) {
	for name, qty := range allocatable {
		suffix, ok := strings.CutPrefix(name, "nvidia.com/")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(qty)
		// The zero-quantity skip below is redundant today, and no test catches
		// its removal -- do not read this comment as a claim that one will.
		// The property it backstops is real, and comes from real hardware:
		//
		//   MIG-capable cards running in whole-GPU mode advertise their
		//   profiles at quantity 0 alongside a normal whole-GPU count. An H200
		//   on <cluster> reports nvidia.com/gpu:8 with mig-1g.18gb,
		//   mig-2g.35gb and mig-3g.71gb all "0". A profile with no capacity is
		//   not a partition, and such a node must stay on the device-plugin
		//   arm; classified as MIG it reports AllocGPU 0 for 8 healthy GPUs.
		//
		// What actually holds that property is summing advertised quantities
		// rather than counting profile keys. That is not silent to change:
		// TestClassifyGPUNodesReportsMIGSlicesWithoutFakingWholeGPUs asserts a
		// summed slice total across two profiles, so a switch to key counting
		// fails immediately, for its own reasons and regardless of this skip.
		// You therefore cannot reach a state where this skip is load-bearing
		// without tripping that test on the way. Only if someone changes both
		// does the zero-quantity property itself break, and
		// TestCountNVIDIAResourcesIgnoresZeroQuantityMIGProfiles is what names
		// it. TestClassifyGPUNodesAgainstRealClusterInventory does not: the
		// device-plugin arm is decided on the whole-GPU count before MIG is
		// consulted, so the real H200 classifies correctly either way.
		//
		// Note for anyone verifying by mutation: grep for the code, not for a
		// phrase this comment also contains, or an unapplied mutation and an
		// applied one look identical. Check the build separately too -- a
		// mutation that does not compile also prints FAIL.
		if err != nil || n <= 0 {
			continue
		}
		if strings.HasPrefix(suffix, "mig-") {
			mig += n
			continue
		}
		whole += n
	}
	return whole, mig
}

// discoverStrandedGPUNodes returns nodes that advertise no allocatable
// nvidia.com/gpu but carry the labels AKS puts on GPU SKUs. Those nodes have
// physical GPUs that nothing has registered, which is what a missing device
// plugin looks like from the API server.
func discoverStrandedGPUNodes(ctx context.Context, r validateNodesRunner, selector string) ([]gpuNodeInfo, error) {
	items, err := fetchNodeItems(ctx, r, selector)
	if err != nil {
		return nil, err
	}
	return classifyStrandedGPUNodes(items, nodesWithResourceSlices(ctx, r)), nil
}

func classifyStrandedGPUNodes(items []nodeItem, draDevices map[string]int) []gpuNodeInfo {
	var nodes []gpuNodeInfo
	for _, it := range items {
		// Not just nvidia.com/gpu: MIG advertises partition resources such as
		// nvidia.com/mig-1g.10gb. Any nvidia.com/* resource means something has
		// registered the devices.
		if advertisesNVIDIAResource(it.Status.Allocatable) {
			continue
		}
		// DRA publishes GPUs as ResourceSlices instead of node extended
		// resources, so a healthy DRA node legitimately advertises no
		// nvidia.com/* resource and is not stranded. Presence, not device
		// count: a driver that has claimed the node has registered, even if it
		// is momentarily publishing an empty device list.
		if _, claimed := draDevices[it.Metadata.Name]; claimed {
			continue
		}
		instanceType := nodeInstanceType(it.Metadata.Labels)
		if !looksLikeGPUNode(it.Metadata.Labels, instanceType) {
			continue
		}
		nodes = append(nodes, gpuNodeInfo{
			Name:         it.Metadata.Name,
			GPUClass:     it.Metadata.Labels[workloadmeta.NodeLabelGPUClass],
			InstanceType: instanceType,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes
}

// advertisesNVIDIAResource reports whether any nvidia.com/* extended resource is
// allocatable on the node. Covers whole GPUs (nvidia.com/gpu) and MIG
// partitions (nvidia.com/mig-1g.10gb).
func advertisesNVIDIAResource(allocatable map[string]string) bool {
	for name, qty := range allocatable {
		if !strings.HasPrefix(name, "nvidia.com/") {
			continue
		}
		if n, err := strconv.Atoi(qty); err == nil && n > 0 {
			return true
		}
	}
	return false
}

// nodesWithResourceSlices returns, per node, the number of distinct GPU devices
// published through DRA ResourceSlices. Best effort: clusters without the
// resource.k8s.io API, or without permission to list it, simply yield an empty
// map, and the caller falls back to extended-resource detection alone.
//
// Devices are deduplicated by (driver, pool, device name) because a pool can be
// split across several slices, and slices are re-published on driver restart.
// MIG devices exposed through DRA collapse onto their parent GPU for the same
// reason the extended-resource path refuses to infer physical GPUs from slice
// counts: the researcher-visible unit is the GPU, not the partition.
func nodesWithResourceSlices(ctx context.Context, r validateNodesRunner) map[string]int {
	raw, err := r.Raw(ctx, []string{"get", "resourceslices", "-o", "json"}, nil)
	if err != nil {
		return nil
	}
	var doc struct {
		Items []struct {
			Spec struct {
				Driver string `json:"driver"`
				Pool   struct {
					Name string `json:"name"`
				} `json:"pool"`
				NodeName string `json:"nodeName"`
				Devices  []struct {
					Name string `json:"name"`
				} `json:"devices"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil
	}
	seen := make(map[string]map[string]bool)
	for _, it := range doc.Items {
		node := it.Spec.NodeName
		if node == "" || !isGPUDRADriver(it.Spec.Driver) {
			continue
		}
		if seen[node] == nil {
			seen[node] = make(map[string]bool)
		}
		for _, dev := range it.Spec.Devices {
			if dev.Name == "" {
				continue
			}
			seen[node][it.Spec.Driver+"/"+it.Spec.Pool.Name+"/"+draParentDevice(dev.Name)] = true
		}
	}
	// Nodes are keyed even when their slice currently lists no devices: the key
	// records that a GPU DRA driver has claimed the node, which is what the
	// stranded-node report needs, while the count is what the inventory needs.
	out := make(map[string]int, len(seen))
	for node, devices := range seen {
		out[node] = len(devices)
	}
	return out
}

// isGPUDRADriver reports whether a DRA driver publishes GPUs. DRA is a generic
// device framework, so counting every driver's devices as GPUs would credit a
// node for its NICs. The NVIDIA driver registers as gpu.nvidia.com; the match
// stays loose enough to cover vendor and fork naming. An unset driver is not
// excluded — the filter exists to drop drivers that are known not to be GPU
// drivers, not to drop everything it cannot positively identify.
func isGPUDRADriver(driver string) bool {
	d := strings.ToLower(strings.TrimSpace(driver))
	if d == "" {
		return true
	}
	return strings.Contains(d, "gpu") || strings.Contains(d, "nvidia")
}

// draParentDevice maps a MIG device name published through DRA back to the
// physical GPU that owns it, so seven 1g profiles on one A100 count as one GPU.
// Non-MIG device names are returned unchanged.
func draParentDevice(name string) string {
	if parent, _, found := strings.Cut(name, "-mig-"); found && parent != "" {
		return parent
	}
	return name
}

// looksLikeGPUNode reports whether a node is GPU hardware independent of
// whether anything has registered the devices. AKS sets
// kubernetes.azure.com/accelerator on GPU node pools, and the NVIDIA GPU VM
// families are the N-series (NC/ND/NV), which is what the instance type encodes.
func looksLikeGPUNode(labels map[string]string, instanceType string) bool {
	if labels["kubernetes.azure.com/accelerator"] != "" || labels["accelerator"] != "" {
		return true
	}
	if labels[workloadmeta.NodeLabelGPUClass] != "" {
		return true
	}
	name := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(instanceType), "STANDARD_"))
	switch {
	case strings.HasPrefix(name, "NC"), strings.HasPrefix(name, "ND"), strings.HasPrefix(name, "NV"):
		return true
	}
	return false
}

type nodeHealthResult struct {
	Node         string
	GPUCount     int
	AllocGPU     int
	Source       gpuSource
	MIGSlices    int
	NVLinkOK     bool
	NVLinkDetail string
	IBTotal      int
	IBActive     int
	ECCErrors    int
	DriverVer    string
	Status       healthStatus
	Reasons      []string
	PodError     string
}

type healthStatus int

const (
	statusHealthy healthStatus = iota
	statusDegraded
	statusUnhealthy
	statusUnknown
)

func (s healthStatus) String() string {
	switch s {
	case statusHealthy:
		return "HEALTHY"
	case statusDegraded:
		return "DEGRADED"
	case statusUnhealthy:
		return "UNHEALTHY"
	default:
		return "UNKNOWN"
	}
}

func runValidationPods(ctx context.Context, r validateNodesRunner, nodes []gpuNodeInfo, timeout time.Duration) []nodeHealthResult {
	results := make([]nodeHealthResult, len(nodes))
	for i, node := range nodes {
		results[i] = runSingleValidation(ctx, r, node, timeout)
	}
	return results
}

func runSingleValidation(ctx context.Context, r validateNodesRunner, node gpuNodeInfo, timeout time.Duration) nodeHealthResult {
	podName := validationPodName(node.Name, time.Now().UnixNano())

	result := nodeHealthResult{
		Node:      node.Name,
		AllocGPU:  node.AllocGPU,
		Source:    node.Source,
		MIGSlices: node.MIGSlices,
		Status:    statusUnknown,
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = r.Raw(cleanupCtx, []string{
			"delete", "pod", podName, "-n", validateNamespace,
			"--ignore-not-found", "--wait=false",
		}, nil)
	}()

	podJSON := buildValidationPodSpec(podName, node.Name)
	if _, err := r.Raw(ctx, []string{"create", "-f", "-", "-n", validateNamespace}, []byte(podJSON)); err != nil {
		result.PodError = fmt.Sprintf("create pod: %v", err)
		return result
	}

	timeoutSec := max(int(timeout.Seconds()), 10)
	waitOut, err := r.Raw(ctx, []string{
		"wait", "pod", podName, "-n", validateNamespace,
		"--for=jsonpath={.status.phase}=Succeeded",
		fmt.Sprintf("--timeout=%ds", timeoutSec),
	}, nil)
	if err != nil {
		failOut, _ := r.Raw(ctx, []string{
			"get", "pod", podName, "-n", validateNamespace,
			"-o", "jsonpath={.status.phase}",
		}, nil)
		if strings.TrimSpace(failOut) == "Failed" {
			logs, _ := r.Raw(ctx, []string{"logs", podName, "-n", validateNamespace}, nil)
			return assessHealth(parseValidationOutput(logs), node)
		}
		result.PodError = fmt.Sprintf("wait: %s %v", strings.TrimSpace(waitOut), err)
		return result
	}

	termMsg, _ := r.Raw(ctx, []string{
		"get", "pod", podName, "-n", validateNamespace,
		"-o", "jsonpath={.status.containerStatuses[0].state.terminated.message}",
	}, nil)

	output := strings.TrimSpace(termMsg)
	if output == "" {
		output, _ = r.Raw(ctx, []string{"logs", podName, "-n", validateNamespace}, nil)
	}

	return assessHealth(parseValidationOutput(output), node)
}

func validationPodName(nodeName string, suffix int64) string {
	name := fmt.Sprintf("%s%s-%x", validatePodPrefix, nodeName, suffix)
	if len(name) > 63 {
		suffixPart := fmt.Sprintf("-%x", suffix)
		maxNodeLen := 63 - len(validatePodPrefix) - len(suffixPart)
		if maxNodeLen < 1 {
			maxNodeLen = 1
		}
		nodeName = strings.TrimRight(nodeName[:min(len(nodeName), maxNodeLen)], "-")
		if nodeName == "" {
			nodeName = "node"
		}
		name = validatePodPrefix + nodeName + suffixPart
	}
	return strings.TrimRight(name, "-")
}

func buildValidationPodSpec(podName, nodeName string) string {
	script := `#!/bin/sh
exec 2>&1
tdnf install -y --quiet util-linux >/dev/null 2>&1 || tdnf install -y util-linux >/dev/null 2>&1
ns() { nsenter -t 1 -m -u -i -n -p -- "$@"; }

# GPU count
gpu_list=$(ns nvidia-smi --list-gpus 2>&1)
gpu_rc=$?
echo "nvidia_smi_rc=$gpu_rc"
gpu_count=$(echo "$gpu_list" | grep -c "^GPU ")
echo "gpu_count=$gpu_count"

# Driver version
driver=$(ns nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1 | tr -d ' ')
echo "driver_version=$driver"

# MIG mode. A GPU with MIG enabled and no MIG instances configured still shows
# up in nvidia-smi, but CUDA cannot create a context on it: cuInit() returns
# 100 CUDA_ERROR_NO_DEVICE and torch reports device_count>0 with
# is_available False. Standard_NC24ads_A100_v4 ships this way.
mig=$(ns nvidia-smi --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | head -1 | tr -d '[:space:]')
echo "mig_mode=$mig"
# Count configured MIG instances. MIG with instances is a supported way to run
# GPUs; MIG with none is the broken state, because there is no device to open.
mig_instances=$(ns nvidia-smi -L 2>/dev/null | grep -c "MIG " || true)
echo "mig_instances=$mig_instances"

# NVLink
nvlink_out=$(ns nvidia-smi nvlink --status 2>&1)
nvlink_rc=$?
echo "nvlink_rc=$nvlink_rc"
if [ $nvlink_rc -eq 0 ] && [ -n "$nvlink_out" ]; then
  inactive=$(echo "$nvlink_out" | grep -c "<inactive>" || true)
  echo "nvlink_inactive=$inactive"
else
  echo "nvlink_inactive=-1"
fi

# ECC uncorrectable errors
ecc_dbe=0
ecc_out=$(ns nvidia-smi --query-gpu=ecc.errors.uncorrected.aggregate.dram --format=csv,noheader 2>/dev/null)
if [ $? -eq 0 ] && [ -n "$ecc_out" ]; then
  for val in $ecc_out; do
    v=$(echo "$val" | tr -d '[:space:]')
    case "$v" in ''|*[!0-9]*) continue ;; esac
    ecc_dbe=$((ecc_dbe + v))
  done
fi
echo "ecc_dbe=$ecc_dbe"

# InfiniBand
ib_total=0
ib_active=0
for state_file in /sys/class/infiniband/*/ports/*/state; do
  [ -f "$state_file" ] || continue
  ib_total=$((ib_total + 1))
  if grep -q "ACTIVE" "$state_file" 2>/dev/null; then
    ib_active=$((ib_active + 1))
  fi
done
echo "ib_total=$ib_total"
echo "ib_active=$ib_active"
`
	spec := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      podName,
			"namespace": validateNamespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "tau-validate",
			},
		},
		"spec": map[string]any{
			"nodeName":      nodeName,
			"hostPID":       true,
			"hostNetwork":   true,
			"restartPolicy": "Never",
			"containers": []map[string]any{{
				"name":    "validate",
				"image":   validateImage,
				"command": []string{"/bin/sh", "-c", script},
				"securityContext": map[string]any{
					"privileged": true,
				},
				"terminationMessagePath":   "/dev/termination-log",
				"terminationMessagePolicy": "FallbackToLogsOnError",
			}},
			"tolerations": []map[string]any{
				{"operator": "Exists"},
			},
		},
	}
	data, _ := json.Marshal(spec)
	return string(data)
}

type validationData struct {
	NvidiaSMIRC    int
	GPUCount       int
	DriverVersion  string
	NVLinkRC       int
	NVLinkInactive int
	ECCErrors      int
	IBTotal        int
	IBActive       int
	MIGMode        string
	MIGInstances   int
	HasOutput      bool
}

func parseValidationOutput(raw string) validationData {
	var d validationData
	d.NVLinkInactive = -1

	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "nvidia_smi_rc":
			d.NvidiaSMIRC, _ = strconv.Atoi(v)
			d.HasOutput = true
		case "gpu_count":
			d.GPUCount, _ = strconv.Atoi(v)
		case "driver_version":
			d.DriverVersion = v
		case "nvlink_rc":
			d.NVLinkRC, _ = strconv.Atoi(v)
		case "nvlink_inactive":
			d.NVLinkInactive, _ = strconv.Atoi(v)
		case "ecc_dbe":
			d.ECCErrors, _ = strconv.Atoi(v)
		case "ib_total":
			d.IBTotal, _ = strconv.Atoi(v)
		case "ib_active":
			d.IBActive, _ = strconv.Atoi(v)
		case "mig_mode":
			d.MIGMode = v
		case "mig_instances":
			d.MIGInstances, _ = strconv.Atoi(v)
		}
	}
	return d
}

func assessHealth(d validationData, node gpuNodeInfo) nodeHealthResult {
	res := nodeHealthResult{
		Node:      node.Name,
		GPUCount:  d.GPUCount,
		AllocGPU:  node.AllocGPU,
		Source:    node.Source,
		MIGSlices: node.MIGSlices,
		NVLinkOK:  true,
		IBTotal:   d.IBTotal,
		IBActive:  d.IBActive,
		ECCErrors: d.ECCErrors,
		DriverVer: d.DriverVersion,
		Status:    statusHealthy,
	}

	if !d.HasOutput {
		res.Status = statusUnknown
		res.PodError = "no validation output received"
		return res
	}

	if d.NvidiaSMIRC != 0 {
		res.Status = statusUnhealthy
		res.Reasons = append(res.Reasons, "nvidia-smi failed")
		return res
	}

	// MIG being enabled is not itself a fault. What is a fault is MIG enabled
	// with zero instances configured: nvidia-smi still
	// reports a healthy GPU, but there is no device to open, so every workload
	// dies at the first cuInit() with 100 CUDA_ERROR_NO_DEVICE. That state is
	// the AKS default on some A100 SKUs and is otherwise very hard to read.
	if strings.EqualFold(d.MIGMode, "Enabled") && d.MIGInstances == 0 {
		res.Status = statusUnhealthy
		res.Reasons = append(res.Reasons, "MIG is enabled but no MIG instances are configured; CUDA cannot create a context (configure MIG instances, or run `nvidia-smi -mig 0` and restart the node)")
	}

	// AllocGPU is 0 on MIG nodes, where allocatable capacity is counted in
	// profile slices, so this comparison is skipped there by construction
	// rather than firing on every healthy MIG node.
	if node.AllocGPU > 0 && d.GPUCount < node.AllocGPU {
		res.Status = statusUnhealthy
		res.Reasons = append(res.Reasons, fmt.Sprintf("GPU count %d < expected %d", d.GPUCount, node.AllocGPU))
	}

	if d.NVLinkInactive > 0 {
		res.NVLinkOK = false
		res.NVLinkDetail = fmt.Sprintf("%d inactive", d.NVLinkInactive)
		res.Status = statusUnhealthy
		res.Reasons = append(res.Reasons, fmt.Sprintf("NVLink %d inactive links", d.NVLinkInactive))
	} else if d.NVLinkInactive < 0 && d.NVLinkRC != 0 {
		res.NVLinkOK = false
		res.NVLinkDetail = "query failed"
		if res.Status == statusHealthy {
			res.Status = statusDegraded
		}
		res.Reasons = append(res.Reasons, "NVLink status unavailable")
	}

	if d.ECCErrors > 0 {
		if res.Status == statusHealthy {
			res.Status = statusDegraded
		}
		res.Reasons = append(res.Reasons, fmt.Sprintf("ECC %d uncorrectable", d.ECCErrors))
	}

	if d.IBTotal > 0 && d.IBActive < d.IBTotal {
		if res.Status == statusHealthy {
			res.Status = statusDegraded
		}
		res.Reasons = append(res.Reasons, fmt.Sprintf("IB %d/%d active", d.IBActive, d.IBTotal))
	}

	return res
}

func printNodeHealthTable(out io.Writer, results []nodeHealthResult) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer tw.Flush()

	fmt.Fprintf(tw, "NODE\tGPUS\tSOURCE\tNVLINK\tIB\tECC\tDRIVER\tSTATUS\n")
	for _, r := range results {
		gpus := "-"
		switch {
		case r.Source == gpuSourceMIG:
			// AllocGPU is meaningless on MIG nodes, so show what is actually
			// known: physical GPUs seen by nvidia-smi, and the slice inventory.
			gpus = fmt.Sprintf("%d (%d slices)", r.GPUCount, r.MIGSlices)
		case r.AllocGPU > 0 || r.GPUCount > 0:
			gpus = fmt.Sprintf("%d/%d", r.GPUCount, r.AllocGPU)
		}

		source := string(r.Source)
		if source == "" {
			source = "-"
		}

		nvlink := "OK"
		if r.NVLinkDetail != "" {
			nvlink = r.NVLinkDetail
		} else if !r.NVLinkOK {
			nvlink = "FAIL"
		}

		ib := "-"
		if r.IBTotal > 0 {
			ib = fmt.Sprintf("%d/%d", r.IBActive, r.IBTotal)
		}

		ecc := "OK"
		if r.ECCErrors > 0 {
			ecc = fmt.Sprintf("%d DBE", r.ECCErrors)
		}

		status := r.Status.String()
		if r.PodError != "" {
			status = "UNKNOWN"
		}
		if len(r.Reasons) > 0 {
			status += " (" + strings.Join(r.Reasons, ", ") + ")"
		}
		if r.PodError != "" {
			status += " (" + r.PodError + ")"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Node, gpus, source, nvlink, ib, ecc, dash(r.DriverVer), status)
	}
}
