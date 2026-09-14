// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	e2e "github.com/Azure/taugrid/tests/e2e"
)

const (
	ncclRDMAResultPathEnv = "NCCL_RDMA_RESULT_PATH"
	ncclRDMAStaleSeconds  = rdmavalidation.DefaultStaleAfterSeconds
)

type ncclRDMAArtifactRecorder struct {
	t           *testing.T
	path        string
	result      rdmavalidation.Result
	written     bool
	cleanupDone bool
}

func newNCCLRDMAArtifactRecorder(t *testing.T, invocation string, createdAt time.Time) *ncclRDMAArtifactRecorder {
	t.Helper()
	attempt, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("NCCL_RDMA_RUN_ATTEMPT")))
	input := e2e.NCCLRDMAValidationInput{
		ValidationID:     invocation,
		RunID:            strings.TrimSpace(os.Getenv("NCCL_RDMA_RUN_ID")),
		Attempt:          attempt,
		WorkspaceID:      strings.TrimSpace(os.Getenv("NCCL_RDMA_WORKSPACE_ID")),
		Cluster:          strings.TrimSpace(os.Getenv("NCCL_RDMA_CLUSTER")),
		Namespace:        stackNamespace,
		ProjectID:        strings.TrimSpace(os.Getenv("NCCL_RDMA_PROJECT_ID")),
		ExperimentID:     strings.TrimSpace(os.Getenv("NCCL_RDMA_EXPERIMENT_ID")),
		RunGroupID:       strings.TrimSpace(os.Getenv("NCCL_RDMA_RUN_GROUP_ID")),
		ExpectedSite:     strings.TrimSpace(os.Getenv("NCCL_RDMA_EXPECTED_SITE")),
		ExpectedPool:     strings.TrimSpace(os.Getenv("NCCL_RDMA_EXPECTED_POOL")),
		ExpectedGPUModel: strings.TrimSpace(os.Getenv("NCCL_RDMA_EXPECTED_GPU_MODEL")),
		SourceRevision:   strings.TrimSpace(os.Getenv("NCCL_RDMA_SOURCE_REVISION")),
		CreatedAt:        createdAt.UTC(), ObservedAt: createdAt.UTC(),
		StaleAfter: time.Duration(ncclRDMAStaleSeconds) * time.Second,
		Cleanup:    rdmavalidation.Cleanup{State: rdmavalidation.CleanupUnknown},
	}
	path := strings.TrimSpace(os.Getenv(ncclRDMAResultPathEnv))
	if path == "" {
		path = rdmavalidation.DefaultArtifactPath(".", invocation)
	}
	return &ncclRDMAArtifactRecorder{
		t: t, path: path, result: e2e.NewNCCLRDMAValidationSkeleton(input),
	}
}

func (recorder *ncclRDMAArtifactRecorder) fallbackWrite() {
	recorder.t.Helper()
	if recorder.written {
		return
	}
	if err := recorder.write(recorder.result); err != nil {
		recorder.t.Errorf("write fail-closed NCCL/RDMA validation artifact: %v", err)
	}
}

func (recorder *ncclRDMAArtifactRecorder) write(result rdmavalidation.Result) error {
	if !result.Cleanup.CompletedAt.IsZero() &&
		(result.ObservedAt.IsZero() || result.ObservedAt.Before(result.Cleanup.CompletedAt)) {
		result.ObservedAt = result.Cleanup.CompletedAt
	} else if result.ObservedAt.IsZero() {
		result.ObservedAt = time.Now().UTC()
	}
	result.StaleAfterSeconds = ncclRDMAStaleSeconds
	result.ValidUntil = result.ObservedAt.Add(time.Duration(ncclRDMAStaleSeconds) * time.Second)
	if err := rdmavalidation.Finalize(&result); err != nil {
		return err
	}
	if _, err := rdmavalidation.WriteArtifact(recorder.path, result); err != nil {
		return err
	}
	recorder.result = result
	recorder.written = true
	recorder.t.Logf("NCCL/RDMA validation artifact: %s", recorder.path)
	return nil
}

func (recorder *ncclRDMAArtifactRecorder) cleanup(
	tc *e2e.TestContext,
	owned []ownedNCCLRDMAResource,
	invocation string,
	ambiguousCreate bool,
) error {
	if recorder.cleanupDone {
		return nil
	}
	recorder.result.Cleanup.StartedAt = time.Now().UTC()
	recorder.result.Cleanup.OwnedResources = ownedResourceRefs(owned)
	err := deleteOwnedNCCLRDMAResourcesAndWait(tc, owned, invocation)
	recorder.result.Cleanup.CompletedAt = time.Now().UTC()
	recorder.result.ObservedAt = recorder.result.Cleanup.CompletedAt
	recorder.result.ValidUntil = recorder.result.ObservedAt.Add(
		time.Duration(ncclRDMAStaleSeconds) * time.Second,
	)
	recorder.cleanupDone = true
	if ambiguousCreate {
		recorder.result.Cleanup.State = rdmavalidation.CleanupUnknown
		recorder.addError(
			rdmavalidation.ReasonMissingRequiredEvidence,
			"cleanup.state",
			"create outcome was ambiguous; only successful-response UIDs were cleaned and possible leaked resources require manual inspection",
		)
		return err
	}

	if err != nil {
		recorder.result.Cleanup.State = rdmavalidation.CleanupIncomplete
		recorder.addError(
			rdmavalidation.ReasonCleanupIncomplete,
			"cleanup.state",
			"UID-owned resource cleanup did not complete before the bounded deadline",
		)
		return err
	}
	recorder.result.Cleanup.State = rdmavalidation.CleanupComplete
	recorder.result.Cleanup.RemainingResources = []rdmavalidation.ResourceRef{}
	return nil
}

func ownedResourceRefs(owned []ownedNCCLRDMAResource) []rdmavalidation.ResourceRef {
	refs := make([]rdmavalidation.ResourceRef, 0, len(owned))
	for _, item := range owned {
		apiVersion := item.GVR.Version
		if item.GVR.Group != "" {
			apiVersion = item.GVR.Group + "/" + item.GVR.Version
		}
		kind := map[string]string{
			"configmaps": "ConfigMap", "jobs": "Job", "networkpolicies": "NetworkPolicy",
			"secrets": "Secret", "services": "Service", "serviceaccounts": "ServiceAccount",
		}[item.GVR.Resource]
		refs = append(refs, rdmavalidation.ResourceRef{
			APIVersion: apiVersion, Kind: kind, Namespace: stackNamespace,
			Name: item.Name, UID: string(item.UID),
		})
	}
	return refs
}

func (recorder *ncclRDMAArtifactRecorder) addError(code rdmavalidation.ReasonCode, field, message string) {
	recorder.result.Errors = append(recorder.result.Errors, rdmavalidation.ValidationError{
		Code: code, Field: field, Message: message,
	})
}

func (recorder *ncclRDMAArtifactRecorder) requireInputs() error {
	required := map[string]string{
		"NCCL_RDMA_RUN_ID":             recorder.result.RunID,
		"NCCL_RDMA_RUN_ATTEMPT":        strconv.Itoa(recorder.result.Attempt),
		"NCCL_RDMA_WORKSPACE_ID":       recorder.result.WorkspaceID,
		"NCCL_RDMA_CLUSTER":            recorder.result.Cluster,
		"NCCL_RDMA_EXPECTED_SITE":      recorder.result.Requested.Topology.Site,
		"NCCL_RDMA_EXPECTED_POOL":      recorder.result.Requested.Topology.Pool,
		"NCCL_RDMA_EXPECTED_GPU_MODEL": recorder.result.Requested.Topology.GPUModel,
		"NCCL_RDMA_SOURCE_REVISION":    recorder.result.Source.Revision,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" || name == "NCCL_RDMA_RUN_ATTEMPT" && recorder.result.Attempt < 1 {
			return fmt.Errorf("%s is required for the machine-readable validation result", name)
		}
	}
	probe := recorder.result
	if err := rdmavalidation.Finalize(&probe); err != nil {
		return fmt.Errorf("validate machine-readable result inputs: %w", err)
	}
	if _, err := os.Stat(recorder.path); err == nil {
		return fmt.Errorf("result artifact %s already exists; refusing to replace immutable history", recorder.path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check result artifact %s: %w", recorder.path, err)
	}
	return nil
}
