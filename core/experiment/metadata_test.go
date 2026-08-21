// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package experiment

import (
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
	"testing"
)

func TestMetadataKubernetesMetadata(t *testing.T) {
	labels, annotations := Metadata{
		RunID:            "train-001",
		Namespace:        "ray",
		WorkspaceID:      "sample",
		ResultScope:      "/data/projects/sample/runs",
		WorkloadKind:     WorkloadKindJob,
		TauCommand:       "tau submit train-001 --env WANDB_KEY=<redacted>",
		Image:            "acr.io/train@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CodeSHA:          "abc123",
		ConfigHash:       HashBytes([]byte("config")),
		GPUCount:         1,
		DRAClaimTemplate: "full-gpu",
		StorageMounts: []StorageMount{
			{Source: "pvc", SourceRef: "training-nfs", Path: "/data"},
		},
	}.KubernetesMetadata()

	if labels[LabelRunID] != "train-001" {
		t.Fatalf("run label: %+v", labels)
	}
	if labels[LabelWorkloadKind] != WorkloadKindJob {
		t.Fatalf("workload kind label: %+v", labels)
	}
	for _, key := range []string{
		AnnotationCaptureVersion,
		AnnotationNamespace,
		AnnotationWorkspaceID,
		AnnotationResultScope,
		AnnotationTauCommand,
		AnnotationImage,
		AnnotationImageDigest,
		AnnotationCodeSHA,
		AnnotationConfigHash,
		AnnotationGPUCount,
		AnnotationDRAClaim,
		AnnotationStorageMounts,
	} {
		if annotations[key] == "" {
			t.Fatalf("missing annotation %s: %+v", key, annotations)
		}
	}
	if annotations[AnnotationCaptureVersion] != captureVersion {
		t.Fatalf("capture version: %+v", annotations)
	}
	if annotations[AnnotationImageDigest] != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("digest: %+v", annotations)
	}
	if annotations[AnnotationWorkspaceID] != "sample" || annotations[AnnotationResultScope] != "/data/projects/sample/runs" {
		t.Fatalf("workspace lifecycle annotations: %+v", annotations)
	}
}

func TestRedactCommandArgs(t *testing.T) {
	got := RedactCommandArgs([]string{
		"tau", "submit", "train",
		"--env", "WANDB_API_KEY=super-secret",
		"--token=ghp_123",
		"--model", "/data/model",
	})
	if strings.Contains(got, "super-secret") || strings.Contains(got, "ghp_123") {
		t.Fatalf("command leaked sensitive values: %s", got)
	}
	for _, want := range []string{"WANDB_API_KEY=<redacted>", "--token=<redacted>", "--model /data/model"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command missing %q: %s", want, got)
		}
	}
}

func TestHashBytesStable(t *testing.T) {
	got := HashBytes([]byte("same-input"))
	if got != HashBytes([]byte("same-input")) {
		t.Fatal("hash was not deterministic")
	}
	if len(got) != 64 {
		t.Fatalf("hash length=%d, want 64", len(got))
	}
}

func TestOversizedAnnotationsAreOmitted(t *testing.T) {
	_, annotations := Metadata{
		RunID:        "train-001",
		WorkloadKind: WorkloadKindJob,
		TauCommand:   strings.Repeat("x", maxAnnotationValueBytes+1),
	}.KubernetesMetadata()
	if annotations[AnnotationTauCommand] != "" {
		t.Fatalf("oversized command should be omitted: %+v", annotations)
	}
	if annotations[AnnotationCaptureVersion] == "" {
		t.Fatalf("independent annotations should still be set: %+v", annotations)
	}
}

func TestInvalidLabelValuesAreOmitted(t *testing.T) {
	labels, _ := Metadata{RunID: "Train_001", WorkloadKind: WorkloadKindJob}.KubernetesMetadata()
	if labels[LabelRunID] != "" {
		t.Fatalf("invalid label should be omitted: %+v", labels)
	}
	if labels[LabelWorkloadKind] != WorkloadKindJob {
		t.Fatalf("valid label should remain: %+v", labels)
	}
}

func TestStellarMetadataUsesNormalizedLabelsAndExactAnnotations(t *testing.T) {
	labels, annotations := Metadata{
		RunID:        "train-001",
		WorkloadKind: WorkloadKindJob,
		Stellar: StellarMetadata{
			Workspace:    "sample",
			Project:      "NanoGPT FineWeb",
			ExperimentID: "nanogpt-api-surface",
			RunGroupID:   "Safe Stack/H200",
			Tags: map[string]string{
				"dataset": "fineweb",
				"recipe":  "api-surface",
			},
		},
	}.KubernetesMetadata()

	if labels[labelStellarProject] != "nanogpt-fineweb" {
		t.Fatalf("project label not normalized: %+v", labels)
	}
	if labels[labelStellarExperiment] != "nanogpt-api-surface" {
		t.Fatalf("experiment label not preserved: %+v", labels)
	}
	if labels[labelStellarGroup] != "safe-stack-h200" {
		t.Fatalf("group label not normalized: %+v", labels)
	}
	if labels[workloadmeta.LabelWorkspace] != "sample" {
		t.Fatalf("workspace label missing: %+v", labels)
	}
	if labels[workloadmeta.LabelWorkspace] != "sample" {
		t.Fatalf("legacy workspace label missing: %+v", labels)
	}
	if annotations[AnnotationStellarProject] != "NanoGPT FineWeb" ||
		annotations[AnnotationStellarExperimentID] != "nanogpt-api-surface" ||
		annotations[AnnotationStellarGroup] != "Safe Stack/H200" {
		t.Fatalf("annotations should preserve user-facing Stellar values: %+v", annotations)
	}
	if _, ok := annotations[workloadmeta.AnnotationStellarExperimentTitle]; ok {
		t.Fatalf("new workloads must not emit the retired experiment title annotation: %+v", annotations)
	}
	if annotations[AnnotationStellarTags] != `{"dataset":"fineweb","recipe":"api-surface","tau_workspace":"sample"}` {
		t.Fatalf("tags annotation not encoded deterministically: %+v", annotations)
	}
}

func TestStellarMetadataPreservesWorkspaceWhenTagsAreOversized(t *testing.T) {
	_, annotations := Metadata{
		Stellar: StellarMetadata{
			Workspace: "sample",
			Tags: map[string]string{
				"oversized": strings.Repeat("x", maxAnnotationValueBytes),
			},
		},
	}.KubernetesMetadata()

	if annotations[AnnotationStellarTags] != `{"tau_workspace":"sample"}` {
		t.Fatalf("oversized tags should retain only the trusted workspace: %+v", annotations)
	}
}

func TestStellarMetadataOmitsOversizedTagsWithoutWorkspace(t *testing.T) {
	_, annotations := Metadata{
		Stellar: StellarMetadata{
			Tags: map[string]string{
				"oversized": strings.Repeat("x", maxAnnotationValueBytes),
			},
		},
	}.KubernetesMetadata()

	if annotations[AnnotationStellarTags] != "" {
		t.Fatalf("oversized unscoped tags should remain omitted: %+v", annotations)
	}
}

func TestEncodeStorageMountsDeterministic(t *testing.T) {
	got := encodeStorageMounts([]StorageMount{
		{Source: "pvc", SourceRef: "b", Path: "/mnt/b"},
		{Source: "pvc", SourceRef: "a", Path: "/data"},
	})
	wantFirst := `"path":"/data"`
	if !strings.Contains(got, wantFirst) || strings.Index(got, wantFirst) > strings.Index(got, `"path":"/mnt/b"`) {
		t.Fatalf("mounts not sorted deterministically: %s", got)
	}
}
