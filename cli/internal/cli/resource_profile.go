package cli

import (
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
)

const defaultSyntheticResourceProfileImage = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0"

func resourceProfileForRender(profileName string, preset *runtopology.ResolvedPreset, opts runtopology.Options, gpuCount int) profile.Profile {
	name := strings.TrimSpace(profileName)
	if name == "" && preset != nil {
		name = strings.TrimSpace(preset.Preset.Profile)
		if name == "" {
			name = preset.Preset.Name
		}
	}

	spec := map[string]any{}
	if topologySpec := topologyProfileSpec(opts); len(topologySpec) > 0 {
		spec["topology"] = topologySpec
	}
	if queueName := firstNonEmpty(opts.QueueName, presetValue(preset, func(p runtopology.Preset) string { return p.QueueName })); queueName != "" {
		spec["queue"] = map[string]any{"localQueue": queueName}
	}
	if resourceSpec := resourceProfileSpec(opts, preset, gpuCount); len(resourceSpec) > 0 {
		spec["resources"] = resourceSpec
	}
	spec["runtime"] = map[string]any{"image": defaultSyntheticResourceProfileImage}

	return profile.Profile{
		Name: name,
		Lane: firstNonEmpty(opts.Lane, presetValue(preset, func(p runtopology.Preset) string { return p.Lane })),
		Spec: spec,
	}
}

func topologyProfileSpec(opts runtopology.Options) map[string]any {
	spec := map[string]any{}
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			spec[key] = strings.TrimSpace(value)
		}
	}
	add("team", opts.Team)
	add("lane", opts.Lane)
	add("mode", opts.Mode)
	add("placement", opts.Placement)
	add("shape", opts.Shape)
	add("gpuClass", opts.GPUClass)
	add("checkpointEvery", opts.CheckpointEvery)
	add("queueName", opts.QueueName)
	add("priorityTier", opts.PriorityTier)
	add("podPriorityClassName", opts.PodPriorityClassName)
	add("workloadPriorityClassName", opts.WorkloadPriorityClassName)
	if opts.DisableKueueTopologyAnnotations {
		spec["disableKueueTopologyAnnotations"] = true
	}
	if opts.DisableDefaultPriorities {
		spec["disableDefaultPriorities"] = true
	}
	return spec
}

func resourceProfileSpec(opts runtopology.Options, preset *runtopology.ResolvedPreset, gpuCount int) map[string]any {
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
		return nil
	}
	gpu := map[string]any{
		"count":      gpuCount,
		"requestVia": profile.GPURequestDevicePlugin,
	}
	gpuClass := firstNonEmpty(opts.GPUClass, presetValue(preset, func(p runtopology.Preset) string { return p.GPUClass }))
	if gpuClass != "" && gpuClass != runtopology.GPUClassAny {
		gpu["size"] = gpuClass
	}
	return map[string]any{"gpu": gpu}
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

func profileRuntimeImage(p profile.Profile) string {
	rt, ok := p.Spec["runtime"].(map[string]any)
	if !ok {
		return ""
	}
	image, _ := rt["image"].(string)
	return image
}

type profilePersistenceMount struct {
	PVC       string
	MountPath string
	ReadOnly  bool
}

func profilePersistenceMounts(p profile.Profile) []profilePersistenceMount {
	res, ok := p.Spec["resources"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := res["persistence"]
	if !ok {
		return nil
	}
	var mounts []profilePersistenceMount
	add := func(entry map[string]any) {
		pvc, _ := entry["pvcName"].(string)
		mountPath, _ := entry["mountPath"].(string)
		readOnly, _ := entry["readOnly"].(bool)
		if pvc != "" && mountPath != "" {
			mounts = append(mounts, profilePersistenceMount{PVC: pvc, MountPath: mountPath, ReadOnly: readOnly})
		}
	}
	switch v := raw.(type) {
	case map[string]any:
		add(v)
	case []any:
		for _, item := range v {
			if entry, ok := item.(map[string]any); ok {
				add(entry)
			}
		}
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].PVC == mounts[j].PVC {
			return mounts[i].MountPath < mounts[j].MountPath
		}
		return mounts[i].PVC < mounts[j].PVC
	})
	return mounts
}
