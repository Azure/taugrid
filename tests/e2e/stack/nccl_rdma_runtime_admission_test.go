// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestVerifyNCCLRDMAEphemeralContainerDeniedUsesExistingOwnedPod(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "e2e-nccl-rdma-2x1xh200-0", Namespace: stackNamespace,
	}}
	tests := map[string]struct {
		reactionError error
		wantError     string
	}{
		"expected policy denial": {
			reactionError: apierrors.NewForbidden(
				schema.GroupResource{Resource: "pods/ephemeralcontainers"},
				pod.Name,
				errors.New("taugrid-nccl-rdma-connect-deny denied request"),
			),
		},
		"not found is not enforcement evidence": {
			reactionError: apierrors.NewNotFound(
				schema.GroupResource{Resource: "pods/ephemeralcontainers"},
				pod.Name,
			),
			wantError: "was not denied by the expected policy",
		},
		"unexpected success fails": {
			wantError: "unexpectedly succeeded",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := fake.NewSimpleClientset(pod.DeepCopy())
			client.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				require.Equal(t, "ephemeralcontainers", action.GetSubresource())
				updated := action.(k8stesting.UpdateAction).GetObject().(*corev1.Pod)
				require.Len(t, updated.Spec.EphemeralContainers, 1)
				security := updated.Spec.EphemeralContainers[0].SecurityContext
				require.NotNil(t, security)
				require.NotNil(t, security.AllowPrivilegeEscalation)
				require.False(t, *security.AllowPrivilegeEscalation)
				require.Equal(t, []corev1.Capability{"ALL"}, security.Capabilities.Drop)
				if test.reactionError != nil {
					return true, nil, test.reactionError
				}
				return true, pod.DeepCopy(), nil
			})
			err := verifyNCCLRDMAEphemeralContainerDenied(context.Background(), client, pod)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
}
