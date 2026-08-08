// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/internal/taukeys"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSingleDevicePreferredAffinityPacksOntoOccupiedNode(t *testing.T) {
	runPreferredAffinityPacksOntoOccupiedNode(t, "e2e-scheduler-single-packing",
		map[string]string{
			"app":                      "tau-single-device-seed",
			taukeys.LabelGPUCount:      "1",
			taukeys.LabelGPUPlacement:  "single-device",
			taukeys.LabelSchedulerTest: "packing-affinity",
		},
		map[string]string{
			"app":                      "tau-single-device-target",
			taukeys.LabelGPUCount:      "1",
			taukeys.LabelGPUPlacement:  "single-device",
			taukeys.LabelSchedulerTest: "packing-affinity",
		},
		"preferred pod affinity should pack single-device Tau GPU pods")
}

func TestSmallSameNodeMultiGPUPreferredAffinityPacksOntoOccupiedNode(t *testing.T) {
	runPreferredAffinityPacksOntoOccupiedNode(t, "e2e-scheduler-samenode-packing",
		map[string]string{
			"app":                      "tau-samenode-seed",
			taukeys.LabelGPUCount:      "2",
			taukeys.LabelGPUPlacement:  "same-node",
			taukeys.LabelSchedulerTest: "packing-affinity",
		},
		map[string]string{
			"app":                      "tau-samenode-target",
			taukeys.LabelGPUCount:      "4",
			taukeys.LabelGPUPlacement:  "same-node",
			taukeys.LabelSchedulerTest: "packing-affinity",
		},
		"preferred pod affinity should pack same-node 2-4 GPU Tau pods")
}

func runPreferredAffinityPacksOntoOccupiedNode(t *testing.T, namespace string, seedLabels, targetLabels map[string]string, failureMessage string) {
	t.Helper()
	tc := e2e.NewTestContext(t, context.Background())
	nodes := schedulableNodes(t, tc)
	if len(nodes) < 2 {
		t.Skip("requires at least two schedulable nodes; for kind, create a control-plane + 2 worker cluster")
	}
	seedNode := nodes[0].Name
	otherNode := nodes[1].Name
	t.Logf("using seed node %s; alternate schedulable node %s", seedNode, otherNode)

	createNamespace(t, tc, namespace)
	t.Cleanup(func() {
		_ = tc.KubeClient().CoreV1().Namespaces().Delete(tc.Ctx(), namespace, metav1.DeleteOptions{})
	})

	seed := pausePod("seed", seedLabels)
	seed.Spec.NodeName = seedNode
	if _, err := tc.KubeClient().CoreV1().Pods(namespace).Create(tc.Ctx(), seed, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create seed pod: %v", err)
	}

	target := pausePod("target", targetLabels)
	target.Spec.Affinity = gpuBinPackingAffinity()
	if _, err := tc.KubeClient().CoreV1().Pods(namespace).Create(tc.Ctx(), target, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create target pod: %v", err)
	}

	scheduled, err := waitForPodScheduled(tc, namespace, "target", 45*time.Second)
	if err != nil {
		tc.DumpPods(namespace, "")
		tc.DumpEvents(namespace)
		t.Fatal(err)
	}
	if scheduled.Spec.NodeName != seedNode {
		tc.DumpPods(namespace, "")
		t.Fatalf("target scheduled on %s, want seed node %s; %s", scheduled.Spec.NodeName, seedNode, failureMessage)
	}
}

func createNamespace(t *testing.T, tc *e2e.TestContext, name string) {
	t.Helper()
	_, err := tc.KubeClient().CoreV1().Namespaces().Create(tc.Ctx(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

func schedulableNodes(t *testing.T, tc *e2e.TestContext) []corev1.Node {
	t.Helper()
	nodes, err := tc.KubeClient().CoreV1().Nodes().List(tc.Ctx(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var out []corev1.Node
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable || hasBlockingTaint(node) {
			continue
		}
		out = append(out, node)
	}
	return out
}

func hasBlockingTaint(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func pausePod(name string, labels map[string]string) *corev1.Pod {
	zero := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: &zero,
			Containers: []corev1.Container{
				{Name: "pause", Image: "registry.k8s.io/pause:3.10"},
			},
		},
	}
}

func gpuBinPackingAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: "kubernetes.io/hostname",
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      taukeys.LabelGPUCount,
									Operator: metav1.LabelSelectorOpExists,
								},
								{
									Key:      taukeys.LabelGPUPlacement,
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"single-device", "same-node"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func waitForPodScheduled(tc *e2e.TestContext, namespace, name string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for pod %s/%s to be scheduled", namespace, name)
		case <-tc.Ctx().Done():
			return nil, tc.Ctx().Err()
		case <-ticker.C:
			pod, err := tc.KubeClient().CoreV1().Pods(namespace).Get(tc.Ctx(), name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if pod.Spec.NodeName != "" {
				return pod, nil
			}
		}
	}
}
