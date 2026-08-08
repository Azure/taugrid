// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/runconfig"
)

// topologyPolicyKnobs pairs each placement knob with the run config key that
// owns it. `tau run` is config-first: these knobs are read from the config's
// policy block by configToDispatch, never from cobra flags.
//
// Registering any of them as a flag on `tau run` or `tau serve` would create a
// second source of truth, and a flag that is registered but never read (which
// is exactly what topologyFlags.addTo was) silently lies to anyone reading the
// code or the help output.
var topologyPolicyKnobs = map[string]string{
	"preset":                     "policy.preset",
	"topology-policy":            "policy.topology_policy",
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

// TestTopologyKnobsStayConfigFirst fails if a placement knob regains a cobra
// flag on the run/serve subtree without the config-first contract being
// revisited. It is scoped to run/serve because `tau cluster validate topology
// --preset` and `tau run list --queue` are unrelated, legitimate flags.
func TestTopologyKnobsStayConfigFirst(t *testing.T) {
	root := NewRoot()
	for _, subtree := range []string{"run", "serve"} {
		target, _, err := root.Find([]string{subtree})
		if err != nil {
			t.Fatalf("find %q: %v", subtree, err)
		}
		walkCommands(target, func(cmd *cobra.Command) {
			path := commandPath(cmd)
			// `tau run list` is a query command, not a submission path; its
			// --queue is a display filter over existing workloads.
			if path == "tau run list" {
				return
			}
			for knob, configKey := range topologyPolicyKnobs {
				if cmd.Flags().Lookup(knob) != nil {
					t.Errorf("%s registers --%s, but that knob is config-first (%s); "+
						"either read the flag in configToDispatch or drop it", path, knob, configKey)
				}
			}
		})
	}
}

// TestTopologyKnobsAreReachableFromConfig is the other half of the contract:
// every knob applyWithChanged can override must be settable from the config,
// otherwise the override is unreachable dead code (which is what
// --checkpoint-every was, and why policy.checkpoint_every is deliberately
// absent: see TestRunConfigRejectsCheckpointEvery).
//
// This drives the real config -> dispatch -> topologyFlags chain so a dropped
// mapping in configToDispatch fails here rather than in production.
func TestTopologyKnobsAreReachableFromConfig(t *testing.T) {
	// Non-empty distinct values so a copy/paste slip in configToDispatch or
	// runJobTopologyFlags shows up as a mismatch rather than passing.
	cfg := runconfig.Config{
		Policy: runconfig.Policy{
			Preset:                   "azure.research.training.l",
			TopologyPolicy:           "/tmp/policy.yaml",
			Team:                     "research",
			Lane:                     "training",
			Mode:                     "fixed",
			Topology:                 "single-node-nvlink",
			Shape:                    "8xa100-80gb",
			GPUClass:                 "a100-80gb",
			Queue:                    "research-training",
			PriorityTier:             "priority",
			WorkloadPriorityClass:    "taugrid-priority",
			PodPriorityClass:         "taugrid-priority",
			DisableDefaultPriorities: true,
		},
	}
	dispatch, err := configToDispatch(cfg, "tau.yaml")
	if err != nil {
		t.Fatalf("configToDispatch: %v", err)
	}
	flags := runJobTopologyFlags(dispatch)

	carried := map[string]string{
		"preset":                  flags.preset,
		"topology-policy":         flags.policyPath,
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
			t.Errorf("runJobTopologyFlags drops %q; %s cannot reach the renderer", knob, topologyPolicyKnobs[knob])
		}
	}
	if !flags.disableDefaultPriorities {
		t.Errorf("runJobTopologyFlags drops disable-default-priorities; %s cannot reach the renderer",
			topologyPolicyKnobs["disable-default-priorities"])
	}

	// Every knob must also report as "changed" so applyWithChanged actually
	// applies it instead of silently deferring to the preset.
	for knob := range topologyPolicyKnobs {
		if knob == "preset" || knob == "topology-policy" {
			// Consumed by resolvePreset, not by the override table.
			continue
		}
		if !runJobTopologyFieldSet(dispatch, knob) {
			t.Errorf("runJobTopologyFieldSet(%q) = false for a config-set value; the override will never fire", knob)
		}
	}
}
