// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kueueray contains Kueue and KubeRay interoperability tests.
package kueueray

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	testNamespace    = "e2e-kueue-kuberay-admitted"
	testClusterQueue = "e2e-kueue-kuberay-admitted-cq"
	testRayJob       = "e2e-rayjob-admitted"
)

func TestMain(m *testing.M) {
	code := m.Run()
	results.FlushAll()
	os.Exit(code)
}

func TestAdmittedRayJobRunsToCompletion(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	tc.ApplyFixture(t, "rayjob-admitted.yaml")

	admission, err := tc.WaitForWorkloadAdmittedByRayJob(testNamespace, testRayJob, 30*time.Second)
	require.NoError(t, err)
	clusterQueue, _, err := unstructured.NestedString(
		admission.Workload.Object, "status", "admission", "clusterQueue",
	)
	require.NoError(t, err)
	require.Equal(t, testClusterQueue, clusterQueue)

	var rayCluster string
	require.Eventually(t, func() bool {
		rayJob, err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(testNamespace).
			Get(tc.Ctx(), testRayJob, metav1.GetOptions{})
		if err != nil {
			return false
		}
		rayCluster, _, _ = unstructured.NestedString(rayJob.Object, "status", "rayClusterName")
		return rayCluster != ""
	}, 3*time.Minute, time.Second)

	for _, nodeType := range []string{"head", "worker"} {
		require.NoError(t, tc.WaitForRunningPodsByLabel(
			testNamespace,
			fmt.Sprintf("ray.io/cluster=%s,ray.io/node-type=%s", rayCluster, nodeType),
			1,
			10*time.Minute,
		))
	}
	require.NoError(t, tc.WaitForRayJobStatus(testNamespace, testRayJob, "SUCCEEDED", 5*time.Minute))
}
