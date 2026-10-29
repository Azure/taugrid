// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"strings"
	"testing"
)

func TestKindSmokeProtectsKubeRayOperatorFromWorkloadPreemption(t *testing.T) {
	script, err := os.ReadFile("../../scripts/kind-smoke-e2e.sh")
	if err != nil {
		t.Fatalf("read kind smoke script: %v", err)
	}
	const priorityOverride = "--set kuberay-operator.priorityClassName=system-cluster-critical"
	if !strings.Contains(string(script), priorityOverride) {
		t.Fatalf("kind smoke install must include %q", priorityOverride)
	}
}

func TestKindSmokeWaitsForSeparateKueueRelease(t *testing.T) {
	script, err := os.ReadFile("../../scripts/kind-smoke-e2e.sh")
	if err != nil {
		t.Fatalf("read kind smoke script: %v", err)
	}
	for _, want := range []string{
		`KUEUE_RELEASE="${TAU_KIND_KUEUE_RELEASE:-taugrid-kueue}"`,
		`KUEUE_NAMESPACE="${TAU_KIND_KUEUE_NAMESPACE:-kueue-system}"`,
		`--kueue-release "$KUEUE_RELEASE"`,
		`-n "$KUEUE_NAMESPACE" rollout status`,
		`deploy/kueue-controller-manager --timeout=180s`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("runtime smoke missing separate Kueue release contract %q", want)
		}
	}
	if strings.Contains(string(script), `${TAUGRID_RELEASE}-kueue-controller-manager`) {
		t.Fatal("runtime smoke still assumes Kueue is an umbrella deployment")
	}
}

func TestKindSmokeExercisesRayServiceAndLegacyCRDUpgrade(t *testing.T) {
	script, err := os.ReadFile("../../scripts/kind-smoke-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"crd/rayservices.ray.io",
		"TestRenderKindRayServiceFixture",
		`KIND_NODE_TASKS_MAX="${TAU_KIND_NODE_TASKS_MAX:-4096}"`,
		`KIND_NODE_PIDS_LIMIT="${TAU_KIND_NODE_PIDS_LIMIT:-8192}"`,
		`"$CONTAINER_ENGINE" update --pids-limit "$KIND_NODE_PIDS_LIMIT"`,
		"wait_for_workload_admitted rayservice.ray.io",
		`--for=condition=Ready --timeout="$RAY_WAIT_TIMEOUT"`,
		`":9000/-/healthz"`,
		"os.statvfs",
		"http://tau-kind-serve-serve-svc:9000/",
		`get workspace.tau.azure.com kind-legacy -o jsonpath='{.metadata.uid}'`,
		`--type=merge -p='{"spec":{"role":"researcher"}}' --dry-run=server`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("runtime smoke missing %q", want)
		}
	}
}
