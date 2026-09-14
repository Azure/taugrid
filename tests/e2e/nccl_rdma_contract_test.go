// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildNCCLRDMAValidationResultConvertsParserEvidence(t *testing.T) {
	parsed, err := ParseNCCLRDMAOutput(validNCCLRDMAOutput)
	require.NoError(t, err)
	input := validNCCLRDMAValidationInput(parsed)
	result, err := BuildNCCLRDMAValidationResult(input)
	require.NoError(t, err)
	require.Equal(t, rdmavalidation.StatusPass, result.Status)
	require.Equal(t, rdmavalidation.ReasonValidationPassed, result.Reason)
	require.Equal(t, "westus3", result.Actual.Site)
	require.Equal(t, "h200pool", result.Actual.Pool)
	require.Equal(t, "GPU-aaaaaaaa", result.Actual.Nodes[0].GPUUUID)
	require.Equal(t, "eth2", result.Actual.Nodes[1].RDMAInterface)
	require.Equal(t, 2, result.Measurements.AlgBWGbps.Count)
	require.Equal(t, 12.5, *result.Measurements.AlgBWGbps.Min)
	require.Len(t, result.Evidence, 5)
	for _, evidence := range result.Evidence {
		require.Regexp(t, `^urn:sha256:[0-9a-f]{64}$`, evidence.URI)
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, evidence.SHA256)
		require.Positive(t, evidence.SizeBytes)
	}
}

func TestBuildNCCLRDMAValidationResultFailsClosedOnPlacementMismatch(t *testing.T) {
	parsed, err := ParseNCCLRDMAOutput(validNCCLRDMAOutput)
	require.NoError(t, err)
	input := validNCCLRDMAValidationInput(parsed)
	input.Nodes["h200-b"].Labels["kubernetes.azure.com/agentpool"] = "otherpool"
	result, err := BuildNCCLRDMAValidationResult(input)
	require.NoError(t, err)
	require.Equal(t, rdmavalidation.StatusFail, result.Status)
	require.Equal(t, rdmavalidation.ReasonPlacementMismatch, result.Reason)
}

func TestBuildNCCLRDMAValidationResultRecordsNonzeroRankExit(t *testing.T) {
	parsed, err := ParseNCCLRDMAOutput(validNCCLRDMAOutput)
	require.NoError(t, err)
	input := validNCCLRDMAValidationInput(parsed)
	input.Pods[1].Status.ContainerStatuses[0].State.Terminated.ExitCode = 17

	result, err := BuildNCCLRDMAValidationResult(input)
	require.NoError(t, err)
	require.Equal(t, 17, *result.JobExitCode)
	require.Equal(t, rdmavalidation.StatusFail, result.Status)
	require.Equal(t, rdmavalidation.ReasonNonzeroExit, result.Reason)
}

func validNCCLRDMAValidationInput(parsed NCCLRDMAResult) NCCLRDMAValidationInput {
	created := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	admitted := created.Add(time.Second)
	started := admitted.Add(time.Second)
	completed := admitted.Add(10 * time.Second)
	cleanupStarted := completed.Add(time.Second)
	cleanupCompleted := cleanupStarted.Add(time.Second)
	pods := []corev1.Pod{
		rdmaContractPod("probe-0", "pod-uid-0", "h200-a", "0"),
		rdmaContractPod("probe-1", "pod-uid-1", "h200-b", "1"),
	}
	nodes := map[string]*corev1.Node{
		"h200-a": rdmaContractNode("h200-a", "node-uid-a"),
		"h200-b": rdmaContractNode("h200-b", "node-uid-b"),
	}
	return NCCLRDMAValidationInput{
		ValidationID: "nccl-rdma-0123456789abcdef0123456789abcdef",
		RunID:        "nccl-rdma-0123456789abcdef0123456789abcdef", Attempt: 1,
		WorkspaceID: "taugrid-rdma", Cluster: "h200-validation", Namespace: "taugrid-rdma-diagnostic",
		ProjectID: "taugrid", ExperimentID: "rdma-validation", RunGroupID: "manual",
		ExpectedSite: "westus3", ExpectedPool: "h200pool", ExpectedGPUModel: "NVIDIA H200",
		SourceRevision: strings.Repeat("1", 40),
		CreatedAt:      created, StartedAt: started, AdmittedAt: admitted, CompletedAt: completed,
		ObservedAt: cleanupCompleted, StaleAfter: 24 * time.Hour,
		Parsed: parsed, Pods: pods, Nodes: nodes, SanitizedManifest: []byte(`{"apiVersion":"batch/v1","kind":"Job"}`),
		Cleanup: rdmavalidation.Cleanup{
			State: rdmavalidation.CleanupComplete, StartedAt: cleanupStarted,
			CompletedAt: cleanupCompleted,
			OwnedResources: []rdmavalidation.ResourceRef{
				{APIVersion: "v1", Kind: "ConfigMap", Namespace: "taugrid-rdma-diagnostic", Name: "probe", UID: "configmap-uid"},
				{APIVersion: "batch/v1", Kind: "Job", Namespace: "taugrid-rdma-diagnostic", Name: "job", UID: "job-uid"},
				{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: "taugrid-rdma-diagnostic", Name: "network", UID: "network-uid"},
				{APIVersion: "v1", Kind: "Secret", Namespace: "taugrid-rdma-diagnostic", Name: "auth", UID: "secret-uid"},
				{APIVersion: "v1", Kind: "Service", Namespace: "taugrid-rdma-diagnostic", Name: "rank0", UID: "service-uid"},
				{APIVersion: "v1", Kind: "ServiceAccount", Namespace: "taugrid-rdma-diagnostic", Name: "runner", UID: "serviceaccount-uid"},
			},
			RemainingResources: []rdmavalidation.ResourceRef{},
		},
	}
}

func rdmaContractPod(name, uid, node, rank string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(uid),
			Labels: map[string]string{"batch.kubernetes.io/job-completion-index": rank},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "probe", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
			},
		}}},
	}
}

func rdmaContractNode(name, uid string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID(uid),
		Labels: map[string]string{
			"topology.kubernetes.io/region":  "westus3",
			"kubernetes.azure.com/agentpool": "h200pool",
		},
	}}
}
