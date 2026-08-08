// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"fmt"
	"strings"
)

// GPU request mechanisms. A profile selects one via
// spec.resources.gpu.requestVia. Device-plugin requests the NVIDIA
// device-plugin extended resource (nvidia.com/gpu); DRA attaches a
// ResourceClaim from spec.resources.dra.claimTemplate.
//
// requestVia is matched case-insensitively after normalizing `_` and spaces to
// `-` (see normalizeGPUValue). For device-plugin the canonical spelling is
// "device-plugin"; the aliases "deviceplugin", "dp", and "nvidia.com-gpu"
// (i.e. "nvidia.com/gpu" with the slash written as a dash) also resolve to it.
// DRA is selected with "dra". Prefer the canonical "device-plugin" / "dra" in
// shipped profiles; the aliases exist only to tolerate user typos.
const (
	GPURequestDevicePlugin = "device-plugin"
	gpuRequestDRA          = "dra"
	gpuRequestMIG          = "mig"

	// defaultGPUResourceName is the device-plugin extended resource requested
	// when a profile does not override spec.resources.gpu.resourceName.
	defaultGPUResourceName = "nvidia.com/gpu"
)

// GPURequestPlan is the resolved, renderer-agnostic decision for how a profile
// asks the cluster for GPUs. It is the single source of truth shared by the
// submit/serve renderers and autocapture so none of them
// inspect spec.resources.dra directly.
type GPURequestPlan struct {
	// Mode is GPURequestDevicePlugin, gpuRequestDRA, gpuRequestMIG, or ""
	// (no GPU request). A non-empty value that is not one of the known modes
	// signals an invalid requestVia; validation surfaces it as an error.
	Mode string

	// ResourceName is the device-plugin extended resource (device-plugin mode).
	ResourceName string

	// Count is the whole-GPU cardinality from spec.resources.gpu.count.
	Count int

	// ClaimTemplate is the DRA ResourceClaimTemplate name (dra mode).
	ClaimTemplate string
}

// GPURequestPlanFromProfile resolves the GPU request mechanism for a profile.
// Back-compat: when requestVia is unset, a profile with a dra.claimTemplate is
// treated as DRA (the historical behavior); otherwise Mode is "".
func GPURequestPlanFromProfile(p Profile) GPURequestPlan {
	plan := GPURequestPlan{ResourceName: defaultGPUResourceName}

	var gpu map[string]any
	if res, ok := p.Spec["resources"].(map[string]any); ok {
		gpu, _ = res["gpu"].(map[string]any)
	}
	if gpu != nil {
		if rn, ok := gpu["resourceName"].(string); ok && strings.TrimSpace(rn) != "" {
			plan.ResourceName = strings.TrimSpace(rn)
		}
	}

	if c, ok, err := GPUContractFromProfile(p); err == nil && ok {
		plan.Count = c.Count
	}
	plan.ClaimTemplate = DRAClaimTemplate(p)

	via := ""
	if gpu != nil {
		via, _ = gpu["requestVia"].(string)
	}
	switch normalizeGPUValue(via) {
	case GPURequestDevicePlugin, "deviceplugin", "dp", "nvidia.com-gpu":
		plan.Mode = GPURequestDevicePlugin
	case gpuRequestDRA:
		plan.Mode = gpuRequestDRA
	case gpuRequestMIG, "mig-slice":
		plan.Mode = gpuRequestMIG
	case "":
		if plan.ClaimTemplate != "" {
			plan.Mode = gpuRequestDRA
		}
	default:
		plan.Mode = normalizeGPUValue(via) // invalid; flagged by validation
	}
	return plan
}

// ApplyGPUResources mutates a container-level resources map (the map that
// becomes container.resources in the pod spec) according to the plan and
// returns the pod-level resourceClaims slice to attach, if any.
//
//   - device-plugin: sets ResourceName=Count in both requests and limits
//     (Kubernetes forbids overcommitting extended resources, so the two must be
//     equal; setting both is unambiguous for the Kueue admission webhook).
//     A pre-existing entry for ResourceName with a different value is an error.
//   - dra: when a ClaimTemplate is set, adds container resources.claims and
//     returns the pod resourceClaims entry. The DRA path leaves a stale dra
//     block from device-plugin profiles untouched (the renderer never calls
//     this in dra mode for those).
//   - "": no GPU resources are added.
func ApplyGPUResources(resources map[string]any, plan GPURequestPlan) ([]any, error) {
	switch plan.Mode {
	case GPURequestDevicePlugin, gpuRequestMIG:
		if plan.Count <= 0 {
			return nil, fmt.Errorf("requestVia: %s requires spec.resources.gpu.count > 0", GPURequestDevicePlugin)
		}
		name := plan.ResourceName
		if name == "" {
			name = defaultGPUResourceName
		}
		requests, err := gpuQuantityMap(resources["requests"], name, plan.Count)
		if err != nil {
			return nil, fmt.Errorf("resources.requests: %w", err)
		}
		limits, err := gpuQuantityMap(resources["limits"], name, plan.Count)
		if err != nil {
			return nil, fmt.Errorf("resources.limits: %w", err)
		}
		resources["requests"] = requests
		resources["limits"] = limits
		return nil, nil
	case gpuRequestDRA:
		if plan.ClaimTemplate == "" {
			return nil, nil
		}
		resources["claims"] = []any{
			map[string]any{"name": "gpu"},
		}
		return []any{
			map[string]any{
				"name":                      "gpu",
				"resourceClaimTemplateName": plan.ClaimTemplate,
			},
		}, nil
	default:
		return nil, nil
	}
}

// gpuQuantityMap returns a copy of the given requests/limits map with the GPU
// resource set to count, erroring if the map already pins a different value.
func gpuQuantityMap(existing any, name string, count int) (map[string]any, error) {
	out := map[string]any{}
	if m, ok := existing.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	if v, ok := out[name]; ok && !gpuQuantityEquals(v, count) {
		return nil, fmt.Errorf("conflicting %s=%v (device-plugin request is %d)", name, v, count)
	}
	out[name] = count
	return out, nil
}

func gpuQuantityEquals(v any, count int) bool {
	switch typed := v.(type) {
	case int:
		return typed == count
	case int64:
		return typed == int64(count)
	case float64:
		return typed == float64(count)
	case string:
		return strings.TrimSpace(typed) == fmt.Sprintf("%d", count)
	default:
		return false
	}
}
