// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"

	"github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
)

const defaultSyntheticResourceProfileImage = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"

func resourceProfileForRender(profileName string, preset *runtopology.ResolvedPreset, opts runtopology.Options, gpuCount int) profile.Profile {
	name := strings.TrimSpace(profileName)
	if name == "" && preset != nil {
		name = strings.TrimSpace(preset.Preset.Profile)
		if name == "" {
			name = preset.Preset.Name
		}
	}

	return profile.Profile{
		Name:  name,
		Lane:  firstNonEmpty(opts.Lane, presetValue(preset, func(p runtopology.Preset) string { return p.Lane })),
		Queue: firstNonEmpty(opts.QueueName, presetValue(preset, func(p runtopology.Preset) string { return p.QueueName })),
		Topology: profile.Topology{
			Team:                      strings.TrimSpace(opts.Team),
			Mode:                      strings.TrimSpace(opts.Mode),
			Placement:                 strings.TrimSpace(opts.Placement),
			Shape:                     strings.TrimSpace(opts.Shape),
			GPUClass:                  strings.TrimSpace(opts.GPUClass),
			PriorityTier:              strings.TrimSpace(opts.PriorityTier),
			PodPriorityClassName:      strings.TrimSpace(opts.PodPriorityClassName),
			WorkloadPriorityClassName: strings.TrimSpace(opts.WorkloadPriorityClassName),
			DisableDefaultPriorities:  opts.DisableDefaultPriorities,
		},
		Resources: profile.Resources{
			GPU: resourceProfileGPU(opts, preset, gpuCount),
		},
		Runtime: profile.Runtime{Image: defaultSyntheticResourceProfileImage},
	}
}

func resourceProfileGPU(opts runtopology.Options, preset *runtopology.ResolvedPreset, gpuCount int) profile.GPUContract {
	if gpuCount <= 0 {
		if count, ok, err := runtopology.GPUCountFromShape(opts.Shape); err == nil && ok {
			gpuCount = count
		}
	}
	if gpuCount <= 0 && preset != nil {
		if count, ok, err := runtopology.GPUCountFromShape(preset.Preset.Shape); err == nil && ok {
			gpuCount = count
		}
	}
	if gpuCount <= 0 {
		return profile.GPUContract{}
	}
	gpu := profile.GPUContract{Count: gpuCount}
	gpuClass := firstNonEmpty(opts.GPUClass, presetValue(preset, func(p runtopology.Preset) string { return p.GPUClass }))
	if gpuClass != "" && gpuClass != runtopology.GPUClassAny {
		gpu.Size = gpuClass
	}
	return gpu
}

func presetValue(preset *runtopology.ResolvedPreset, get func(runtopology.Preset) string) string {
	if preset == nil {
		return ""
	}
	return strings.TrimSpace(get(preset.Preset))
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (f topologyFlags) resourceProfileOptions() runtopology.Options {
	return runtopology.Options{
		Team:                      f.team,
		Lane:                      f.lane,
		Mode:                      f.mode,
		Placement:                 f.topology,
		Shape:                     f.shape,
		GPUClass:                  f.gpuClass,
		QueueName:                 f.queue,
		PriorityTier:              f.priorityTier,
		WorkloadPriorityClassName: f.workloadPriorityClass,
		PodPriorityClassName:      f.podPriorityClass,
		DisableDefaultPriorities:  f.disableDefaultPriorities,
	}
}
