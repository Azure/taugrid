// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	e2e "github.com/Azure/taugrid/tests/e2e"
)

func TestNCCLRDMAFixtureResolvesFromStackPackageCWD(t *testing.T) {
	t.Setenv("NCCL_RDMA_E2E_IMAGE", "mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("NCCL_RDMA_INVOCATION", "nccl-rdma-0123456789abcdef0123456789abcdef")
	t.Setenv("E2E_STACK_NAMESPACE", "approved-nccl-rdma")
	t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", "h200-rdma")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")

	data, err := e2e.ReadFixtureWithSubstitutions("stack/fixtures/nccl-rdma-mpijob-2x8xh200.yaml")
	require.NoError(t, err)
	require.Contains(t, string(data), "namespace: approved-nccl-rdma")
	require.Contains(t, string(data), "kueue.x-k8s.io/queue-name: h200-rdma")
	require.NotContains(t, string(data), "{{")
}

func TestRequireNCCLRDMANamespaceApproval(t *testing.T) {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "approved",
			Labels: map[string]string{
				"pod-security.kubernetes.io/enforce": "privileged",
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

func TestClaimAmbiguousNCCLRDMACreateOnlyClaimsMatchingInvocation(t *testing.T) {
	const invocation = "nccl-rdma-0123456789abcdef0123456789abcdef"
	job := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]interface{}{
			"name":      ncclRDMAMPIJobName,
			"namespace": stackNamespace,
			"uid":       "owned-uid",
			"labels": map[string]interface{}{
				ncclRDMAInvocationKey: invocation,
			},
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job)
	resource := client.Resource(mpiJobGVR).Namespace(stackNamespace)

	uid, err := claimAmbiguousNCCLRDMACreate(context.Background(), resource, invocation, context.DeadlineExceeded)
	require.Equal(t, types.UID("owned-uid"), uid)
	require.ErrorContains(t, err, "ambiguous error")

	uid, err = claimAmbiguousNCCLRDMACreate(context.Background(), resource, "nccl-rdma-ffffffffffffffffffffffffffffffff", errors.New("timeout"))
	require.Empty(t, uid)
	require.ErrorContains(t, err, "refusing cleanup")
}
