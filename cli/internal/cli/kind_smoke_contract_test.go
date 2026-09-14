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

func TestKindSmokeExercisesRayServiceAndLegacyCRDUpgrade(t *testing.T) {
	script, err := os.ReadFile("../../scripts/kind-smoke-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"crd/rayservices.ray.io",
		"TestRenderKindRayServiceFixture",
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
