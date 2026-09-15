// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	e2e "github.com/Azure/taugrid/tests/e2e"
)

const (
	ncclRDMAJobName       = "e2e-nccl-rdma-2x1xh200"
	ncclRDMAConfirmation  = "create-fixed-nccl-rdma-indexed-job"
	ncclRDMAInvocationKey = "e2e.taugrid.azure.com/invocation"
	ncclRDMAOwnedUIDFile  = "NCCL_RDMA_OWNED_UID_FILE"
)

var ncclRDMAJobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

// TestNCCLRDMA2x1H200 is an operator-owned 2-GPU inter-node RDMA validation,
// not a full-node bandwidth benchmark or standard Tau profile.
func TestNCCLRDMA2x1H200(t *testing.T) {
	if os.Getenv("AI_RUNTIME_E2E") != "1" {
		t.Skip("set AI_RUNTIME_E2E=1 to run live e2e tests")
	}
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run GPU stack tests")
	}
	if os.Getenv("E2E_NCCL_RDMA") != "1" {
		t.Skip("set E2E_NCCL_RDMA=1 to run the NCCL/RDMA Indexed Job diagnostic")
	}
	require.Equal(t, ncclRDMAConfirmation, os.Getenv("NCCL_RDMA_CONFIRM"),
		"NCCL/RDMA diagnostic requires the explicit harness confirmation token")
	invocation := strings.TrimSpace(os.Getenv("NCCL_RDMA_INVOCATION"))
	require.Regexp(t, `^nccl-rdma-[a-f0-9]{32}$`, invocation,
		"NCCL/RDMA diagnostic requires the unique harness invocation marker")
	require.True(t, stackUsesArgoCDQueue(), "NCCL/RDMA diagnostic requires a pre-existing namespace and LocalQueue")
	recorder := newNCCLRDMAArtifactRecorder(t, invocation, time.Now().UTC())
	t.Cleanup(recorder.fallbackWrite)
	require.NoError(t, recorder.requireInputs())

	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	job, err := readNCCLRDMAJobFixture()
	require.NoError(t, err)
	owned, createErr := createNCCLRDMAResources(tc, job, invocation)
	t.Cleanup(func() {
		if recorder.cleanupDone {
			return
		}
		if err := recorder.cleanup(tc, owned, invocation, createErr != nil); err != nil {
			t.Errorf("delete only successful-response UID-owned NCCL/RDMA resources: %v", err)
		}
	})
	require.NoError(t, createErr)
	jobUID := ownedUID(owned, ncclRDMAJobGVR, ncclRDMAJobName)
	require.NotEmpty(t, jobUID)
	suspended, found, err := unstructured.NestedBool(job.Object, "spec", "suspend")
	require.NoError(t, err)
	require.True(t, found && suspended, "persisted Job must remain suspended until Kueue admits it")

	jobDeadline := time.Now().Add(13 * time.Minute)
	ownedSelector := fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s", ncclRDMAInvocationKey, invocation)
	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, ncclRDMAJobGVR, ncclRDMAJobName)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, ownedSelector)
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
	})

	require.NoError(t, waitForNCCLRDMAWorkloadAdmitted(tc, jobUID, 2*time.Minute), "Kueue should admit the fixed Job")
	admittedAt, err := ncclRDMAWorkloadAdmittedAt(tc, jobUID)
	require.NoError(t, err)
	require.NoError(t, waitForNCCLRDMAJobSuspendedState(tc, jobUID, false, 30*time.Second),
		"Kueue should unsuspend only the admitted Job")
	pods, err := waitForNCCLRDMAPodsScheduled(tc, jobUID, invocation, 5*time.Minute)
	if err != nil && strings.Contains(err.Error(), "failed") {
		recorder.addError(rdmavalidation.ReasonRuntimeError, "pods", "an Indexed Job pod failed before placement validation")
	}
	require.NoError(t, err)
	require.NoError(t, requireNCCLRDMAEphemeralContainerDenied(tc.Ctx(), pods),
		"the untrusted identity must not add ephemeral containers to owned diagnostic pods")
	nodesByIndex, err := requireNCCLRDMAExactPlacement(pods,
		os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"))
	if err != nil {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "Indexed Job pods did not retain exact distinct-node placement")
	}
	require.NoError(t, err)
	err = requireNCCLRDMASelectedNodes(
		tc.Ctx(), tc.KubeClient(), nodesByIndex,
		os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
	)
	if err != nil {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "selected nodes did not retain the exact approved H200 selector")
	}
	require.NoError(t, err)

	remaining := time.Until(jobDeadline)
	require.Positive(t, remaining, "Job consumed the bounded diagnostic deadline before execution completed")
	err = waitForNCCLRDMAJobSucceeded(tc, jobUID, remaining)
	if err != nil && (strings.Contains(err.Error(), "Job failed") || strings.Contains(err.Error(), "failed completion indexes")) {
		recorder.addError(rdmavalidation.ReasonNonzeroExit, "job_exit_code", "Job reported a failed rank or nonzero termination")
	}
	require.NoError(t, err, "both Indexed Job pods must exit zero within the bounded diagnostic deadline")
	logs, err := readNCCLRDMACombinedLogs(tc, invocation)
	require.NoError(t, err)
	result, err := e2e.ParseNCCLRDMAOutput(logs)
	if err != nil {
		recorder.addError(
			e2e.NCCLRDMAParseFailureReason(err),
			"parser",
			"combined rank logs did not satisfy the fail-closed NCCL/RDMA parser",
		)
	}
	require.NoError(t, err, "combined rank logs must satisfy the fail-closed NCCL/RDMA parser")
	expectedNodes := [2]string{nodesByIndex[0], nodesByIndex[1]}
	if result.Nodes != expectedNodes {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "probe-reported nodes did not match Kubernetes pod placement")
	}
	require.Equal(t, expectedNodes, result.Nodes,
		"probe-reported actual nodes must match the Indexed Job pod placement")
	require.Positive(t, result.MaxAlgBW)
	require.Positive(t, result.MaxBusBW)

	pods, err = waitForNCCLRDMAPodsScheduled(tc, jobUID, invocation, 30*time.Second)
	require.NoError(t, err)
	nodes, err := readNCCLRDMASelectedNodes(tc, nodesByIndex)
	require.NoError(t, err)
	completedJob, err := tc.KubeClient().BatchV1().Jobs(stackNamespace).Get(tc.Ctx(), ncclRDMAJobName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, completedJob.Status.StartTime)
	require.NotNil(t, completedJob.Status.CompletionTime)
	manifest, err := sanitizedNCCLRDMAJob(job)
	require.NoError(t, err)
	cleanupErr := recorder.cleanup(tc, owned, invocation, false)
	contract, err := e2e.BuildNCCLRDMAValidationResult(e2e.NCCLRDMAValidationInput{
		ValidationID: invocation,
		RunID:        recorder.result.RunID, Attempt: recorder.result.Attempt,
		WorkspaceID: recorder.result.WorkspaceID, Cluster: recorder.result.Cluster,
		Namespace: stackNamespace, ProjectID: recorder.result.ProjectID,
		ExperimentID: recorder.result.ExperimentID, RunGroupID: recorder.result.RunGroupID,
		ExpectedSite:     recorder.result.Requested.Topology.Site,
		ExpectedRegion:   recorder.result.Requested.Topology.Region,
		ExpectedPool:     recorder.result.Requested.Topology.Pool,
		ExpectedGPUModel: recorder.result.Requested.Topology.GPUModel,
		SourceRevision:   recorder.result.Source.Revision,
		CreatedAt:        job.GetCreationTimestamp().Time.UTC(),
		StartedAt:        completedJob.Status.StartTime.Time.UTC(),
		AdmittedAt:       admittedAt,
		CompletedAt:      completedJob.Status.CompletionTime.Time.UTC(),
		ObservedAt:       recorder.result.Cleanup.CompletedAt,
		StaleAfter:       time.Duration(ncclRDMAStaleSeconds) * time.Second,
		Parsed:           result, Pods: pods, Nodes: nodes, SanitizedManifest: manifest,
		Cleanup: recorder.result.Cleanup,
	})
	require.NoError(t, err)
	require.NoError(t, persistNCCLRDMAContractAfterCleanup(recorder, contract, cleanupErr))
	require.Equal(t, rdmavalidation.StatusPass, contract.Status)
}

func requireNCCLRDMAEphemeralContainerDenied(ctx context.Context, pods []corev1.Pod) error {
	if len(pods) == 0 {
		return fmt.Errorf("no owned diagnostic pod is available for the ephemeral-container denial probe")
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = strings.TrimSpace(os.Getenv("NCCL_RDMA_KUBECONFIG"))
	overrides := &clientcmd.ConfigOverrides{
		CurrentContext: strings.TrimSpace(os.Getenv("NCCL_RDMA_KUBE_CONTEXT")),
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return fmt.Errorf("load explicit kubeconfig for ephemeral-container denial probe: %w", err)
	}
	config.Impersonate.UserName = strings.TrimSpace(os.Getenv("NCCL_RDMA_UNTRUSTED_USERNAME"))
	if config.Impersonate.UserName == "" {
		return fmt.Errorf("NCCL_RDMA_UNTRUSTED_USERNAME is required for the ephemeral-container denial probe")
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create untrusted client for ephemeral-container denial probe: %w", err)
	}
	return verifyNCCLRDMAEphemeralContainerDenied(ctx, client, pods[0])
}

func verifyNCCLRDMAEphemeralContainerDenied(
	ctx context.Context,
	client kubernetes.Interface,
	ownedPod corev1.Pod,
) error {
	pod := ownedPod.DeepCopy()
	allowPrivilegeEscalation := false
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    "attacker",
			Image:   "invalid.example/attacker@sha256:" + strings.Repeat("b", 64),
			Command: []string{"/bin/false"},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPrivilegeEscalation,
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			},
		},
	}}
	_, err := client.CoreV1().Pods(pod.Namespace).UpdateEphemeralContainers(
		ctx, pod.Name, pod, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
	)
	if err == nil {
		return fmt.Errorf("ephemeral-container dry-run unexpectedly succeeded")
	}
	if !apierrors.IsForbidden(err) ||
		!strings.Contains(err.Error(), "taugrid-nccl-rdma-connect-deny") {
		return fmt.Errorf("ephemeral-container dry-run was not denied by the expected policy: %w", err)
	}
	return nil
}

type ownedNCCLRDMAResource struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
	UID       types.UID
}

type ownedNCCLRMALedgerEntry struct {
	Group     string    `json:"group"`
	Version   string    `json:"version"`
	Resource  string    `json:"resource"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

type ncclRDMAResourceManifest struct {
	GVR    schema.GroupVersionResource
	Object *unstructured.Unstructured
}

func readNCCLRDMAJobFixture() (*unstructured.Unstructured, error) {
	data, err := e2e.ReadFixtureWithSubstitutions("stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml")
	if err != nil {
		return nil, err
	}
	job := &unstructured.Unstructured{}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(job); err != nil {
		return nil, fmt.Errorf("decode NCCL/RDMA Job fixture: %w", err)
	}
	return job, nil
}

func createNCCLRDMAResources(
	tc *e2e.TestContext,
	job *unstructured.Unstructured,
	invocation string,
) ([]ownedNCCLRDMAResource, error) {
	if job.GetName() != ncclRDMAJobName || job.GetNamespace() != stackNamespace {
		return nil, fmt.Errorf("NCCL/RDMA fixture identity changed unexpectedly: %s/%s", job.GetNamespace(), job.GetName())
	}
	if job.GetLabels()[ncclRDMAInvocationKey] != invocation {
		return nil, fmt.Errorf("NCCL/RDMA fixture invocation marker does not match the authorized invocation")
	}
	if err := validateNCCLRDMAOwnedUIDLedger(); err != nil {
		return nil, err
	}

	script, err := e2e.ReadRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	if err != nil {
		return nil, fmt.Errorf("read repository-owned probe: %w", err)
	}
	authKey := make([]byte, 32)
	if _, err := rand.Read(authKey); err != nil {
		return nil, fmt.Errorf("generate run-scoped authentication key: %w", err)
	}
	support, err := e2e.BuildNCCLRDMASupportResources(stackNamespace, invocation, string(script), authKey)
	if err != nil {
		return nil, err
	}
	manifests := []ncclRDMAResourceManifest{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, Object: mustUnstructured(tc, support.ServiceAccount)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Object: mustUnstructured(tc, support.ConfigMap)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, Object: mustUnstructured(tc, support.Secret)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "services"}, Object: mustUnstructured(tc, support.Service)},
		{GVR: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, Object: mustUnstructured(tc, support.NetworkPolicy)},
		{GVR: ncclRDMAJobGVR, Object: job},
	}
	for _, manifest := range manifests {
		resource := tc.DynamicClient().Resource(manifest.GVR).Namespace(stackNamespace)
		_, getErr := resource.Get(tc.Ctx(), manifest.Object.GetName(), metav1.GetOptions{})
		if getErr == nil {
			return nil, fmt.Errorf("fixed resource %s/%s already exists; refusing to replace or adopt it",
				manifest.GVR.Resource, manifest.Object.GetName())
		}
		if !apierrors.IsNotFound(getErr) {
			return nil, fmt.Errorf("check fixed %s/%s absence: %w", manifest.GVR.Resource, manifest.Object.GetName(), getErr)
		}
	}
	if err := requireNCCLRDMANamespaceApproval(tc.Ctx(), tc.KubeClient(), stackNamespace); err != nil {
		return nil, err
	}

	owned := make([]ownedNCCLRDMAResource, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.GVR == ncclRDMAJobGVR {
			if err := requireNCCLRDMANamespaceApproval(tc.Ctx(), tc.KubeClient(), stackNamespace); err != nil {
				return owned, err
			}
		}
		resource := tc.DynamicClient().Resource(manifest.GVR).Namespace(stackNamespace)
		created, createErr := resource.Create(tc.Ctx(), manifest.Object, metav1.CreateOptions{})
		item, err := successfulNCCLRDMAOwnedResource(manifest, created, createErr)
		if err != nil {
			return owned, err
		}
		owned = append(owned, item)
		if err := appendNCCLRDMAOwnedUID(item); err != nil {
			return owned, fmt.Errorf("record successful create UID for %s/%s: %w",
				manifest.GVR.Resource, manifest.Object.GetName(), err)
		}
		if manifest.GVR == ncclRDMAJobGVR {
			job.Object = created.Object
		}
	}
	return owned, nil
}

func successfulNCCLRDMAOwnedResource(
	manifest ncclRDMAResourceManifest,
	created *unstructured.Unstructured,
	createErr error,
) (ownedNCCLRDMAResource, error) {
	if createErr != nil {
		return ownedNCCLRDMAResource{}, fmt.Errorf(
			"create %s/%s returned an ambiguous error without an owned UID; the object may have persisted and requires manual inspection, so cleanup will not adopt it by name or labels: %w",
			manifest.GVR.Resource, manifest.Object.GetName(), createErr,
		)
	}
	if created == nil || created.GetUID() == "" {
		return ownedNCCLRDMAResource{}, fmt.Errorf(
			"create %s/%s succeeded without returning an owned UID; the object may have persisted and requires manual inspection, so cleanup will not adopt it by name or labels",
			manifest.GVR.Resource, manifest.Object.GetName(),
		)
	}
	return ownedNCCLRDMAResource{
		GVR:       manifest.GVR,
		Namespace: manifest.Object.GetNamespace(),
		Name:      manifest.Object.GetName(),
		UID:       created.GetUID(),
	}, nil
}

func mustUnstructured(tc *e2e.TestContext, object interface{}) *unstructured.Unstructured {
	tc.Helper()
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	require.NoError(tc, err)
	return &unstructured.Unstructured{Object: raw}
}

func validateNCCLRDMAOwnedUIDLedger() error {
	path := strings.TrimSpace(os.Getenv(ncclRDMAOwnedUIDFile))
	if path == "" {
		return fmt.Errorf("%s is required so cleanup uses only UIDs returned by successful CREATE responses",
			ncclRDMAOwnedUIDFile)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat owned UID ledger %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("owned UID ledger %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open owned UID ledger %s: %w", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		if _, err := decodeNCCLRDMAOwnedUIDLedgerEntry(scanner.Bytes()); err != nil {
			return fmt.Errorf("parse owned UID ledger %s line %d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read owned UID ledger %s: %w", path, err)
	}
	return nil
}

func appendNCCLRDMAOwnedUID(item ownedNCCLRDMAResource) error {
	entry := ownedNCCLRMALedgerEntry{
		Group:     item.GVR.Group,
		Version:   item.GVR.Version,
		Resource:  item.GVR.Resource,
		Namespace: item.Namespace,
		Name:      item.Name,
		UID:       item.UID,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(os.Getenv(ncclRDMAOwnedUIDFile))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.WriteByte('\n'); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return file.Sync()
}

func decodeNCCLRDMAOwnedUIDLedgerEntry(data []byte) (ownedNCCLRMALedgerEntry, error) {
	var entry ownedNCCLRMALedgerEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return entry, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return entry, errors.New("multiple JSON values are not allowed")
		}
		return entry, fmt.Errorf("decode trailing ledger data: %w", err)
	}
	if strings.TrimSpace(entry.Version) == "" || strings.TrimSpace(entry.Resource) == "" ||
		strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(string(entry.UID)) == "" {
		return entry, errors.New("version, resource, name, and UID are required")
	}
	for field, value := range map[string]string{
		"group": entry.Group, "version": entry.Version, "resource": entry.Resource,
		"namespace": entry.Namespace, "name": entry.Name, "uid": string(entry.UID),
	} {
		if strings.ContainsAny(value, "\r\n") {
			return entry, fmt.Errorf("%s contains a newline", field)
		}
	}
	return entry, nil
}

func requireNCCLRDMANamespaceApproval(ctx context.Context, kubeClient kubernetes.Interface, namespace string) error {
	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read NCCL/RDMA namespace %s immediately before create: %w", namespace, err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		return fmt.Errorf("namespace %s no longer has pod-security.kubernetes.io/enforce=restricted; refusing create", namespace)
	}
	if ns.Annotations["tau.azure.com/nccl-rdma-diagnostic-approved"] != "true" {
		return fmt.Errorf("namespace %s no longer has tau.azure.com/nccl-rdma-diagnostic-approved=true; refusing create", namespace)
	}
	return nil
}

func waitForNCCLRDMAWorkloadAdmitted(tc *e2e.TestContext, uid types.UID, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.DynamicClient().Resource(ncclRDMAJobGVR).Namespace(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.GetUID() != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.GetUID())
		}
		workloads, err := tc.DynamicClient().Resource(e2e.WorkloadGVR).Namespace(stackNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for i := range workloads.Items {
			workload := &workloads.Items[i]
			owned := false
			for _, ref := range workload.GetOwnerReferences() {
				if ref.Kind == "Job" && ref.Name == ncclRDMAJobName && ref.UID == job.GetUID() {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
			conditions, _, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
			if err != nil {
				return false, err
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]interface{})
				if ok && condition["type"] == "Admitted" && condition["status"] == "True" {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

func ncclRDMAWorkloadAdmittedAt(tc *e2e.TestContext, uid types.UID) (time.Time, error) {
	workloads, err := tc.DynamicClient().Resource(e2e.WorkloadGVR).Namespace(stackNamespace).List(
		tc.Ctx(), metav1.ListOptions{},
	)
	if err != nil {
		return time.Time{}, err
	}
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		for _, ref := range workload.GetOwnerReferences() {
			if ref.Kind != "Job" || ref.Name != ncclRDMAJobName || ref.UID != uid {
				continue
			}
			conditions, _, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
			if err != nil {
				return time.Time{}, err
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]interface{})
				if !ok || condition["type"] != "Admitted" || condition["status"] != "True" {
					continue
				}
				value, ok := condition["lastTransitionTime"].(string)
				if !ok || value == "" {
					return time.Time{}, fmt.Errorf("admitted Workload is missing lastTransitionTime")
				}
				admittedAt, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return time.Time{}, fmt.Errorf("parse Workload admitted lastTransitionTime: %w", err)
				}
				return admittedAt.UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("owned admitted Workload was not found")
}

func waitForNCCLRDMAJobSuspendedState(tc *e2e.TestContext, uid types.UID, want bool, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.DynamicClient().Resource(ncclRDMAJobGVR).Namespace(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.GetUID() != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.GetUID())
		}
		suspended, found, err := unstructured.NestedBool(job.Object, "spec", "suspend")
		if err != nil {
			return false, err
		}
		return found && suspended == want, nil
	})
}

func waitForNCCLRDMAJobSucceeded(tc *e2e.TestContext, uid types.UID, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.KubeClient().BatchV1().Jobs(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.UID != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.UID)
		}
		complete := false
		for _, condition := range job.Status.Conditions {
			if condition.Status == corev1.ConditionTrue &&
				(condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) {
				return false, fmt.Errorf("Job failed: reason=%s message=%s", condition.Reason, condition.Message)
			}
			if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobComplete {
				complete = true
			}
		}
		if job.Status.FailedIndexes != nil && *job.Status.FailedIndexes != "" {
			return false, fmt.Errorf("Job reported failed completion indexes %q", *job.Status.FailedIndexes)
		}
		if complete && job.Status.Succeeded == 2 && job.Status.Failed == 0 &&
			job.Status.CompletedIndexes == "0,1" {
			return true, nil
		}
		return false, nil
	})
}

func waitForNCCLRDMAPodsScheduled(
	tc *e2e.TestContext,
	jobUID types.UID,
	invocation string,
	timeout time.Duration,
) ([]corev1.Pod, error) {
	var result []corev1.Pod
	err := wait.PollUntilContextTimeout(tc.Ctx(), time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s",
				ncclRDMAInvocationKey, invocation),
		})
		if err != nil {
			return false, err
		}
		if len(pods.Items) != 2 {
			return false, nil
		}
		for _, pod := range pods.Items {
			owned := false
			for _, owner := range pod.OwnerReferences {
				if owner.APIVersion == "batch/v1" && owner.Kind == "Job" &&
					owner.Name == ncclRDMAJobName && owner.UID == jobUID {
					owned = true
					break
				}
			}
			if !owned {
				return false, fmt.Errorf("pod %s is not owned by Job UID %s", pod.Name, jobUID)
			}
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("Indexed Job pod %s failed before placement validation completed", pod.Name)
			}
			if pod.Spec.NodeName == "" || (pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded) {
				return false, nil
			}
		}
		result = pods.Items
		return true, nil
	})
	return result, err
}

func requireNCCLRDMAExactPlacement(pods []corev1.Pod, selectorKey, selectorValue string) ([2]string, error) {
	var nodes [2]string
	seenNodes := map[string]bool{}
	seenIndexes := map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeSelector[selectorKey] != selectorValue {
			return nodes, fmt.Errorf("pod %s does not retain exact H200 selector", pod.Name)
		}
		index := pod.Labels["batch.kubernetes.io/job-completion-index"]
		if index != "0" && index != "1" {
			return nodes, fmt.Errorf("pod %s has invalid Indexed Job completion label %q", pod.Name, index)
		}
		if seenIndexes[index] {
			return nodes, fmt.Errorf("duplicate Indexed Job completion label %q", index)
		}
		seenIndexes[index] = true
		nodes[index[0]-'0'] = pod.Spec.NodeName
		seenNodes[pod.Spec.NodeName] = true
	}
	if len(seenNodes) != 2 || len(seenIndexes) != 2 {
		return nodes, fmt.Errorf("expected exactly two completion indexes on distinct actual nodes")
	}
	return nodes, nil
}

func requireNCCLRDMASelectedNodes(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	nodes [2]string,
	selectorKey, selectorValue string,
) error {
	if selectorKey == "" || selectorValue == "" {
		return fmt.Errorf("exact H200 selector key and value are required")
	}

	for _, nodeName := range nodes {
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read selected node %s: %w", nodeName, err)
		}
		if node.Labels[selectorKey] != selectorValue {
			return fmt.Errorf("selected node %s does not have %s=%s", nodeName, selectorKey, selectorValue)
		}
	}
	return nil
}

func readNCCLRDMASelectedNodes(
	tc *e2e.TestContext,
	nodes [2]string,
) (map[string]*corev1.Node, error) {
	result := make(map[string]*corev1.Node, 2)
	for _, name := range nodes {
		node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("read selected node %s for validation artifact: %w", name, err)
		}
		result[name] = node
	}
	return result, nil
}

func sanitizedNCCLRDMAJob(job *unstructured.Unstructured) ([]byte, error) {
	sanitized := job.DeepCopy()
	sanitized.SetManagedFields(nil)
	sanitized.SetResourceVersion("")
	sanitized.SetGeneration(0)
	unstructured.RemoveNestedField(sanitized.Object, "status")
	raw, err := json.MarshalIndent(sanitized.Object, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal sanitized Job manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

func readNCCLRDMACombinedLogs(tc *e2e.TestContext, invocation string) (string, error) {
	pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s",
			ncclRDMAInvocationKey, invocation),
	})
	if err != nil {
		return "", err
	}
	if len(pods.Items) != 2 {
		return "", fmt.Errorf("expected exactly two Indexed Job pods, got %d", len(pods.Items))
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Labels["batch.kubernetes.io/job-completion-index"] <
			pods.Items[j].Labels["batch.kubernetes.io/job-completion-index"]
	})
	var combined strings.Builder
	for _, pod := range pods.Items {
		rank := pod.Labels["batch.kubernetes.io/job-completion-index"]
		if rank != "0" && rank != "1" {
			return "", fmt.Errorf("pod %s has invalid completion index %q", pod.Name, rank)
		}
		fmt.Fprintf(&combined, "TAUGRID_RANK_LOG_BEGIN rank=%s\n", rank)
		stream, err := tc.KubeClient().CoreV1().Pods(stackNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "probe",
		}).Stream(tc.Ctx())
		if err != nil {
			return "", fmt.Errorf("open logs for %s: %w", pod.Name, err)
		}
		data, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil {
			return "", fmt.Errorf("read logs for %s: %w", pod.Name, readErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close logs for %s: %w", pod.Name, closeErr)
		}
		combined.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			combined.WriteByte('\n')
		}
		fmt.Fprintf(&combined, "TAUGRID_RANK_LOG_END rank=%s\n", rank)
	}
	return combined.String(), nil
}

func deleteOwnedNCCLRDMAResourcesAndWait(
	tc *e2e.TestContext,
	owned []ownedNCCLRDMAResource,
	invocation string,
) error {
	return deleteOwnedNCCLRDMAResourcesWithin(
		tc.Ctx(), tc.DynamicClient(), tc.KubeClient(), owned, invocation, 3*time.Minute,
	)
}

func deleteOwnedNCCLRDMAResourcesWithin(
	parent context.Context,
	dynamicClient dynamic.Interface,
	kubeClient kubernetes.Interface,
	owned []ownedNCCLRDMAResource,
	invocation string,
	timeout time.Duration,
) error {
	cleanupCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	pollInterval := time.Second
	if timeout < 10*time.Second {
		pollInterval = timeout / 10
		if pollInterval <= 0 {
			pollInterval = time.Millisecond
		}
	}
	var cleanupErrors []error
	type pendingResource struct {
		item     ownedNCCLRDMAResource
		resource dynamic.ResourceInterface
	}
	jobs := make([]pendingResource, 0, 1)
	support := make([]pendingResource, 0, len(owned))
	for index := len(owned) - 1; index >= 0; index-- {
		item := owned[index]
		var resource dynamic.ResourceInterface = dynamicClient.Resource(item.GVR)
		if item.Namespace != "" {
			resource = dynamicClient.Resource(item.GVR).Namespace(item.Namespace)
		}
		candidate := pendingResource{item: item, resource: resource}
		if item.GVR == ncclRDMAJobGVR {
			jobs = append(jobs, candidate)
		} else {
			support = append(support, candidate)
		}
	}
	startDelete := func(candidate pendingResource) {
		foreground := metav1.DeletePropagationForeground
		err := candidate.resource.Delete(cleanupCtx, candidate.item.Name, metav1.DeleteOptions{
			PropagationPolicy: &foreground,
			Preconditions:     &metav1.Preconditions{UID: &candidate.item.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"start UID-owned delete for %s/%s UID %s: %w",
				candidate.item.GVR.Resource, candidate.item.Name, candidate.item.UID, err,
			))
		}
	}

	for _, job := range jobs {
		startDelete(job)
	}
	if len(jobs) > 0 {
		if len(cleanupErrors) > 0 {
			return errors.Join(cleanupErrors...)
		}
		selector := fmt.Sprintf(
			"e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s",
			ncclRDMAInvocationKey,
			invocation,
		)
		var barrierError error
		err := wait.PollUntilContextTimeout(cleanupCtx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
			for _, job := range jobs {
				current, err := job.resource.Get(ctx, job.item.Name, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					continue
				}
				if err != nil {
					barrierError = fmt.Errorf("verify Job deletion: %w", err)
					return false, nil
				}
				if current.GetUID() == job.item.UID {
					return false, nil
				}
			}
			pods, err := kubeClient.CoreV1().Pods(stackNamespace).List(
				ctx,
				metav1.ListOptions{LabelSelector: selector},
			)
			if err != nil {
				barrierError = fmt.Errorf("verify diagnostic Pod drain: %w", err)
				return false, nil
			}
			return len(pods.Items) == 0, nil
		})
		if err != nil {
			cleanupErrors = append(
				cleanupErrors,
				fmt.Errorf("Job deletion and diagnostic Pod drain did not complete before cleanup deadline: %w", err),
			)
			if barrierError != nil {
				cleanupErrors = append(cleanupErrors, barrierError)
			}
			return errors.Join(cleanupErrors...)
		}
	}

	for _, candidate := range support {
		startDelete(candidate)
	}

	remaining := append([]pendingResource(nil), support...)
	err := wait.PollUntilContextTimeout(cleanupCtx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		next := remaining[:0]
		for _, candidate := range remaining {
			current, err := candidate.resource.Get(ctx, candidate.item.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				next = append(next, candidate)
				continue
			}
			if current.GetUID() != candidate.item.UID {
				continue
			}
			next = append(next, candidate)
		}
		remaining = next
		return len(remaining) == 0, nil
	})
	if err != nil {
		for _, candidate := range remaining {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"UID-owned %s/%s UID %s remained after cleanup deadline",
				candidate.item.GVR.Resource, candidate.item.Name, candidate.item.UID,
			))
		}
	}
	return errors.Join(cleanupErrors...)
}

func ownedUID(owned []ownedNCCLRDMAResource, gvr schema.GroupVersionResource, name string) types.UID {
	for _, item := range owned {
		if item.GVR == gvr && item.Name == name {
			return item.UID
		}
	}
	return ""
}
