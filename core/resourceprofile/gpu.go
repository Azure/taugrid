// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"fmt"
	"github.com/Azure/taugrid/core/workloadmeta"
	"regexp"
	"strconv"
	"strings"
)

const (
	gpuPlacementSingleDevice       = "single-device"
	gpuPlacementSameNode           = "same-node"
	gpuPlacementDistributedWorkers = "distributed-workers"
)

var claimGPUCountRE = regexp.MustCompile(`(^|[^0-9])([1-9][0-9]*)-?gpus?($|[^a-z0-9])`)

// GPUContract is the explicit profile-level GPU cardinality and placement
// contract under spec.resources.gpu.
type GPUContract struct {
	Count        int
	Size         string
	Placement    string
	MemoryGiBMin int
}

type GPUSchedulingPlan struct {
	Contract        GPUContract
	Labels          map[string]string
	Annotations     map[string]string
	PackingAffinity map[string]any
}

// GPUContractFromProfile reads spec.resources.gpu. Missing blocks are allowed
// for backwards compatibility and return ok=false.
func GPUContractFromProfile(p Profile) (GPUContract, bool, error) {
	res, ok := p.Spec["resources"].(map[string]any)
	if !ok {
		return GPUContract{}, false, nil
	}
	raw, ok := res["gpu"]
	if !ok {
		return GPUContract{}, false, nil
	}
	gpu, ok := raw.(map[string]any)
	if !ok {
		return GPUContract{}, false, fmt.Errorf("spec.resources.gpu must be a map")
	}
	c := GPUContract{}
	if rawCount, exists := gpu["count"]; exists {
		count, err := positiveInt(rawCount)
		if err != nil {
			return GPUContract{}, true, fmt.Errorf("spec.resources.gpu.count: %w", err)
		}
		c.Count = count
	}
	if rawMemory, exists := gpu["memoryGiBMin"]; exists {
		memory, err := positiveInt(rawMemory)
		if err != nil {
			return GPUContract{}, true, fmt.Errorf("spec.resources.gpu.memoryGiBMin: %w", err)
		}
		c.MemoryGiBMin = memory
	}
	if size, ok := gpu["size"].(string); ok {
		c.Size = normalizeGPUValue(size)
	}
	if placement, ok := gpu["placement"].(string); ok {
		c.Placement = normalizeGPUPlacement(placement)
		if !validGPUPlacement(c.Placement) {
			return GPUContract{}, true, fmt.Errorf("spec.resources.gpu.placement=%q is invalid (allowed: %s|%s|%s)", c.Placement, gpuPlacementSingleDevice, gpuPlacementSameNode, gpuPlacementDistributedWorkers)
		}
	}
	if c.Placement == gpuPlacementSingleDevice && c.Count > 1 {
		return GPUContract{}, true, fmt.Errorf("spec.resources.gpu.placement=%s requires count=1, got count=%d", gpuPlacementSingleDevice, c.Count)
	}
	if c.Placement == gpuPlacementSameNode && c.Count == 1 {
		return GPUContract{}, true, fmt.Errorf("spec.resources.gpu.placement=%s requires count>=2, got count=1", gpuPlacementSameNode)
	}
	return c, true, nil
}

func BuildGPUSchedulingPlan(p Profile) (GPUSchedulingPlan, error) {
	if err := validateGPUContract(p); err != nil {
		return GPUSchedulingPlan{}, err
	}
	c, ok, err := GPUContractFromProfile(p)
	if err != nil || !ok {
		return GPUSchedulingPlan{}, err
	}
	return GPUSchedulingPlan{
		Contract:        c,
		Labels:          c.Labels(),
		Annotations:     c.Annotations(),
		PackingAffinity: gpuBinPackingAffinity(c),
	}, nil
}

// validateGPUContract rejects a profile whose GPU contract cannot be rendered
// into a coherent pod spec. It is called by BuildGPUSchedulingPlan, so every
// render path that requests GPUs is gated on it.
func validateGPUContract(p Profile) error {
	c, ok, err := GPUContractFromProfile(p)
	if err != nil {
		return fmt.Errorf("profile %q GPU contract invalid: %s", p.Name, err.Error())
	}
	if !ok {
		return nil
	}

	var errs []string
	plan := GPURequestPlanFromProfile(p)
	switch plan.Mode {
	case GPURequestDevicePlugin:
		if c.Count <= 0 {
			errs = append(errs, fmt.Sprintf("requestVia: %s requires spec.resources.gpu.count > 0", GPURequestDevicePlugin))
		}
	case gpuRequestMIG:
		if c.Count <= 0 {
			errs = append(errs, fmt.Sprintf("requestVia: %s requires spec.resources.gpu.count > 0", gpuRequestMIG))
		}
		if !strings.HasPrefix(plan.ResourceName, "nvidia.com/mig-") {
			errs = append(errs, fmt.Sprintf("requestVia: %s requires spec.resources.gpu.resourceName to be a nvidia.com/mig-<profile> resource, got %q", gpuRequestMIG, plan.ResourceName))
		}
	case gpuRequestDRA:
		// DRA cross-checks below.
	case "":
		if c.Count > 0 {
			errs = append(errs, "spec.resources.gpu.count is set but the profile has no GPU request mechanism; set spec.resources.gpu.requestVia: device-plugin or spec.resources.dra.claimTemplate")
		}
	default:
		errs = append(errs, fmt.Sprintf("spec.resources.gpu.requestVia=%q is invalid (allowed: %s|%s|%s)", plan.Mode, GPURequestDevicePlugin, gpuRequestDRA, gpuRequestMIG))
	}

	// DRA claimTemplate cross-checks only apply when the profile actually
	// requests GPUs via DRA; a stale/inherited dra block on a device-plugin
	// or MIG profile is ignored.
	if plan.Mode != GPURequestDevicePlugin && plan.Mode != gpuRequestMIG {
		claimTemplate := DRAClaimTemplate(p)
		claimCount, claimKnown := GPUCountFromClaimTemplate(claimTemplate)
		if claimTemplate != "" && c.Count > 0 && claimKnown && claimCount != c.Count {
			errs = append(errs, fmt.Sprintf("spec.resources.gpu.count=%d does not match DRA claimTemplate %q (%d GPUs)", c.Count, claimTemplate, claimCount))
		}
		if claimKnown {
			switch c.Placement {
			case gpuPlacementSingleDevice:
				if claimCount != 1 {
					errs = append(errs, fmt.Sprintf("placement=%s requires a one-GPU claimTemplate, got %q (%d GPUs)", gpuPlacementSingleDevice, claimTemplate, claimCount))
				}
			case gpuPlacementSameNode:
				if claimCount < 2 {
					errs = append(errs, fmt.Sprintf("placement=%s requires a multi-GPU same-pod claimTemplate, got %q", gpuPlacementSameNode, claimTemplate))
				}
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("profile %q GPU contract invalid: %s", p.Name, strings.Join(errs, "; "))
}

func (c GPUContract) Labels() map[string]string {
	out := map[string]string{}
	if c.Count > 0 {
		out[workloadmeta.AnnotationGPUCount] = strconv.Itoa(c.Count)
	}
	if c.Size != "" {
		out[workloadmeta.LabelGPUSize] = c.Size
	}
	if c.Placement != "" {
		out[workloadmeta.LabelGPUPlacement] = c.Placement
	}
	return out
}

func (c GPUContract) Annotations() map[string]string {
	out := c.Labels()
	if c.MemoryGiBMin > 0 {
		out[workloadmeta.AnnotationGPUMemoryGiBMin] = strconv.Itoa(c.MemoryGiBMin)
	}
	if summary := c.Summary(); summary != "" {
		out[workloadmeta.AnnotationGPUContract] = summary
	}
	return out
}

func (c GPUContract) Summary() string {
	var parts []string
	if c.Count > 0 {
		parts = append(parts, "count="+strconv.Itoa(c.Count))
	}
	if c.Size != "" {
		parts = append(parts, "size="+c.Size)
	}
	if c.Placement != "" {
		parts = append(parts, "placement="+c.Placement)
	}
	if c.MemoryGiBMin > 0 {
		parts = append(parts, "memoryGiBMin="+strconv.Itoa(c.MemoryGiBMin))
	}
	return strings.Join(parts, ",")
}

func DRAClaimTemplate(p Profile) string {
	res, ok := p.Spec["resources"].(map[string]any)
	if !ok {
		return ""
	}
	dra, ok := res["dra"].(map[string]any)
	if !ok {
		return ""
	}
	claimTemplate, _ := dra["claimTemplate"].(string)
	return strings.TrimSpace(claimTemplate)
}

func GPUCountFromClaimTemplate(name string) (int, bool) {
	normalized := normalizeGPUValue(name)
	if normalized == "" {
		return 0, false
	}
	switch normalized {
	case "full-gpu", "ds-full-gpu", "one-gpu", "one-gpus", "single-gpu":
		return 1, true
	case "two-gpu", "two-gpus":
		return 2, true
	case "four-gpu", "four-gpus":
		return 4, true
	case "eight-gpu", "eight-gpus":
		return 8, true
	}
	m := claimGPUCountRE.FindStringSubmatch(normalized)
	if len(m) == 4 {
		n, err := strconv.Atoi(m[2])
		if err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func gpuBinPackingAffinity(c GPUContract) map[string]any {
	if !shouldBinPackGPUNode(c) {
		return nil
	}
	return map[string]any{
		"podAffinity": map[string]any{
			"preferredDuringSchedulingIgnoredDuringExecution": []any{
				map[string]any{
					"weight": int64(100),
					"podAffinityTerm": map[string]any{
						"topologyKey": "kubernetes.io/hostname",
						"labelSelector": map[string]any{
							"matchExpressions": []any{
								map[string]any{
									"key":      workloadmeta.AnnotationGPUCount,
									"operator": "Exists",
								},
								map[string]any{
									"key":      workloadmeta.LabelGPUPlacement,
									"operator": "In",
									"values":   []any{gpuPlacementSingleDevice, gpuPlacementSameNode},
								},
							},
						},
					},
				},
			},
		},
	}
}

func shouldBinPackGPUNode(c GPUContract) bool {
	if c.Count == 1 && c.Placement == gpuPlacementSingleDevice {
		return true
	}
	return c.Count >= 2 && c.Count <= 4 && c.Placement == gpuPlacementSameNode
}

func positiveInt(v any) (int, error) {
	switch typed := v.(type) {
	case int:
		if typed > 0 {
			return typed, nil
		}
	case int64:
		if typed > 0 {
			return int(typed), nil
		}
	case float64:
		if typed > 0 && typed == float64(int(typed)) {
			return int(typed), nil
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("must be a positive integer, got %v", v)
}

func normalizeGPUPlacement(v string) string {
	v = normalizeGPUValue(v)
	switch v {
	case "single", "one", "one-gpu", "single-gpu", "single-full-gpu":
		return gpuPlacementSingleDevice
	case "same-pod", "single-node", "same-host", "same-hostname":
		return gpuPlacementSameNode
	case "distributed", "per-worker", "per-rank", "worker":
		return gpuPlacementDistributedWorkers
	default:
		return v
	}
}

func normalizeGPUValue(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.ReplaceAll(v, "_", "-")
	v = strings.ReplaceAll(v, " ", "-")
	return v
}

func validGPUPlacement(v string) bool {
	switch v {
	case "", gpuPlacementSingleDevice, gpuPlacementSameNode, gpuPlacementDistributedWorkers:
		return true
	default:
		return false
	}
}
