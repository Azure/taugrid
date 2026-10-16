// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/runconfig"
)

var placementPolicyKnobs = map[string]string{
	"team":                       "policy.team",
	"lane":                       "policy.lane",
	"mode":                       "policy.mode",
	"topology":                   "policy.topology",
	"shape":                      "policy.shape",
	"gpu-class":                  "policy.gpu_class",
	"queue":                      "policy.queue",
	"priority-tier":              "policy.priority_tier",
	"workload-priority-class":    "policy.workload_priority_class",
	"pod-priority-class":         "policy.pod_priority_class",
	"disable-default-priorities": "policy.disable_default_priorities",
}

func commandPath(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil; c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}

func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, visit)
	}
}

func TestTopologyKnobsStayConfigFirst(t *testing.T) {
	root := NewRoot()
	for _, subtree := range []string{"run", "serve"} {
		target, _, err := root.Find([]string{subtree})
		if err != nil {
			t.Fatalf("find %q: %v", subtree, err)
		}
		walkCommands(target, func(cmd *cobra.Command) {
			path := commandPath(cmd)
			if path == "tau run list" {
				return
			}
			for knob, configKey := range placementPolicyKnobs {
				if cmd.Flags().Lookup(knob) != nil {
					t.Errorf("%s registers --%s, but that knob is config-first (%s); "+
						"either read the flag in configToDispatch or drop it", path, knob, configKey)
				}
			}
		})
	}
}

func TestTopologyKnobsAreReachableFromConfig(t *testing.T) {
	disablePriorities := true
	cfg := runconfig.Config{
		Policy: runconfig.Policy{
			Team:                     "research",
			Lane:                     "training",
			Mode:                     "fixed",
			Topology:                 "same-host",
			Shape:                    "8xa100-80gb",
			GPUClass:                 "a100-80gb",
			Queue:                    "research-training",
			PriorityTier:             "priority",
			WorkloadPriorityClass:    "taugrid-priority",
			PodPriorityClass:         "taugrid-priority",
			DisableDefaultPriorities: &disablePriorities,
		},
	}
	dispatch, err := configToDispatch(cfg, "tau.yaml")
	if err != nil {
		t.Fatalf("configToDispatch: %v", err)
	}
	flags := runJobTopologyFlags(dispatch)

	carried := map[string]string{
		"team":                    flags.team,
		"lane":                    flags.lane,
		"mode":                    flags.mode,
		"topology":                flags.topology,
		"shape":                   flags.shape,
		"gpu-class":               flags.gpuClass,
		"queue":                   flags.queue,
		"priority-tier":           flags.priorityTier,
		"workload-priority-class": flags.workloadPriorityClass,
		"pod-priority-class":      flags.podPriorityClass,
	}
	for knob, got := range carried {
		if got == "" {
			t.Errorf("runJobTopologyFlags drops %q; %s cannot reach the renderer", knob, placementPolicyKnobs[knob])
		}
	}
	if !flags.disableDefaultPriorities {
		t.Errorf("runJobTopologyFlags drops disable-default-priorities; %s cannot reach the renderer",
			placementPolicyKnobs["disable-default-priorities"])
	}

	for knob := range placementPolicyKnobs {
		if !runJobTopologyFieldSet(dispatch, knob) {
			t.Errorf("runJobTopologyFieldSet(%q) = false for a config-set value; the override will never fire", knob)
		}
	}
}
