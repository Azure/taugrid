// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestConformanceExistingQueueRoutingIsReadOnly(t *testing.T) {
	for _, c := range []struct {
		normal, large, wantNormal, wantLarge string
	}{
		{"batch", "training", "batch", "training"},
		{"", "training", argoCDStackQueue, "training"},
		{"shared", "shared", "shared", "shared"},
	} {
		t.Run(c.normal+"/"+c.large, func(t *testing.T) {
			t.Setenv("E2E_STACK_USE_ARGOCD_QUEUE", "1")
			t.Setenv("E2E_STACK_NAMESPACE", "shared-workloads")
			t.Setenv("E2E_STACK_QUEUE", c.normal)
			t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", c.large)
			require.True(t, stackUsesArgoCDQueue())
			require.Equal(t, "shared-workloads", stackNamespaceForRun())
			require.Equal(t, c.wantNormal, stackQueueForRun())
			require.Equal(t, c.wantLarge, stackLargeGPUQueueForRun())
			originalNamespace := stackNamespace
			stackNamespace = stackNamespaceForRun()
			t.Cleanup(func() { stackNamespace = originalNamespace })
			kube := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: stackNamespace}})
			objects := []runtime.Object{}
			for _, name := range []string{c.wantNormal, c.wantLarge} {
				if len(objects) == 1 && c.wantNormal == c.wantLarge {
					continue
				}
				objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "LocalQueue",
					"metadata": map[string]interface{}{"name": name, "namespace": stackNamespace},
				}})
			}
			dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
			require.NoError(t, requireArgoCDStackQueue(context.Background(), kube, dyn))
			require.Len(t, kube.Actions(), 1)
			require.Len(t, dyn.Actions(), len(objects))
			for _, action := range append(kube.Actions(), dyn.Actions()...) {
				require.Equal(t, "get", action.GetVerb())
			}
		})
	}
}
