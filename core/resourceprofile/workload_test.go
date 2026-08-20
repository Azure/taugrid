// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizeAndValidateWorkloadProfile(t *testing.T) {
	got, err := NormalizeAndValidateWorkloadProfile(WorkloadProfile{
		Name:        " Research_GPU.1 ",
		Description: "  Single GPU  ",
		Applicability: ProfileApplicability{
			Teams:      []string{" Vision Team ", "research"},
			Lanes:      []string{" Training "},
			Namespaces: []string{" Team_A "},
		},
		GPUsPerWorker:     1,
		WorkerCount:       1,
		Mode:              " FIXED ",
		Placement:         " SINGLE_NODE_NVLINK ",
		DefaultLocalQueue: " JobQueue ",
		Priorities: ProfilePriorities{
			WorkloadPriorityClassName: " Tau_Default ",
			PodPriorityClassName:      " Tau_Default ",
		},
	})
	if err != nil {
		t.Fatalf("NormalizeAndValidateWorkloadProfile() error = %v", err)
	}
	if got.Name != "research-gpu.1" || got.Description != "Single GPU" {
		t.Fatalf("normalized identity = %#v", got)
	}
	if strings.Join(got.Applicability.Teams, ",") != "research,vision-team" {
		t.Fatalf("teams = %#v, want sorted normalized values", got.Applicability.Teams)
	}
	if strings.Join(got.Applicability.Namespaces, ",") != "team-a" {
		t.Fatalf("namespaces = %#v, want one normalized value", got.Applicability.Namespaces)
	}
	if got.Mode != ModeFixed || got.Placement != PlacementSingleNodeNVLink || got.DefaultLocalQueue != "jobqueue" {
		t.Fatalf("normalized routing = %#v", got)
	}
	if got.ExecutionTarget != ExecutionTargetSingleCluster {
		t.Fatalf("executionTarget = %q, want default %q", got.ExecutionTarget, ExecutionTargetSingleCluster)
	}
}

func TestValidateWorkloadProfileCrossFields(t *testing.T) {
	valid := testWorkloadProfile()
	tests := []struct {
		name    string
		mutate  func(*WorkloadProfile)
		wantErr string
	}{
		{"negative GPUs", func(p *WorkloadProfile) { p.GPUsPerWorker = -1 }, "gpusPerWorker"},
		{"non-positive workers", func(p *WorkloadProfile) { p.WorkerCount = 0 }, "workerCount"},
		{"unknown mode", func(p *WorkloadProfile) { p.Mode = "spot" }, "mode"},
		{"unknown placement", func(p *WorkloadProfile) { p.Placement = "same-rack" }, "placement"},
		{"unknown execution target", func(p *WorkloadProfile) { p.ExecutionTarget = "multiCluster" }, "executionTarget"},
		{"multi-worker placement", func(p *WorkloadProfile) { p.WorkerCount = 2 }, "multi-node-nccl"},
		{"duplicate applicability", func(p *WorkloadProfile) { p.Applicability.Teams = []string{"research", "research"} }, "duplicate"},
		{"missing priority", func(p *WorkloadProfile) { p.Priorities.PodPriorityClassName = "" }, "required unless"},
		{"disabled with priority", func(p *WorkloadProfile) { p.Priorities.DisableDefaultPriorities = true }, "cannot be combined"},
		{"beta without teams", func(p *WorkloadProfile) {
			p.ExecutionTarget = ExecutionTargetMultiKueueBeta
			p.Applicability.Teams = nil
		}, "applicability.teams"},
		{"beta without namespaces", func(p *WorkloadProfile) {
			p.ExecutionTarget = ExecutionTargetMultiKueueBeta
			p.Applicability.Namespaces = nil
		}, "applicability.namespaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid
			tt.mutate(&p)
			if err := ValidateWorkloadProfile(p); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateWorkloadProfile() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}

	cpu := valid
	cpu.GPUsPerWorker = 0
	if err := ValidateWorkloadProfile(cpu); err != nil {
		t.Fatalf("CPU workload profile with zero GPUs: %v", err)
	}

	disabled := valid
	disabled.Priorities = ProfilePriorities{DisableDefaultPriorities: true}
	if err := ValidateWorkloadProfile(disabled); err != nil {
		t.Fatalf("explicitly disabled priorities should validate: %v", err)
	}
}

func TestProfileSetHashIsCanonicalAndOperationallyStable(t *testing.T) {
	first := testResolvedProfile("research-1gpu")
	second := testResolvedProfile("research-2gpu")
	second.GPUsPerWorker = 2

	hashA, err := ProfileSetHash([]ResolvedWorkloadProfile{second, first})
	if err != nil {
		t.Fatalf("ProfileSetHash() error = %v", err)
	}

	first.LocalQueues = []ResolvedLocalQueue{
		{Namespace: "zeta", Name: "jobqueue", ClusterQueue: "gpu-cq"},
		{Namespace: "alpha", Name: "jobqueue", ClusterQueue: "gpu-cq"},
	}
	first.ClusterQueues = []string{"gpu-cq", "batch-cq"}
	first.Conditions = []metav1.Condition{{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 99,
		Reason:             "Ready",
		Message:            "a new operational message",
		LastTransitionTime: metav1.NewTime(time.Unix(1234, 0)),
	}}
	second.Conditions = []metav1.Condition{{Type: ConditionReady, Message: "ignored"}}
	hashB, err := ProfileSetHash([]ResolvedWorkloadProfile{first, second})
	if err != nil {
		t.Fatalf("ProfileSetHash() reordered error = %v", err)
	}
	if hashA != hashB {
		t.Fatalf("canonical hash changed with ordering/conditions: %q != %q", hashA, hashB)
	}

	first.ResourceFlavors = append(first.ResourceFlavors, "h200")
	hashC, err := ProfileSetHash([]ResolvedWorkloadProfile{first, second})
	if err != nil {
		t.Fatalf("ProfileSetHash() identity change error = %v", err)
	}
	if hashC == hashA {
		t.Fatal("hash did not change when a resolved identity changed")
	}

	single := testResolvedProfile("execution-target")
	beta := testResolvedProfile("execution-target")
	beta.ExecutionTarget = ExecutionTargetMultiKueueBeta
	singleHash, err := ProfileSetHash([]ResolvedWorkloadProfile{single})
	if err != nil {
		t.Fatalf("single-cluster ProfileSetHash() error = %v", err)
	}
	betaHash, err := ProfileSetHash([]ResolvedWorkloadProfile{beta})
	if err != nil {
		t.Fatalf("MultiKueue Beta ProfileSetHash() error = %v", err)
	}
	if singleHash == betaHash {
		t.Fatal("hash did not change with executionTarget")
	}
}

func TestProfileSetSnapshotRoundTripAndVersionRejection(t *testing.T) {
	resolved := testResolvedProfile("research-1gpu")
	resolved.ExecutionTarget = ExecutionTargetMultiKueueBeta
	resolved.Conditions = []metav1.Condition{{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 7,
		Reason:             "Ready",
		Message:            "all referenced resources are ready",
	}}
	snapshot, err := NewProfileSetSnapshot(7, []ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatalf("NewProfileSetSnapshot() error = %v", err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	got, err := DecodeProfileSetSnapshot(data)
	if err != nil {
		t.Fatalf("DecodeProfileSetSnapshot() error = %v", err)
	}
	if got.APIVersion != ProfileSnapshotAPIVersion || got.Kind != ProfileSnapshotKind || got.ProfileSetHash != snapshot.ProfileSetHash {
		t.Fatalf("snapshot round trip = %#v", got)
	}
	if got.Profiles[0].ExecutionTarget != ExecutionTargetMultiKueueBeta {
		t.Fatalf("snapshot executionTarget = %q", got.Profiles[0].ExecutionTarget)
	}

	jsonData, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if _, err := DecodeProfileSetSnapshot(jsonData); err != nil {
		t.Fatalf("JSON snapshot should decode: %v", err)
	}

	snapshot.APIVersion = "example.invalid/v2"
	data, err = yaml.Marshal(snapshot)
	if err != nil {
		t.Fatalf("yaml.Marshal(version) error = %v", err)
	}
	if _, err := DecodeProfileSetSnapshot(data); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("version rejection error = %v", err)
	}
}

func TestResolvedWorkloadProfileRenderProfile(t *testing.T) {
	resolved := testResolvedProfile("research-1gpu")
	render, err := resolved.RenderProfile("alpha", "research", "training")
	if err != nil {
		t.Fatalf("RenderProfile() error = %v", err)
	}
	if render.Name != resolved.Name || render.Queue != "jobqueue" || render.Topology.Mode != ModeFixed ||
		render.Topology.Placement != PlacementIndependent || render.Resources.GPU.Count != 1 ||
		render.ExecutionTarget != ExecutionTargetSingleCluster {
		t.Fatalf("render profile = %#v", render)
	}
	if render.Topology.WorkloadPriorityClassName != "tau-default" || render.Topology.PodPriorityClassName != "tau-default" {
		t.Fatalf("render priorities = %#v", render.Topology)
	}
	clusterQueue, err := resolved.ClusterQueueFor("alpha", render.Queue)
	if err != nil || clusterQueue != "gpu-cq" {
		t.Fatalf("ClusterQueueFor() = %q, %v", clusterQueue, err)
	}
	resolved.ClusterQueues = append(resolved.ClusterQueues, "other-cq")
	if _, err := resolved.ClusterQueueFor("other", render.Queue); err == nil {
		t.Fatal("ClusterQueueFor() accepted an unobserved namespace binding")
	}
}

func testWorkloadProfile() WorkloadProfile {
	return WorkloadProfile{
		Name: "research-1gpu",
		Applicability: ProfileApplicability{
			Teams: []string{"research"}, Lanes: []string{"training"}, Namespaces: []string{"alpha"},
		},
		GPUsPerWorker:     1,
		WorkerCount:       1,
		Mode:              ModeFixed,
		Placement:         PlacementIndependent,
		DefaultLocalQueue: "jobqueue",
		ExecutionTarget:   ExecutionTargetSingleCluster,
		Priorities: ProfilePriorities{
			WorkloadPriorityClassName: "tau-default",
			PodPriorityClassName:      "tau-default",
		},
	}
}

func testResolvedProfile(name string) ResolvedWorkloadProfile {
	intent := testWorkloadProfile()
	intent.Name = name
	return ResolvedWorkloadProfile{
		WorkloadProfile: intent,
		LocalQueues: []ResolvedLocalQueue{
			{Namespace: "alpha", Name: "jobqueue", ClusterQueue: "gpu-cq"},
			{Namespace: "zeta", Name: "jobqueue", ClusterQueue: "gpu-cq"},
		},
		ClusterQueues:           []string{"batch-cq", "gpu-cq"},
		ResourceFlavors:         []string{"a100"},
		Topologies:              []string{"default"},
		WorkloadPriorityClasses: []string{"tau-default"},
		PodPriorityClasses:      []string{"tau-default"},
	}
}
