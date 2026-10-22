// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpuinventory

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

type podResources struct {
	total        quantityMap
	pending      quantityMap
	bound        quantityMap
	countUnknown bool
	diagnostics  []Diagnostic
}

type quantityMap map[string]resource.Quantity

func calculatePodResources(p podObject) podResources {
	diagnostics := []Diagnostic{}
	regular := quantityMap{}
	for _, container := range p.Spec.Containers {
		addQuantities(regular, containerGPUResources(container, &diagnostics))
	}

	restartable := quantityMap{}
	peakInit := quantityMap{}
	for _, container := range p.Spec.InitContainers {
		current := containerGPUResources(container, &diagnostics)
		if strings.EqualFold(strings.TrimSpace(container.RestartPolicy), "Always") {
			addQuantities(restartable, current)
			maxQuantities(peakInit, restartable)
			continue
		}
		withSidecars := cloneQuantities(restartable)
		addQuantities(withSidecars, current)
		maxQuantities(peakInit, withSidecars)
	}
	addQuantities(regular, restartable)
	maxQuantities(regular, peakInit)
	addResourceList(regular, p.Spec.Overhead, &diagnostics)

	out := podResources{
		total:        regular,
		countUnknown: len(p.Spec.ResourceClaims) > 0,
		diagnostics:  diagnostics,
	}
	if out.countUnknown {
		out.diagnostics = appendDiagnostic(out.diagnostics, Diagnostic{
			Code:    "dra_count_unknown",
			Message: "Pod resourceClaims may request GPU devices whose count is not available from Pod resources.",
		})
	}
	for _, container := range append(append([]podContainer(nil), p.Spec.Containers...), p.Spec.InitContainers...) {
		if len(container.Resources.Claims) == 0 {
			continue
		}
		out.countUnknown = true
		out.diagnostics = appendDiagnostic(out.diagnostics, Diagnostic{
			Code:    "dra_count_unknown",
			Message: "Container resource claims may request GPU devices whose count is not available from Pod resources.",
		})
	}
	if strings.TrimSpace(p.Spec.NodeName) == "" {
		out.pending = cloneQuantities(out.total)
	} else {
		out.bound = cloneQuantities(out.total)
	}
	return out
}

func containerGPUResources(container podContainer, diagnostics *[]Diagnostic) quantityMap {
	out := quantityMap{}
	names := map[string]struct{}{}
	for name := range container.Resources.Requests {
		if isGPUResource(name) {
			names[name] = struct{}{}
		}
	}
	for name := range container.Resources.Limits {
		if isGPUResource(name) {
			names[name] = struct{}{}
		}
	}
	for name := range names {
		value, ok := container.Resources.Requests[name]
		if !ok {
			value = container.Resources.Limits[name]
		}
		addQuantity(out, name, value, diagnostics)
	}
	return out
}

func addResourceList(dst quantityMap, values map[string]string, diagnostics *[]Diagnostic) {
	for name, value := range values {
		if isGPUResource(name) {
			addQuantity(dst, name, value, diagnostics)
		}
	}
}

func addQuantity(dst quantityMap, name, raw string, diagnostics *[]Diagnostic) {
	quantity, err := resource.ParseQuantity(raw)
	if err != nil || quantity.Sign() < 0 {
		*diagnostics = appendDiagnostic(*diagnostics, Diagnostic{
			Code:    "invalid_gpu_quantity",
			Message: "A GPU resource quantity was invalid and was not counted.",
		})
		return
	}
	if quantity.Sign() == 0 {
		return
	}
	if _, exact := quantity.AsInt64(); !exact {
		*diagnostics = appendDiagnostic(*diagnostics, Diagnostic{
			Code:    "invalid_gpu_quantity",
			Message: "A GPU resource quantity was invalid and was not counted.",
		})
		return
	}
	current := dst[name]
	current.Add(quantity)
	dst[name] = current
}

func addQuantities(dst, src quantityMap) {
	for name, quantity := range src {
		current := dst[name]
		current.Add(quantity)
		dst[name] = current
	}
}

func maxQuantities(dst, candidate quantityMap) {
	for name, quantity := range candidate {
		current, ok := dst[name]
		if !ok || current.Cmp(quantity) < 0 {
			dst[name] = quantity.DeepCopy()
		}
	}
}

func cloneQuantities(src quantityMap) quantityMap {
	if len(src) == 0 {
		return quantityMap{}
	}
	out := make(quantityMap, len(src))
	for name, quantity := range src {
		out[name] = quantity.DeepCopy()
	}
	return out
}

func isGPUResource(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == fullGPUResource || strings.HasPrefix(name, "nvidia.com/mig-") {
		return true
	}
	slash := strings.IndexByte(name, '/')
	if slash <= 0 || slash == len(name)-1 {
		return false
	}
	resourceName := name[slash+1:]
	return strings.Contains(resourceName, "gpu") || strings.Contains(resourceName, "mig")
}

func quantityList(values quantityMap) []ResourceQuantity {
	out := make([]ResourceQuantity, 0, len(values))
	for name, quantity := range values {
		if quantity.Sign() <= 0 {
			continue
		}
		out = append(out, ResourceQuantity{Name: name, Count: quantity.Value()})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func quantityValue(values quantityMap, name string) int64 {
	quantity, ok := values[name]
	if !ok || quantity.Sign() <= 0 {
		return 0
	}
	return quantity.Value()
}

func wholeGPUQuantity(values quantityMap) int64 {
	var total int64
	for name, quantity := range values {
		if quantity.Sign() > 0 && strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "/gpu") {
			total += quantity.Value()
		}
	}
	return total
}

func appendDiagnostic(values []Diagnostic, candidate Diagnostic) []Diagnostic {
	for _, value := range values {
		if value.Code == candidate.Code {
			return values
		}
	}
	return append(values, candidate)
}
