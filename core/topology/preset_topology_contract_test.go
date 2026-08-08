// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package topology

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/resourceprofile"
)

const managedDevicePluginTopology = "default-node-topology"

func managedDevicePluginGPUPresets(t *testing.T, policy Policy) map[string]Preset {
	t.Helper()
	presets := make(map[string]Preset)
	for name, preset := range policy.Presets {
		if preset.GPUClass == "" || !usesManagedSharedGPUQueue(preset) ||
			strings.HasSuffix(preset.ResourceFlavor, "-dra") {
			continue
		}
		presets[name] = preset
	}
	if len(presets) == 0 {
		t.Fatalf("policy %s exposes no managed device-plugin GPU presets", policy.SourceFile)
	}
	return presets
}

func topologyContractProfile() profile.Profile {
	return profile.Profile{
		Name: "topology-contract",
		Spec: map[string]any{
			"policy": map[string]any{
				"preemptable":         true,
				"checkpointOnPreempt": true,
			},
		},
	}
}

func TestEmbeddedDevicePluginGPUPresetsEnableTAS(t *testing.T) {
	policy, err := LoadPolicy("")
	if err != nil {
		t.Fatalf("load embedded policy: %v", err)
	}

	for name, preset := range managedDevicePluginGPUPresets(t, policy) {
		t.Run(name, func(t *testing.T) {
			if preset.TopologyName != managedDevicePluginTopology {
				t.Fatalf("topologyName=%q want %q", preset.TopologyName, managedDevicePluginTopology)
			}

			resolved := policy.resolve(preset)
			if resolved.Options.DisableKueueTopologyAnnotations {
				t.Fatal("device-plugin preset disables Kueue TAS annotations")
			}
			plan, err := Build(topologyContractProfile(), resolved.Options)
			if err != nil {
				t.Fatalf("build topology plan: %v", err)
			}

			switch preset.Placement {
			case "independent":
				if got := plan.Annotations[unconstrainedTopologyAnnot]; got != "true" {
					t.Fatalf("unconstrained topology annotation=%q want true; annotations=%v", got, plan.Annotations)
				}
			case "single-node-nvlink":
				if got := plan.Annotations[requiredTopologyAnnotation]; got != hostnameTopology {
					t.Fatalf("required topology annotation=%q want %q; annotations=%v", got, hostnameTopology, plan.Annotations)
				}
			case "multi-node-nccl":
				if got := plan.Annotations[unconstrainedTopologyAnnot]; got != "true" {
					t.Fatalf("unconstrained topology annotation=%q want true; annotations=%v", got, plan.Annotations)
				}
				if value, ok := plan.Annotations[preferredTopologyAnnotation]; ok {
					t.Fatalf("multi-node preset emitted unsupported preferred topology %q", value)
				}
			default:
				t.Fatalf("managed GPU preset has untested placement %q", preset.Placement)
			}
		})
	}
}

func TestWithDRAQueueStripsEmbeddedPresetTASAnnotations(t *testing.T) {
	for _, name := range []string{
		"azure.research.training.l",
		"azure.research.large-memory.2x",
		"azure.research.large-memory.2node",
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := ResolvePreset("", name)
			if err != nil {
				t.Fatalf("resolve preset: %v", err)
			}
			dra := WithDRAQueue(resolved)
			if dra.Preset.TopologyName != "" || !dra.Options.DisableKueueTopologyAnnotations {
				t.Fatalf("DRA conversion retained TAS contract: %+v", dra)
			}
			plan, err := Build(profile.Profile{Name: "dra-topology-contract"}, dra.Options)
			if err != nil {
				t.Fatalf("build DRA topology plan: %v", err)
			}
			for _, annotation := range []string{
				unconstrainedTopologyAnnot,
				requiredTopologyAnnotation,
				preferredTopologyAnnotation,
			} {
				if value, ok := plan.Annotations[annotation]; ok {
					t.Errorf("DRA plan retained TAS annotation %s=%q", annotation, value)
				}
			}
		})
	}
}
