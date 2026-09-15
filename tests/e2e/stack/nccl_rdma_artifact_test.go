// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
)

func TestNCCLRDMAArtifactRecorderWritesUnknownFailureArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rdma-validation", "validation.json")
	t.Setenv("NCCL_RDMA_RESULT_PATH", path)
	t.Setenv("NCCL_RDMA_RUN_ID", "nccl-rdma-0123456789abcdef0123456789abcdef")
	t.Setenv("NCCL_RDMA_RUN_ATTEMPT", "1")
	t.Setenv("NCCL_RDMA_WORKSPACE_ID", "taugrid-rdma")
	t.Setenv("NCCL_RDMA_CLUSTER", "h200-validation")
	t.Setenv("NCCL_RDMA_EXPECTED_SITE", "westus3")
	t.Setenv("NCCL_RDMA_EXPECTED_REGION", "westus3")
	t.Setenv("NCCL_RDMA_EXPECTED_POOL", "h200pool")
	t.Setenv("NCCL_RDMA_EXPECTED_GPU_MODEL", "NVIDIA H200")
	t.Setenv("NCCL_RDMA_SOURCE_REVISION", strings.Repeat("1", 40))
	recorder := newNCCLRDMAArtifactRecorder(
		t,
		"nccl-rdma-0123456789abcdef0123456789abcdef",
		time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC),
	)
	require.NoError(t, recorder.requireInputs())
	recorder.result.Cleanup = rdmavalidation.Cleanup{
		State:       rdmavalidation.CleanupComplete,
		StartedAt:   time.Date(2026, time.September, 14, 20, 0, 1, 0, time.UTC),
		CompletedAt: time.Date(2026, time.September, 14, 20, 0, 2, 0, time.UTC),
	}

	require.NoError(t, recorder.write(recorder.result))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var result rdmavalidation.Result
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, rdmavalidation.StatusUnknown, result.Status)
	require.Equal(t, rdmavalidation.ReasonEvidenceIntegrityMissing, result.Reason)
	require.Equal(t, rdmavalidation.CleanupComplete, result.Cleanup.State)
	require.Equal(t, recorder.result.ObservedAt, result.ObservedAt)
	require.Equal(t, result.ObservedAt.Add(24*time.Hour), result.ValidUntil)
	require.NoError(t, rdmavalidation.Validate(result))
}

func TestNCCLRDMAArtifactRecorderRejectsInvalidTelemetryIDBeforeCreate(t *testing.T) {
	t.Setenv("NCCL_RDMA_RESULT_PATH", filepath.Join(t.TempDir(), "result.json"))
	t.Setenv("NCCL_RDMA_RUN_ID", "nccl-rdma-0123456789abcdef0123456789abcdef")
	t.Setenv("NCCL_RDMA_RUN_ATTEMPT", "1")
	t.Setenv("NCCL_RDMA_WORKSPACE_ID", "Invalid/Workspace")
	t.Setenv("NCCL_RDMA_CLUSTER", "h200-validation")
	t.Setenv("NCCL_RDMA_EXPECTED_SITE", "westus3")
	t.Setenv("NCCL_RDMA_EXPECTED_REGION", "westus3")
	t.Setenv("NCCL_RDMA_EXPECTED_POOL", "h200pool")
	t.Setenv("NCCL_RDMA_EXPECTED_GPU_MODEL", "NVIDIA H200")
	t.Setenv("NCCL_RDMA_SOURCE_REVISION", strings.Repeat("1", 40))
	recorder := newNCCLRDMAArtifactRecorder(
		t,
		"nccl-rdma-0123456789abcdef0123456789abcdef",
		time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC),
	)
	require.ErrorContains(t, recorder.requireInputs(), "workspace_id")
}

func TestNCCLRDMAArtifactRecorderPersistsContractPlacementFailure(t *testing.T) {
	raw, err := e2e.ReadRepoFile("core/rdmavalidation/testdata/pass.golden.json")
	require.NoError(t, err)
	var result rdmavalidation.Result
	require.NoError(t, json.Unmarshal(raw, &result))
	mismatch := false
	result.Placement.MatchesRequest = &mismatch

	path := filepath.Join(t.TempDir(), "placement-failure.json")
	recorder := &ncclRDMAArtifactRecorder{t: t, path: path}
	require.NoError(t, recorder.write(result))

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var stored rdmavalidation.Result
	require.NoError(t, json.Unmarshal(written, &stored))
	require.Equal(t, rdmavalidation.StatusFail, stored.Status)
	require.Equal(t, rdmavalidation.ReasonPlacementMismatch, stored.Reason)
	require.NotEmpty(t, stored.Measurements.Samples)
	require.NotEmpty(t, stored.Evidence)
}
