// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcapture

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRunDataMapsRunProfileToStoreRecords(t *testing.T) {
	created := mustTime("2026-04-27T10:00:00Z")
	started := mustTime("2026-04-27T10:02:00Z")
	finished := mustTime("2026-04-27T10:32:00Z")
	record, err := RunData(status.Snapshot{
		Name:          "train-001",
		Namespace:     "ray",
		Labels:        map[string]string{workloadmeta.LabelRunID: "train-001", workloadmeta.LabelWorkloadKind: "job", workloadmeta.LabelProfile: "research-train-gpu", "kueue.x-k8s.io/queue-name": "training-queue", workloadmeta.LabelTeam: "research", workloadmeta.LabelLane: "training", workloadmeta.LabelGPUClass: "a100-80gb"},
		Annotations:   map[string]string{workloadmeta.AnnotationCaptureVersion: "v1alpha1", workloadmeta.AnnotationNamespace: "ray", workloadmeta.AnnotationTauCommand: "tau submit train-001", workloadmeta.AnnotationImageDigest: "sha256:abc", workloadmeta.AnnotationCodeSHA: "abc123", workloadmeta.AnnotationConfigHash: "config123", workloadmeta.AnnotationGPUCount: "8", workloadmeta.AnnotationStorageMounts: `[{"source":"pvc","path":"/data"}]`, workloadmeta.AnnotationResultPath: "/data/runs/train-001"},
		JobFound:      true,
		JobCreatedAt:  created,
		JobStartedAt:  started,
		JobFinishedAt: finished,
		JobConditions: []status.Condition{{Type: "Complete", Status: "True"}},
		Workloads:     []status.Workload{{Name: "train-001-workload", Queue: "training-queue", Phase: "Finished", Admitted: true}},
		Pods:          []status.Pod{{Name: "p", UID: "pod-uid", Node: "node-a", Phase: "Succeeded", StartedAt: started.Add(30 * time.Second), ResourceClaims: []string{"claim-a"}}},
	}, status.CostProfile{
		Profile:    "research-train-gpu",
		GPUType:    "a100",
		GPUsPerPod: 8,
		Pods:       1,
		Hours:      0.5,
		TotalUsd:   3.49,
	}, status.ExperimentRunDataOptions{Project: "project-alpha", RunGroupID: "reference-group", Cluster: "kind-taugrid"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Run.RunID != "train-001" || record.Run.State != "succeeded" || record.Run.TauCommand != "tau submit train-001" {
		t.Fatalf("unexpected run record: %+v", record.Run)
	}
	if record.Run.Project != "project-alpha" || record.Run.RunGroupID != "reference-group" {
		t.Fatalf("run identity not preserved: %+v", record.Run)
	}
	if record.RunContext == nil {
		t.Fatal("run context missing")
	}
	if record.RunContext.Cluster != "kind-taugrid" || record.RunContext.LocalQueue != "training-queue" || record.RunContext.KueueWorkload != "train-001-workload" {
		t.Fatalf("unexpected run context: %+v", record.RunContext)
	}
	if record.RunContext.GPUCount == nil || *record.RunContext.GPUCount != 8 {
		t.Fatalf("gpu count not captured: %+v", record.RunContext.GPUCount)
	}
	if record.RunContext.QueueWaitSeconds == nil || *record.RunContext.QueueWaitSeconds != 120 {
		t.Fatalf("queue wait not captured: %+v", record.RunContext.QueueWaitSeconds)
	}
	if record.RunContext.GPUHours == nil || *record.RunContext.GPUHours != 4 {
		t.Fatalf("gpu hours not captured: %+v", record.RunContext.GPUHours)
	}
	if len(record.Tags) != 1 || record.Tags[0].Key != "tau.capture.source" {
		t.Fatalf("capture source tag missing: %+v", record.Tags)
	}
}

func TestRunDataRejectsMissingJob(t *testing.T) {
	_, err := RunData(status.Snapshot{Name: "ghost", Namespace: "tau"}, status.CostProfile{}, status.ExperimentRunDataOptions{})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected missing job error, got %v", err)
	}
}

func TestRunDataAcceptsRayJob(t *testing.T) {
	created := mustTime("2026-04-27T10:00:00Z")
	started := mustTime("2026-04-27T10:02:00Z")
	finished := mustTime("2026-04-27T10:32:00Z")
	record, err := RunData(status.Snapshot{
		Name:             "ray-train-001",
		Namespace:        "ray",
		Labels:           map[string]string{workloadmeta.LabelRunID: "ray-train-001"},
		RayJobFound:      true,
		RayJobStatus:     "Complete",
		RayJobCreatedAt:  created,
		RayJobStartedAt:  started,
		RayJobFinishedAt: finished,
	}, status.CostProfile{}, status.ExperimentRunDataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if record.Run.RunID != "ray-train-001" {
		t.Fatalf("unexpected run id: %s", record.Run.RunID)
	}
	if record.Run.State != "succeeded" {
		t.Fatalf("expected succeeded, got: %s", record.Run.State)
	}
	if record.Run.CompletedAt != "2026-04-27T10:32:00Z" {
		t.Fatalf("expected CompletedAt from RayJob endTime, got: %s", record.Run.CompletedAt)
	}
}
