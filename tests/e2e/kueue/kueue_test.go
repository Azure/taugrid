// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kueue

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	kueueNamespace            = "kueue-system"
	kueueControllerDeployment = "kueue-controller-manager"
	testNamespace             = "e2e-kueue"
	testLocalQueue            = "e2e-queue"
)

func TestMain(m *testing.M) {
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

	if err := e2e.ApplyFixtureWithClient(ctx, dynamicClient, "kueue-resources.yaml"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup Kueue resources: %v\n", err)
		e2e.DumpDeploymentDiagnostics(ctx, kubeClient, kueueNamespace, kueueControllerDeployment)
		return 1
	}
	defer e2e.DeleteFixtureWithClient(ctx, dynamicClient, "kueue-resources.yaml")
	defer results.FlushAll()

	return m.Run()
}

func registerKueueDiagnostics(tc *e2e.TestContext) {
	tc.Helper()
	tc.OnFailure(func() {
		tc.DumpPods(testNamespace, "")
		tc.DumpEvents(testNamespace)
		tc.DumpWorkloads(testNamespace)
		tc.DumpLocalQueues(testNamespace)
	})
}

func TestGangSchedulingBlocked(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	registerKueueDiagnostics(tc)

	tc.ApplyFixture(t, "job-gang-blocked.yaml")

	result, err := tc.WaitForWorkloadQuotaNotReserved(testNamespace, "e2e-gang-blocked", 10*time.Second)
	require.NoError(t, err, "workload should be pending (quota exceeded)")
	assert.Contains(t, result.Reason, "Pending", "should be pending due to quota exceeded")
	assert.Contains(t, result.Message, "insufficient quota",
		"should be blocked by insufficient total quota, not unused quota")

	err = tc.WaitForLocalQueueCounts(testNamespace, testLocalQueue, 1, 0, 10*time.Second)
	require.NoError(t, err, "localqueue should have 1 pending, 0 admitted")

	err = tc.WaitForRunningJobPods(testNamespace, "e2e-gang-blocked", 0, 10*time.Second)
	require.NoError(t, err, "blocked job should have zero running pods")
}

func TestGangSchedulingAdmitted(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	registerKueueDiagnostics(tc)

	tc.ApplyFixture(t, "job-gang-fits.yaml")

	_, err := tc.WaitForWorkloadAdmitted(testNamespace, "e2e-gang-fits", 20*time.Second)
	require.NoError(t, err, "workload should be admitted")

	err = tc.WaitForRunningJobPods(testNamespace, "e2e-gang-fits", 2, 40*time.Second)
	require.NoError(t, err, "admitted job should have 2 running pods")

	tc.ApplyFixture(t, "job-gang-pending.yaml")

	result, err := tc.WaitForWorkloadQuotaNotReserved(testNamespace, "e2e-gang-pending", 10*time.Second)
	require.NoError(t, err, "second workload should be pending (insufficient unused quota)")
	assert.Contains(t, result.Reason, "Pending", "should be pending due to insufficient unused quota")
	assert.Contains(t, result.Message, "insufficient unused quota",
		"should be blocked by insufficient unused quota, not total capacity")

	err = tc.WaitForLocalQueueCounts(testNamespace, testLocalQueue, 1, 1, 10*time.Second)
	require.NoError(t, err, "localqueue should have 1 pending, 1 admitted")
}
