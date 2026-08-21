// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type selectedWorkloadProfile struct {
	Selection    profile.Selection
	Render       profile.Profile
	ClusterQueue string
}

func resolveSnapshotRunWorkloadProfile(ctx context.Context, o unresolvedRunOptions) (unresolvedRunOptions, error) {
	if strings.TrimSpace(o.workloadProfileSnapshot) == "" {
		return o, fmt.Errorf("policy.workload_profile_snapshot is required for snapshot profile resolution")
	}
	if o.dryRun != "client" {
		return o, fmt.Errorf(
			"policy.workload_profile_snapshot requires --dry-run=client; snapshots cannot be used for server dry-run or apply",
		)
	}
	for _, scope := range []struct {
		field string
		value string
	}{
		{field: "policy.namespace", value: o.namespace},
		{field: "policy.team", value: o.team},
		{field: "policy.lane", value: o.lane},
	} {
		if strings.TrimSpace(scope.value) == "" {
			return o, fmt.Errorf(
				"%s is required with policy.workload_profile_snapshot so offline profile selection has explicit namespace/team/lane authorization scope",
				scope.field,
			)
		}
	}
	data, err := os.ReadFile(o.workloadProfileSnapshot)
	if err != nil {
		return o, fmt.Errorf("read policy.workload_profile_snapshot %q: %w", o.workloadProfileSnapshot, err)
	}
	provider, err := profile.DecodeSnapshotProvider(data)
	if err != nil {
		return o, fmt.Errorf("load policy.workload_profile_snapshot %q: %w", o.workloadProfileSnapshot, err)
	}
	return selectRunWorkloadProfile(ctx, o, provider)
}

func resolveClusterRunWorkloadProfile(ctx context.Context, o unresolvedRunOptions) (unresolvedRunOptions, error) {
	kubeContext := firstNonEmpty(o.kubeContext, defaultKubeContext())
	client, err := newClusterProfileClient(kubeContext)
	if err != nil {
		return o, err
	}
	return selectRunWorkloadProfile(ctx, o, profile.NewClusterProvider(client))
}

func selectRunWorkloadProfile(
	ctx context.Context,
	o unresolvedRunOptions,
	provider *profile.Provider,
) (unresolvedRunOptions, error) {
	profileName := o.profileName
	if !o.profileNameExplicit && (o.explicitPolicyFields != nil || o.selectedWorkloadProfile != nil) {
		profileName = ""
	}
	selection, err := provider.Select(ctx, profile.SelectionRequest{
		Name:      profileName,
		Namespace: o.namespace,
		Team:      o.team,
		Lane:      o.lane,
	})
	if err != nil {
		return o, err
	}
	renderProfile, err := selection.Profile.RenderProfile(o.namespace, o.team, o.lane)
	if err != nil {
		return o, fmt.Errorf("convert workload profile %q for rendering: %w", selection.Profile.Name, err)
	}
	return applySelectedWorkloadProfile(o, selection, renderProfile)
}

func applySelectedWorkloadProfile(
	o unresolvedRunOptions,
	selection profile.Selection,
	renderProfile profile.Profile,
) (unresolvedRunOptions, error) {
	conflicts := []struct {
		field    string
		key      string
		explicit string
		selected string
	}{
		{field: "policy.queue", key: "queue", explicit: o.queue, selected: renderProfile.Queue},
		{field: "policy.mode", key: "mode", explicit: o.mode, selected: renderProfile.Topology.Mode},
		{field: "policy.topology", key: "topology", explicit: o.topology, selected: renderProfile.Topology.Placement},
		{field: "policy.workload_priority_class", key: "workload_priority_class", explicit: o.workloadPriorityClass, selected: renderProfile.Topology.WorkloadPriorityClassName},
		{field: "policy.pod_priority_class", key: "pod_priority_class", explicit: o.podPriorityClass, selected: renderProfile.Topology.PodPriorityClassName},
	}
	for _, conflict := range conflicts {
		explicit := strings.TrimSpace(conflict.explicit)
		wasExplicit := policyFieldWasExplicit(o, conflict.key, explicit != "")
		if conflict.key == "queue" && o.workspaceQueueResolved {
			wasExplicit = true
		}
		if wasExplicit && explicit != "" &&
			explicit != strings.TrimSpace(conflict.selected) {
			return o, fmt.Errorf(
				"%s=%q conflicts with authoritative workload profile %q value %q",
				conflict.field,
				explicit,
				selection.Profile.Name,
				conflict.selected,
			)
		}
	}
	if o.disablePrioritiesExplicit &&
		o.disableDefaultPriorities != renderProfile.Topology.DisableDefaultPriorities {
		return o, fmt.Errorf(
			"policy.disable_default_priorities=%t conflicts with authoritative workload profile %q value %t",
			o.disableDefaultPriorities,
			selection.Profile.Name,
			renderProfile.Topology.DisableDefaultPriorities,
		)
	}
	if policyFieldWasExplicit(o, "priority_tier", strings.TrimSpace(o.priorityTier) != "") {
		return o, fmt.Errorf(
			"policy.priority_tier cannot be combined with authoritative priority classes from workload profile %q; remove policy.priority_tier",
			selection.Profile.Name,
		)
	}
	for _, unsupported := range []struct {
		field string
		key   string
		set   bool
	}{
		{field: "policy.shape", key: "shape", set: strings.TrimSpace(o.shape) != ""},
		{field: "policy.gpu_class", key: "gpu_class", set: strings.TrimSpace(o.gpuClass) != ""},
		{field: "policy.node_selector", key: "node_selector", set: len(o.nodeSelectors) > 0},
		{field: "policy.clear_node_selector", key: "clear_node_selector", set: o.clearNodeSelector},
	} {
		if policyFieldWasExplicit(o, unsupported.key, unsupported.set) {
			return o, fmt.Errorf(
				"%s cannot be combined with authoritative workload profile %q; select a profile with the required platform placement",
				unsupported.field,
				selection.Profile.Name,
			)
		}
	}

	profileWorkers := int(selection.Profile.WorkerCount)
	profileGPUs := int(selection.Profile.GPUsPerWorker)
	if o.file != "" {
		if o.workersExplicit && o.workers != profileWorkers {
			return o, profileCardinalityConflict(selection.Profile.Name, "compute.workers", o.workers, profileWorkers)
		}
		if jobGPUsWereExplicit(o) && o.jobGPUs != nil && *o.jobGPUs != profileGPUs {
			return o, profileCardinalityConflict(selection.Profile.Name, "compute.gpus", *o.jobGPUs, profileGPUs)
		}
		if o.gpusPerWorkerExplicit && o.gpusPerWorker != profileGPUs {
			return o, profileCardinalityConflict(selection.Profile.Name, "compute.gpus_per_worker", o.gpusPerWorker, profileGPUs)
		}
		o.workers = profileWorkers
		o.gpusPerWorker = profileGPUs
	} else {
		engine, err := resolveDirectRunEngine(o)
		if err != nil {
			return o, err
		}
		switch engine {
		case directRunEngineJob:
			if jobGPUsWereExplicit(o) && o.jobGPUs != nil && *o.jobGPUs != profileGPUs {
				return o, profileCardinalityConflict(selection.Profile.Name, "compute.gpus", *o.jobGPUs, profileGPUs)
			}
			if o.nodesExplicit && o.nodes != profileWorkers {
				return o, profileCardinalityConflict(selection.Profile.Name, "execution.nodes", o.nodes, profileWorkers)
			}
			gpus := profileGPUs
			o.jobGPUs = &gpus
			o.nodes = profileWorkers
		case directRunEngineRayJob:
			if o.workersExplicit && o.workers != profileWorkers {
				return o, profileCardinalityConflict(selection.Profile.Name, "compute.workers", o.workers, profileWorkers)
			}
			if o.gpusPerWorkerExplicit && o.gpusPerWorker != profileGPUs {
				return o, profileCardinalityConflict(selection.Profile.Name, "compute.gpus_per_worker", o.gpusPerWorker, profileGPUs)
			}
			o.workers = profileWorkers
			o.gpusPerWorker = profileGPUs
		}
	}

	o.profileName = selection.Profile.Name
	o.queue = renderProfile.Queue
	o.mode = renderProfile.Topology.Mode
	o.topology = renderProfile.Topology.Placement
	o.priorityTier = ""
	o.workloadPriorityClass = renderProfile.Topology.WorkloadPriorityClassName
	o.podPriorityClass = renderProfile.Topology.PodPriorityClassName
	o.disableDefaultPriorities = renderProfile.Topology.DisableDefaultPriorities
	clusterQueue := ""
	if o.selectedWorkloadProfile != nil {
		clusterQueue = o.selectedWorkloadProfile.ClusterQueue
	}
	if clusterQueue == "" {
		resolvedClusterQueue, err := selection.Profile.ClusterQueueFor(o.namespace, renderProfile.Queue)
		if err != nil {
			return o, fmt.Errorf("resolve authoritative workload profile queue binding: %w", err)
		}
		clusterQueue = resolvedClusterQueue
	}
	o.selectedWorkloadProfile = &selectedWorkloadProfile{
		Selection:    selection,
		Render:       renderProfile,
		ClusterQueue: clusterQueue,
	}
	return o, nil
}

func policyFieldWasExplicit(o unresolvedRunOptions, field string, fallback bool) bool {
	if o.explicitPolicyFields != nil {
		return o.explicitPolicyFields[field]
	}
	return o.selectedWorkloadProfile == nil && fallback
}

func jobGPUsWereExplicit(o unresolvedRunOptions) bool {
	return o.jobGPUsExplicit || o.selectedWorkloadProfile == nil && o.jobGPUs != nil
}

func profileCardinalityConflict(profileName, field string, explicit, selected int) error {
	return fmt.Errorf(
		"%s=%d conflicts with authoritative workload profile %q value %d",
		field,
		explicit,
		profileName,
		selected,
	)
}

func stampSelectedWorkloadProfile(
	labels map[string]string,
	annotations map[string]string,
	selected *selectedWorkloadProfile,
) (map[string]string, map[string]string, error) {
	if selected == nil {
		return labels, annotations, fmt.Errorf("no TauCluster workload profile was selected for rendering")
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	labels[workloadmeta.LabelProfile] = selected.Selection.Profile.Name
	annotations[workloadmeta.AnnotationTauClusterGeneration] = strconv.FormatInt(selected.Selection.Generation, 10)
	annotations[workloadmeta.AnnotationWorkloadProfileSetHash] = selected.Selection.ProfileSetHash
	annotations[workloadmeta.AnnotationWorkloadProfileName] = selected.Selection.Profile.Name
	return labels, annotations, nil
}

func validateSelectedWorkloadProfileMode(selected *selectedWorkloadProfile, dryRun string) error {
	if selected == nil {
		return fmt.Errorf("no TauCluster workload profile was selected for rendering")
	}
	if selected.Selection.Source == profile.ProfileSourceSnapshot && dryRun != "client" {
		return fmt.Errorf(
			"policy.workload_profile_snapshot requires --dry-run=client; snapshots cannot be used for server dry-run or apply",
		)
	}
	return nil
}
