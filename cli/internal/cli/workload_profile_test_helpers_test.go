// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func attachAuthoritativeProfileForTest(o *runDispatchOptions) {
	if o.selectedWorkloadProfile != nil {
		return
	}

	name := strings.TrimSpace(o.profileName)
	if name == "" {
		name = "test.single-gpu"
	}
	team, lane := "research", "training"
	gpus, workers := 1, 1
	placement := profile.PlacementIndependent
	switch {
	case strings.Contains(name, ".eval."):
		lane = "eval"
	case strings.Contains(name, ".elastic."):
		lane = "elastic"
	case strings.Contains(name, ".large-memory."):
		lane = "large-memory"
		placement = profile.PlacementSingleNodeNVLink
	}
	switch {
	case strings.HasSuffix(name, ".2x"):
		gpus = 2
	case strings.HasSuffix(name, ".4x"):
		gpus = 4
	case strings.HasSuffix(name, ".xl"):
		gpus = 8
	case strings.HasSuffix(name, ".2node"):
		gpus, workers = 8, 2
		placement = profile.PlacementMultiNodeNCCL
	}
	if strings.Contains(name, ".experimental.") {
		team = "experimental"
	}
	namespace := strings.TrimSpace(o.namespace)
	if namespace == "" {
		namespace = "test-workspace"
		o.namespace = namespace
	}
	queue := strings.TrimSpace(o.queue)
	if queue == "" {
		queue = "jobqueue"
		if o.gpuResourceMode == "dra" {
			queue = "jobqueue-dra"
		}
		o.queue = queue
	}
	if o.team == "" {
		o.team = team
	}
	if o.lane == "" {
		o.lane = lane
	}
	if o.mode == "" {
		o.mode = profile.ModeFixed
	}
	if o.topology == "" {
		o.topology = placement
	}
	if o.workloadPriorityClass == "" && !o.disableDefaultPriorities {
		o.workloadPriorityClass = "taugrid-default"
	}
	if o.podPriorityClass == "" && !o.disableDefaultPriorities {
		o.podPriorityClass = "taugrid-default"
	}
	o.profileName = name
	intent := profile.WorkloadProfile{
		Name:              name,
		Applicability:     profile.ProfileApplicability{Teams: []string{o.team}, Lanes: []string{o.lane}, Namespaces: []string{namespace}},
		GPUsPerWorker:     int32(gpus),
		WorkerCount:       int32(workers),
		Mode:              o.mode,
		Placement:         o.topology,
		DefaultLocalQueue: queue,
		ExecutionTarget:   profile.ExecutionTargetSingleCluster,
		Priorities: profile.ProfilePriorities{
			WorkloadPriorityClassName: o.workloadPriorityClass,
			PodPriorityClassName:      o.podPriorityClass,
			DisableDefaultPriorities:  o.disableDefaultPriorities,
		},
	}
	o.selectedWorkloadProfile = &selectedWorkloadProfile{
		Selection: profile.Selection{
			Generation:     1,
			ProfileSetHash: "test-profile-set",
			Profile: profile.ResolvedWorkloadProfile{
				WorkloadProfile: intent,
				LocalQueues: []profile.ResolvedLocalQueue{{
					Namespace:    namespace,
					Name:         queue,
					ClusterQueue: "tau-cq",
				}},
			},
			Source: profile.ProfileSourceCluster,
		},
		Render: profile.Profile{
			Name:            name,
			Lane:            o.lane,
			Queue:           queue,
			ExecutionTarget: profile.ExecutionTargetSingleCluster,
			Topology: profile.Topology{
				Team:                      o.team,
				Mode:                      o.mode,
				Placement:                 o.topology,
				WorkloadPriorityClassName: o.workloadPriorityClass,
				PodPriorityClassName:      o.podPriorityClass,
				DisableDefaultPriorities:  o.disableDefaultPriorities,
			},
		},
		ClusterQueue: "tau-cq",
	}
	setAuthoritativeProfileCardinalityForTest(o, gpus, workers)
}

func setAuthoritativeProfileCardinalityForTest(o *runDispatchOptions, gpus, workers int) {
	if o.selectedWorkloadProfile == nil {
		return
	}
	o.selectedWorkloadProfile.Selection.Profile.GPUsPerWorker = int32(gpus)
	o.selectedWorkloadProfile.Selection.Profile.WorkerCount = int32(workers)
	o.selectedWorkloadProfile.Render.Resources.GPU.Count = gpus
	if o.file != "" {
		o.gpusPerWorker = gpus
		o.workers = workers
		return
	}
	if engine, err := resolveDirectRunEngine(*o); err == nil {
		switch engine {
		case directRunEngineJob:
			o.jobGPUs = &gpus
			o.nodes = workers
		case directRunEngineRayJob:
			o.gpusPerWorker = gpus
			o.workers = workers
		}
	}
}

func installClusterProfileClientForTest(t *testing.T, profiles ...profile.ResolvedWorkloadProfile) {
	t.Helper()
	client := readyClusterProfileClientForProfiles(t, 7, false, profiles...)
	original := newClusterProfileClient
	newClusterProfileClient = func(string) (dynamic.Interface, error) {
		return client, nil
	}
	t.Cleanup(func() { newClusterProfileClient = original })
}

func readyClusterProfileClientForProfiles(
	t *testing.T,
	generation int64,
	stale bool,
	profiles ...profile.ResolvedWorkloadProfile,
) dynamic.Interface {
	t.Helper()
	profiles = append([]profile.ResolvedWorkloadProfile(nil), profiles...)
	for i := range profiles {
		profiles[i].Conditions = []metav1.Condition{{
			Type:               profile.ConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             "Ready",
			Message:            "ready",
		}}
	}
	hash, err := profile.ProfileSetHash(profiles)
	if err != nil {
		t.Fatal(err)
	}
	observedGeneration := generation
	if stale {
		observedGeneration--
	}
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile.ProfileSetStatus{
		ObservedGeneration: observedGeneration,
		Observed:           int32(len(profiles)),
		Ready:              int32(len(profiles)),
		ProfileSetHash:     hash,
		Profiles:           profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	conditions, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&struct {
		Conditions []metav1.Condition `json:"conditions"`
	}{Conditions: []metav1.Condition{{
		Type:               profile.ConditionWorkloadProfilesReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "WorkloadProfilesReady",
		Message:            "ready",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tau.azure.com/v1alpha1",
		"kind":       "TauCluster",
		"metadata": map[string]any{
			"name":       profile.TauClusterName,
			"generation": generation,
		},
		"status": map[string]any{
			"workloadProfiles": status,
			"conditions":       conditions["conditions"],
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(profile.TauClusterGVR).Create(
		context.Background(),
		cluster,
		metav1.CreateOptions{},
	); err != nil {
		t.Fatal(err)
	}
	return client
}

func runConfigProfileForTest(t *testing.T, configPath string) profile.ResolvedWorkloadProfile {
	t.Helper()
	cfg, err := runconfig.Load(configPath)
	if err != nil {
		return resolvedWorkloadProfileForTest("test-profile", "jobqueue", 1, 1)
	}
	workloadConfig := cfg
	if manifestFile := strings.TrimSpace(cfg.Workflow.File); manifestFile != "" {
		if !filepath.IsAbs(manifestFile) {
			manifestFile = filepath.Join(filepath.Dir(configPath), manifestFile)
		}
		if manifestConfig, loadErr := runconfig.Load(manifestFile); loadErr == nil {
			workloadConfig.Compute = manifestConfig.Compute
			workloadConfig.Execution = manifestConfig.Execution
		}
	}
	name := strings.TrimSpace(cfg.Policy.Profile)
	if name == "" {
		name = "test-profile"
	}
	queue := strings.TrimSpace(cfg.Policy.Queue)
	if queue == "" || queue == "auto" {
		queue = "jobqueue"
	}
	gpus := 1
	if workloadConfig.Compute.GPUs != nil {
		gpus = *workloadConfig.Compute.GPUs
	} else if workloadConfig.Compute.GPUsPerWorker != nil {
		gpus = *workloadConfig.Compute.GPUsPerWorker
	}
	workers := 1
	if workloadConfig.Compute.Workers != nil {
		workers = *workloadConfig.Compute.Workers
	} else if workloadConfig.Execution.Nodes != nil {
		workers = *workloadConfig.Execution.Nodes
	}
	namespace := strings.TrimSpace(cfg.Policy.Namespace)
	if namespace == "" {
		namespace = "test-workspace"
	}
	resolved := resolvedWorkloadProfileForTest(name, queue, gpus, workers, namespace)
	resolved.Mode = firstNonEmpty(cfg.Policy.Mode, profile.ModeFixed)
	resolved.Placement = firstNonEmpty(cfg.Policy.Topology, resolved.Placement)
	resolved.Priorities.DisableDefaultPriorities = cfg.Policy.DisableDefaultPriorities != nil &&
		*cfg.Policy.DisableDefaultPriorities
	if resolved.Priorities.DisableDefaultPriorities {
		resolved.Priorities.WorkloadPriorityClassName = ""
		resolved.Priorities.PodPriorityClassName = ""
	} else {
		resolved.Priorities.WorkloadPriorityClassName = firstNonEmpty(
			cfg.Policy.WorkloadPriorityClass,
			"taugrid-default",
		)
		resolved.Priorities.PodPriorityClassName = firstNonEmpty(
			cfg.Policy.PodPriorityClass,
			"taugrid-default",
		)
	}
	return resolved
}

func resolvedWorkloadProfileForTest(
	name, queue string,
	gpus, workers int,
	namespaces ...string,
) profile.ResolvedWorkloadProfile {
	placement := profile.PlacementIndependent
	if workers > 1 {
		placement = profile.PlacementMultiNodeNCCL
	}
	if len(namespaces) == 0 {
		namespaces = []string{"default", "taugrid-default", "test-workspace"}
	}
	localQueues := make([]profile.ResolvedLocalQueue, 0, len(namespaces))
	for _, namespace := range namespaces {
		localQueues = append(localQueues, profile.ResolvedLocalQueue{
			Namespace:    namespace,
			Name:         queue,
			ClusterQueue: "tau-cq",
		})
	}
	return profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name:              name,
			GPUsPerWorker:     int32(gpus),
			WorkerCount:       int32(workers),
			Mode:              profile.ModeFixed,
			Placement:         placement,
			DefaultLocalQueue: queue,
			ExecutionTarget:   profile.ExecutionTargetSingleCluster,
			Priorities: profile.ProfilePriorities{
				WorkloadPriorityClassName: "taugrid-default",
				PodPriorityClassName:      "taugrid-default",
			},
		},
		LocalQueues:   localQueues,
		ClusterQueues: []string{"tau-cq"},
	}
}
