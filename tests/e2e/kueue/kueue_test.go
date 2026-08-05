// Package kueue contains e2e tests for the Kueue scheduling system.
//
// Tests verify that the Kueue Helm chart deploys correctly and that
// gang scheduling works as expected (all-or-nothing pod admission).
//
// TestMain sets up shared Kueue resources (ResourceFlavor, ClusterQueue,
// LocalQueue) once for all tests and tears them down at the end.
// Each test is fully independent — it creates and cleans up its own Jobs.
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

	if err := e2e.ApplyFixtureWithClient(ctx, dynamicClient, "kueue-resources.yaml"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup Kueue resources: %v\n", err)
		e2e.DumpDeploymentDiagnostics(ctx, kubeClient, kueueNamespace, kueueControllerDeployment)
		return 1
	}
	defer e2e.DeleteFixtureWithClient(ctx, dynamicClient, "kueue-resources.yaml")
	defer results.FlushAll()

	return m.Run()
}

// --- Tests ---

// registerKueueDiagnostics registers an OnFailure callback that dumps the test
// namespace's pods, events, workloads, and local queues. This ensures assertion
// failures outside WaitFor* calls still produce namespace-level diagnostics.
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

	// Apply the "blocked" job: 2 pods × 2 CPU = 4 CPU > 3 CPU quota (can never fit)
	tc.ApplyFixture(t, "job-gang-blocked.yaml")

	// Kueue should NOT reserve quota — total request exceeds maximum capacity
	result, err := tc.WaitForWorkloadQuotaNotReserved(testNamespace, "e2e-gang-blocked", 10*time.Second)
	require.NoError(t, err, "workload should be pending (quota exceeded)")
	assert.Contains(t, result.Reason, "Pending", "should be pending due to quota exceeded")
	assert.Contains(t, result.Message, "insufficient quota",
		"should be blocked by insufficient total quota, not unused quota")

	// Queue should show 1 pending, 0 admitted.
	// Poll because the LocalQueue counter reconciles asynchronously after the Workload status.
	err = tc.WaitForLocalQueueCounts(testNamespace, testLocalQueue, 1, 0, 10*time.Second)
	require.NoError(t, err, "localqueue should have 1 pending, 0 admitted")

	// Zero pods running (gang scheduling = all-or-nothing)
	err = tc.WaitForRunningJobPods(testNamespace, "e2e-gang-blocked", 0, 10*time.Second)
	require.NoError(t, err, "blocked job should have zero running pods")
}

func TestGangSchedulingAdmitted(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	registerKueueDiagnostics(tc)

	// Apply the "fits" job: 2 pods × 1 CPU = 2 CPU ≤ 3 CPU quota (leaves 1 CPU free)
	tc.ApplyFixture(t, "job-gang-fits.yaml")

	// Kueue should admit this workload
	_, err := tc.WaitForWorkloadAdmitted(testNamespace, "e2e-gang-fits", 20*time.Second)
	require.NoError(t, err, "workload should be admitted")

	// Both pods should be running (gang scheduling = both appear together)
	err = tc.WaitForRunningJobPods(testNamespace, "e2e-gang-fits", 2, 40*time.Second)
	require.NoError(t, err, "admitted job should have 2 running pods")

	// Now apply a second job that COULD fit in an empty queue (2 CPU ≤ 3 CPU)
	// but can't because only 1 CPU is unused (the "fits" job is using 2 CPU).
	tc.ApplyFixture(t, "job-gang-pending.yaml")

	result, err := tc.WaitForWorkloadQuotaNotReserved(testNamespace, "e2e-gang-pending", 10*time.Second)
	require.NoError(t, err, "second workload should be pending (insufficient unused quota)")
	assert.Contains(t, result.Reason, "Pending", "should be pending due to insufficient unused quota")
	assert.Contains(t, result.Message, "insufficient unused quota",
		"should be blocked by insufficient unused quota, not total capacity")

	// Queue should show 1 pending (the second job), 1 admitted (the first job).
	// Poll because the LocalQueue counter reconciles asynchronously after the Workload status.
	err = tc.WaitForLocalQueueCounts(testNamespace, testLocalQueue, 1, 1, 10*time.Second)
	require.NoError(t, err, "localqueue should have 1 pending, 1 admitted")
}
