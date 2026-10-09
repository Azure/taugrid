// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchedulerRecoveryObservation contains the verified scheduler and GPU
// accounting state passed to a workload-specific recovery policy.
type SchedulerRecoveryObservation struct {
	TotalWorkers          int
	RunningWorkers        int
	PendingWorkers        int
	RunningOnTarget       int
	RunningOnOtherNodes   int
	RequestedGPUsOnTarget int64
	PendingGPURequest     int64
	TargetAllocatableGPUs int64
	HasInsufficientEvent  bool
}

// SchedulerRecoveryOptions configures the guarded direct-binding workaround.
// Eligible must fail closed and return true only for the caller's exact,
// previously verified scheduler defect.
type SchedulerRecoveryOptions struct {
	Namespace       string
	WorkerSelector  string
	TargetNode      string
	ExpectedWorkers int
	Timeout         time.Duration
	PollInterval    time.Duration
	Eligible        func(SchedulerRecoveryObservation) bool
	Description     string
}

// RecoverSingleSchedulerBlockedGPUWorker binds one Pending GPU worker only
// after shared accounting and scheduler evidence satisfy the caller's policy.
func (tc *TestContext) RecoverSingleSchedulerBlockedGPUWorker(options SchedulerRecoveryOptions) error {
	tc.Helper()
	if options.Namespace == "" || options.WorkerSelector == "" || options.TargetNode == "" {
		return fmt.Errorf("scheduler recovery requires namespace, worker selector, and target node")
	}
	if options.ExpectedWorkers <= 0 || options.Timeout <= 0 || options.Eligible == nil {
		return fmt.Errorf("scheduler recovery requires expected workers, timeout, and eligibility policy")
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 2 * time.Second
	}
	if options.Description == "" {
		options.Description = "GPU worker"
	}

	client := tc.KubeClient()
	gpuName := corev1.ResourceName("nvidia.com/gpu")
	deadline := time.Now().Add(options.Timeout)

	for time.Now().Before(deadline) {
		pods, err := client.CoreV1().Pods(options.Namespace).List(tc.Ctx(), metav1.ListOptions{
			LabelSelector: options.WorkerSelector,
		})
		if err != nil {
			return fmt.Errorf("list %s pods for scheduler recovery: %w", options.Description, err)
		}

		observation := SchedulerRecoveryObservation{TotalWorkers: len(pods.Items)}
		runningByNode := map[string]int{}
		var pendingPod *corev1.Pod
		for i := range pods.Items {
			pod := &pods.Items[i]
			switch pod.Status.Phase {
			case corev1.PodRunning:
				observation.RunningWorkers++
				runningByNode[pod.Spec.NodeName]++
			case corev1.PodPending:
				observation.PendingWorkers++
				pendingPod = pod
			}
		}
		if observation.RunningWorkers == options.ExpectedWorkers {
			return nil
		}
		if pendingPod == nil || observation.PendingWorkers != 1 {
			time.Sleep(options.PollInterval)
			continue
		}

		observation.RunningOnTarget = runningByNode[options.TargetNode]
		for node, count := range runningByNode {
			if node != options.TargetNode {
				observation.RunningOnOtherNodes += count
			}
		}
		observation.PendingGPURequest = podGPURequest(pendingPod, gpuName)

		node, err := client.CoreV1().Nodes().Get(tc.Ctx(), options.TargetNode, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get scheduler recovery node %s: %w", options.TargetNode, err)
		}
		allocatable := node.Status.Allocatable[gpuName]
		observation.TargetAllocatableGPUs = allocatable.Value()

		nodePods, err := client.CoreV1().Pods("").List(tc.Ctx(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", options.TargetNode),
		})
		if err != nil {
			return fmt.Errorf("list pods on scheduler recovery node %s: %w", options.TargetNode, err)
		}
		for i := range nodePods.Items {
			pod := &nodePods.Items[i]
			if pod.Spec.NodeName != options.TargetNode ||
				pod.Status.Phase == corev1.PodSucceeded ||
				pod.Status.Phase == corev1.PodFailed {
				continue
			}
			observation.RequestedGPUsOnTarget += podGPURequest(pod, gpuName)
		}

		events, err := client.CoreV1().Events(options.Namespace).List(tc.Ctx(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", pendingPod.Name),
		})
		if err != nil {
			return fmt.Errorf("list events for pending %s pod %s: %w",
				options.Description, pendingPod.Name, err)
		}
		for i := range events.Items {
			event := &events.Items[i]
			if event.InvolvedObject.Name == pendingPod.Name &&
				strings.Contains(event.Message, "Insufficient nvidia.com/gpu") {
				observation.HasInsufficientEvent = true
				break
			}
		}

		if !options.Eligible(observation) {
			time.Sleep(options.PollInterval)
			continue
		}

		err = client.CoreV1().Pods(options.Namespace).Bind(tc.Ctx(), &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: pendingPod.Name, Namespace: options.Namespace},
			Target: corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       options.TargetNode,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("bind scheduler-blocked %s pod %s to %s: %w",
				options.Description, pendingPod.Name, options.TargetNode, err)
		}
		tc.Logf("bound scheduler-blocked %s pod %s to verified recovery node %s",
			options.Description, pendingPod.Name, options.TargetNode)
		return nil
	}

	return fmt.Errorf("%s scheduler recovery condition did not become eligible within %s",
		options.Description, options.Timeout)
}

func podGPURequest(pod *corev1.Pod, gpuName corev1.ResourceName) int64 {
	var regular int64
	for i := range pod.Spec.Containers {
		request := pod.Spec.Containers[i].Resources.Requests[gpuName]
		regular += request.Value()
	}
	var maxInit int64
	for i := range pod.Spec.InitContainers {
		request := pod.Spec.InitContainers[i].Resources.Requests[gpuName]
		if value := request.Value(); value > maxInit {
			maxInit = value
		}
	}
	if maxInit > regular {
		return maxInit
	}
	return regular
}
