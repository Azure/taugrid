// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kuberay contains e2e tests for the KubeRay operator.
//
// Tests verify that the KubeRay Helm chart deploys correctly and that
// RayClusters can be created and reach a healthy running state.
//
// TestMain sets up a shared namespace for RayCluster tests and tears it
// down at the end. Each test is fully independent — it creates and cleans
// up its own RayCluster resources.
package kuberay

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	kuberayNamespace          = "kuberay-system"
	kuberayOperatorDeployment = "kuberay-operator"
	testNamespace             = "e2e-kuberay"
	testRayClusterName        = "e2e-raycluster"
)

func TestMain(m *testing.M) {
	// Skip all setup if e2e tests are not enabled
	if os.Getenv("AI_RUNTIME_E2E") != "1" {
		os.Exit(m.Run())
	}

	os.Exit(runTests(m))
}

func runTests(m *testing.M) (code int) {
	kubeClient, dynamicClient, err := e2e.BuildClients()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build K8s clients: %v\n", err)
		return 1
	}

	ctx := context.Background()

	if err := e2e.ApplyFixtureWithClient(ctx, dynamicClient, "raycluster.yaml"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup kuberay test resources: %v\n", err)
		e2e.DumpDeploymentDiagnostics(ctx, kubeClient, kuberayNamespace, kuberayOperatorDeployment)
		return 1
	}
	defer e2e.DeleteFixtureWithClient(ctx, dynamicClient, "raycluster.yaml")
	defer results.FlushAll()

	return m.Run()
}

func TestRayClusterRunning(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	// CR-specific diagnostics on failure (controller logs are auto-captured by NewTestContext).
	tc.OnFailure(func() {
		tc.DumpPods(testNamespace, "")
		tc.DumpEvents(testNamespace)
		tc.DumpCRState(testNamespace, e2e.RayClusterGVR, testRayClusterName)
	})

	// RayCluster is created by TestMain — wait for pods to be Running

	// Wait for the head pod to be Running and Ready.
	err := tc.WaitForRunningPodsByLabel(testNamespace, "ray.io/node-type=head", 1, 3*time.Minute)
	require.NoError(t, err, "RayCluster should have 1 running head pod")

	// Wait for the worker pod to be Running and Ready (needs GCS connection to head).
	err = tc.WaitForRunningPodsByLabel(testNamespace, "ray.io/node-type=worker", 1, 3*time.Minute)
	require.NoError(t, err, "RayCluster should have 1 running worker pod")
}

// TestRayDataProgressBarsDisabled verifies that the operator injects
// RAY_DATA_DISABLE_PROGRESS_BARS=1 into Ray containers via defaultContainerEnvs.
func TestRayDataProgressBarsDisabled(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	tc.OnFailure(func() {
		tc.DumpPods(testNamespace, "")
	})

	for _, selector := range []string{"ray.io/node-type=head", "ray.io/node-type=worker"} {
		err := tc.WaitForRunningPodsByLabel(testNamespace, selector, 1, 3*time.Minute)
		require.NoError(t, err, "waiting for pod with selector %s", selector)

		pods, err := tc.KubeClient().CoreV1().Pods(testNamespace).List(
			tc.Ctx(), metav1.ListOptions{LabelSelector: selector},
		)
		require.NoError(t, err)
		require.Len(t, pods.Items, 1)

		pod := pods.Items[0]
		found := false
		for _, c := range pod.Spec.Containers {
			for _, env := range c.Env {
				if env.Name == "RAY_DATA_DISABLE_PROGRESS_BARS" {
					require.Equal(t, "1", env.Value,
						"RAY_DATA_DISABLE_PROGRESS_BARS should be '1' in container %s of pod %s", c.Name, pod.Name)
					found = true
				}
			}
		}
		require.True(t, found,
			"RAY_DATA_DISABLE_PROGRESS_BARS env var not found in any container of pod %s (selector: %s)", pod.Name, selector)
	}
}
