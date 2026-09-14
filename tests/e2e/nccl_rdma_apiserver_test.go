// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestNCCLRDMAManualSelectorAPIServerDefaulting(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to installed envtest binaries for real API-server defaulting coverage")
	}

	testEnvironment := &envtest.Environment{}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testEnvironment.Stop())
	})

	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	ctx := context.Background()
	namespace := "nccl-rdma-defaulting"
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, job := renderNCCLRDMAFixture(t)
	job.Namespace = namespace
	created, err := client.BatchV1().Jobs(namespace).Create(ctx, &job, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotNil(t, created.Spec.ManualSelector)
	require.True(t, *created.Spec.ManualSelector)
	require.Equal(t, job.Spec.Selector, created.Spec.Selector)
	require.Equal(t, job.Spec.Template.Labels, created.Spec.Template.Labels)
	require.NotContains(t, created.Spec.Selector.MatchLabels, "batch.kubernetes.io/controller-uid")
	require.NotContains(t, created.Spec.Selector.MatchLabels, "controller-uid")
	require.NotContains(t, created.Spec.Template.Labels, "batch.kubernetes.io/controller-uid")
	require.NotContains(t, created.Spec.Template.Labels, "controller-uid")

	overlapping := job.DeepCopy()
	overlapping.Name = "e2e-nccl-rdma-overlap"
	overlapping.Spec.Selector.MatchLabels[NCCLRDMAInvocationKey] =
		"nccl-rdma-ffffffffffffffffffffffffffffffff"
	_, err = client.BatchV1().Jobs(namespace).Create(ctx, overlapping, metav1.CreateOptions{})
	require.ErrorContains(t, err, "selector")
}
