// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRecoverSingleSchedulerBlockedGPUWorkerAccountsAndBinds(t *testing.T) {
	const (
		namespace  = "nightly"
		targetNode = "gpu-target"
		otherNode  = "gpu-other"
	)

	objects := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: targetNode},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
			}},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "insufficient-gpu", Namespace: namespace},
			InvolvedObject: corev1.ObjectReference{Name: "worker-pending"},
			Message:        "0/2 nodes are available: Insufficient nvidia.com/gpu",
		},
	}
	for i := range 7 {
		objects = append(objects, recoveryWorkerPod(namespace,
			fmt.Sprintf("worker-target-%d", i), targetNode, corev1.PodRunning))
	}
	for i := range 8 {
		objects = append(objects, recoveryWorkerPod(namespace,
			fmt.Sprintf("worker-other-%d", i), otherNode, corev1.PodRunning))
	}
	objects = append(objects, recoveryWorkerPod(namespace,
		"worker-pending", "", corev1.PodPending))

	client := fake.NewSimpleClientset(objects...)
	var binding *corev1.Binding
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok || createAction.GetSubresource() != "binding" {
			return false, nil, nil
		}
		binding = createAction.GetObject().(*corev1.Binding)
		return true, binding, nil
	})

	tc := &TestContext{T: t, ctx: context.Background(), kubeClient: client}
	err := tc.RecoverSingleSchedulerBlockedGPUWorker(SchedulerRecoveryOptions{
		Namespace:       namespace,
		WorkerSelector:  "test=worker",
		TargetNode:      targetNode,
		ExpectedWorkers: 16,
		Timeout:         time.Second,
		PollInterval:    time.Millisecond,
		Eligible: func(observation SchedulerRecoveryObservation) bool {
			require.Equal(t, SchedulerRecoveryObservation{
				TotalWorkers:          16,
				RunningWorkers:        15,
				PendingWorkers:        1,
				RunningOnTarget:       7,
				RunningOnOtherNodes:   8,
				RequestedGPUsOnTarget: 7,
				PendingGPURequest:     1,
				TargetAllocatableGPUs: 8,
				HasInsufficientEvent:  true,
			}, observation)
			return true
		},
		Description: "test worker",
	})
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.Equal(t, "worker-pending", binding.Name)
	require.Equal(t, namespace, binding.Namespace)
	require.Equal(t, targetNode, binding.Target.Name)
}

func TestPodGPURequestUsesLargestSchedulingRequirement(t *testing.T) {
	gpuName := corev1.ResourceName("nvidia.com/gpu")
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			{Resources: gpuResources("1")},
			{Resources: gpuResources("1")},
		},
		InitContainers: []corev1.Container{
			{Resources: gpuResources("4")},
		},
	}}
	require.Equal(t, int64(4), podGPURequest(pod, gpuName))
}

func recoveryWorkerPod(namespace, name, node string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"test": "worker"},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{
				{Resources: gpuResources("1")},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func gpuResources(gpus string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(gpus),
	}}
}
