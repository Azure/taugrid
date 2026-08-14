// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const defaultGPUResourceName = "nvidia.com/gpu"

// GPUContract is the profile-level device-plugin GPU contract.
type GPUContract struct {
	Count int
	Size  string
}

func (c GPUContract) Labels() map[string]string {
	out := map[string]string{}
	if c.Count > 0 {
		out[workloadmeta.AnnotationGPUCount] = strconv.Itoa(c.Count)
	}
	if c.Size != "" {
		out[workloadmeta.LabelGPUSize] = c.Size
	}
	return out
}

func (c GPUContract) Annotations() map[string]string {
	out := c.Labels()
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
	return strings.Join(parts, ",")
}

// AddGPUResources adds the NVIDIA device-plugin resource to requests and
// limits. A non-positive count is a no-op for CPU-only profiles.
func AddGPUResources(resources map[string]any, count int) {
	if count <= 0 {
		return
	}
	for _, key := range []string{"requests", "limits"} {
		values := map[string]any{}
		if existing, ok := resources[key].(map[string]any); ok {
			for name, value := range existing {
				values[name] = value
			}
		}
		values[defaultGPUResourceName] = count
		resources[key] = values
	}
}
