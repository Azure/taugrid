// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	e2e "github.com/Azure/taugrid/tests/e2e"
)

func TestNCCLRDMAFixtureResolvesFromStackPackageCWD(t *testing.T) {
	t.Setenv("NCCL_RDMA_INVOCATION", "nccl-rdma-0123456789abcdef0123456789abcdef")
	t.Setenv("E2E_STACK_NAMESPACE", "approved-nccl-rdma")
	t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", "h200-rdma")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")

	data, err := e2e.ReadFixtureWithSubstitutions("stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml")
	require.NoError(t, err)
	require.Contains(t, string(data), "namespace: approved-nccl-rdma")
	require.Contains(t, string(data), "completionMode: Indexed")
	require.NotContains(t, string(data), "{{")
}

func TestRequireNCCLRDMANamespaceApproval(t *testing.T) {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "approved",
			Labels: map[string]string{
				"pod-security.kubernetes.io/enforce": "restricted",
			},
			Annotations: map[string]string{
				"tau.azure.com/nccl-rdma-diagnostic-approved": "true",
			},
		},
	}
	client := kubernetesfake.NewSimpleClientset(namespace)
	require.NoError(t, requireNCCLRDMANamespaceApproval(context.Background(), client, namespace.Name))

	namespace.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
	_, err := client.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, requireNCCLRDMANamespaceApproval(context.Background(), client, namespace.Name), "refusing create")
}

func TestSuccessfulNCCLRDMAOwnedResourceNeverAdoptsAmbiguousCreate(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]interface{}{
			"name": ncclRDMAJobName,
			"uid":  "owned-uid",
		},
	}}
	manifest := ncclRDMAResourceManifest{GVR: ncclRDMAJobGVR, Object: job.DeepCopy()}
	item, err := successfulNCCLRDMAOwnedResource(
		manifest, job, context.DeadlineExceeded,
	)
	require.Empty(t, item.UID)
	require.ErrorContains(t, err, "may have persisted")
	require.ErrorContains(t, err, "will not adopt")

	withoutUID := job.DeepCopy()
	withoutUID.SetUID("")
	item, err = successfulNCCLRDMAOwnedResource(manifest, withoutUID, nil)
	require.Empty(t, item.UID)
	require.ErrorContains(t, err, "without returning an owned UID")

	item, err = successfulNCCLRDMAOwnedResource(manifest, job, nil)
	require.NoError(t, err)
	require.Equal(t, types.UID("owned-uid"), item.UID)
}

func TestNCCLRDMAOwnedUIDLedgerRecordsOnlySuccessfulCreateResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned-uids")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	t.Setenv(ncclRDMAOwnedUIDFile, path)
	require.NoError(t, validateNCCLRDMAOwnedUIDLedger())

	item := ownedNCCLRDMAResource{
		GVR: ncclRDMAJobGVR, Name: ncclRDMAJobName, UID: types.UID("owned-uid"),
	}

	require.NoError(t, appendNCCLRDMAOwnedUID(item))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "batch|v1|jobs|e2e-nccl-rdma-2x1xh200|owned-uid\n", string(data))

	t.Setenv(ncclRDMAOwnedUIDFile, filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, validateNCCLRDMAOwnedUIDLedger(), "stat owned UID ledger")

	t.Setenv(ncclRDMAOwnedUIDFile, "")
	require.ErrorContains(
		t,
		validateNCCLRDMAOwnedUIDLedger(),
		"cleanup uses only UIDs returned by successful CREATE responses",
	)
}

func TestOwnedResourceRefsUseOnlySuccessfulCreateUIDs(t *testing.T) {
	owned := []ownedNCCLRDMAResource{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, Name: "auth", UID: "secret-uid"},
		{GVR: ncclRDMAJobGVR, Name: ncclRDMAJobName, UID: "job-uid"},
	}
	refs := ownedResourceRefs(owned)
	require.Len(t, refs, 2)
	require.Equal(t, "secret-uid", refs[0].UID)
	require.Equal(t, "Secret", refs[0].Kind)
	require.Equal(t, "job-uid", refs[1].UID)
	require.Equal(t, "batch/v1", refs[1].APIVersion)
	require.Equal(t, "Job", refs[1].Kind)
}

func TestDeleteOwnedNCCLRDMAResourcesContinuesAfterOneDeleteFails(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]interface{}{
			"name": ncclRDMAJobName, "namespace": stackNamespace, "uid": "job-uid",
		},
	}}
	configMap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{
			"name": "nccl-rdma-probe", "namespace": stackNamespace, "uid": "configmap-uid",
		},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job, configMap)
	dynamicClient.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("job finalizer is stuck")
	})
	owned := []ownedNCCLRDMAResource{
		{
			GVR:  schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			Name: "nccl-rdma-probe", UID: "configmap-uid",
		},
		{GVR: ncclRDMAJobGVR, Name: ncclRDMAJobName, UID: "job-uid"},
	}

	err := deleteOwnedNCCLRDMAResourcesWithin(
		context.Background(),
		dynamicClient,
		kubernetesfake.NewSimpleClientset(),
		owned,
		"nccl-rdma-0123456789abcdef0123456789abcdef",
		50*time.Millisecond,
	)
	require.ErrorContains(t, err, "job finalizer is stuck")
	_, getErr := dynamicClient.Resource(schema.GroupVersionResource{
		Version: "v1", Resource: "configmaps",
	}).Namespace(stackNamespace).Get(context.Background(), "nccl-rdma-probe", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(getErr), "cleanup must attempt later resources after the Job delete fails")
}

func TestRequireNCCLRDMAExactPlacement(t *testing.T) {
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"batch.kubernetes.io/job-completion-index": "0"}},
			Spec: corev1.PodSpec{
				NodeName:     "h200-a",
				NodeSelector: map[string]string{"accelerator": "nvidia-h200"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"batch.kubernetes.io/job-completion-index": "1"}},
			Spec: corev1.PodSpec{
				NodeName:     "h200-b",
				NodeSelector: map[string]string{"accelerator": "nvidia-h200"},
			},
		},
	}
	nodes, err := requireNCCLRDMAExactPlacement(pods, "accelerator", "nvidia-h200")
	require.NoError(t, err)
	require.Equal(t, [2]string{"h200-a", "h200-b"}, nodes)

	pods[1].Spec.NodeName = "h200-a"
	_, err = requireNCCLRDMAExactPlacement(pods, "accelerator", "nvidia-h200")
	require.Error(t, err)
}

func TestRequireNCCLRDMASelectedNodes(t *testing.T) {
	nodes := [2]string{"h200-a", "h200-b"}
	client := kubernetesfake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "h200-a", Labels: map[string]string{"accelerator": "nvidia-h200"},
		}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "h200-b", Labels: map[string]string{"accelerator": "nvidia-h200"},
		}},
	)
	require.NoError(t, requireNCCLRDMASelectedNodes(
		context.Background(), client, nodes, "accelerator", "nvidia-h200",
	))

	node, err := client.CoreV1().Nodes().Get(context.Background(), "h200-b", metav1.GetOptions{})
	require.NoError(t, err)
	node.Labels["accelerator"] = "other"
	_, err = client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, requireNCCLRDMASelectedNodes(
		context.Background(), client, nodes, "accelerator", "nvidia-h200",
	))
}
