// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/kube"
)

// KubeRay tears down the ray-head container when a RayJob reaches a terminal
// state. `tau run logs` used to exec `ray job logs` inside that container, so
// reading the output of a run that merely SUCCEEDED failed with
// `container not found ("ray-head")` -- i.e. it broke on the normal end state
// of every healthy run.
//
// These tests pin the fallback to the log-offload sidecar, which outlives the
// ray-head container and holds the same driver output.

// fakeKubectlRunner writes a POSIX shell stub at Runner.Path so rayJobLogs
// exercises its real argv construction rather than a mocked seam.
func fakeKubectlRunner(t *testing.T, script string) *kube.Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &kube.Runner{Path: path}
}

const driverOutputLine = "step 199 loss 0.032720"

// headResolutionCases answers the jobId / rayClusterName / head-pod lookups that
// precede the log read, so each test below differs only in log behaviour.
const headResolutionCases = `
  *status.jobId*)          printf '%s' 'raysubmit_abc123' ;;
  *status.rayClusterName*) printf '%s' 'demo-train-tzqst' ;;
  *"get pods"*)            printf '%s' 'demo-train-tzqst-head-v7ctq' ;;
`

func TestRayJobLogsFallsBackToSidecarWhenHeadContainerGone(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sidecar-was-read")
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *exec*)
    echo 'error: unable to upgrade connection: container not found ("ray-head")' >&2
    exit 1 ;;
  *`+raylogoffload.SidecarContainerName+`*)
    : > `+marker+`
    printf '%s\n' '`+driverOutputLine+`' ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false)
	if err != nil {
		t.Fatalf("expected sidecar fallback to succeed, got: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("fallback never read the %s sidecar", raylogoffload.SidecarContainerName)
	}
	if !strings.Contains(out, driverOutputLine) {
		t.Fatalf("output = %q, want it to contain %q", out, driverOutputLine)
	}
}

func TestRayJobLogsPrefersExecAndDoesNotReadSidecarWhenHeadAlive(t *testing.T) {
	// Negative control for the test above: if the sidecar were read
	// unconditionally, that test would pass even with the precedence inverted.
	marker := filepath.Join(t.TempDir(), "sidecar-was-read")
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *exec*)  printf '%s\n' 'live streaming output' ;;
  *`+raylogoffload.SidecarContainerName+`*)
    : > `+marker+`
    printf '%s\n' 'sidecar output that must not be used' ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("sidecar was read even though exec succeeded; precedence is inverted")
	}
	if !strings.Contains(out, "live streaming output") {
		t.Fatalf("output = %q, want the exec output", out)
	}
}

func TestRayJobLogsSurfacesBothErrorsWhenSidecarAlsoFails(t *testing.T) {
	// A fallback that swallows the original error is worse than no fallback:
	// the researcher loses the cause. Both must appear.
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *) echo 'boom' >&2; exit 1 ;;
esac
`)

	_, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false)
	if err == nil {
		t.Fatal("expected an error when both the exec and the sidecar fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ray job logs:") {
		t.Errorf("error dropped the original exec failure: %v", err)
	}
	if !strings.Contains(msg, raylogoffload.SidecarContainerName) {
		t.Errorf("error does not name the sidecar fallback: %v", err)
	}
}
