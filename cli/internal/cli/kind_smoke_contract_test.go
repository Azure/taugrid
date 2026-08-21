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
