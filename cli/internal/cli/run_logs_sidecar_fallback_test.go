// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
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

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, -1)
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

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, -1)
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

	_, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, -1)
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

func TestRayJobLogsUsesSidecarForFiniteTail(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *"`+raylogoffload.SidecarContainerName+` --tail=2"*) printf 'second\nthird\n' ;;
  *exec*) echo 'finite tail unexpectedly used ray job logs' >&2; exit 4 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, 2)
	if err != nil {
		t.Fatalf("expected sidecar tail to succeed, got: %v", err)
	}
	if out != "second\nthird\n" {
		t.Fatalf("output = %q, want final two lines", out)
	}
}

func TestRayJobLogsReturnsPartialOutputWhenAllReadsFail(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *`+raylogoffload.SidecarContainerName+`*) printf 'sidecar partial\n'; echo 'sidecar failed' >&2; exit 1 ;;
  *exec*) printf 'ray cli partial\n'; echo 'ray cli failed' >&2; exit 1 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, 20)
	if err == nil {
		t.Fatal("expected both reads to fail")
	}
	if out != "ray cli partial\n" {
		t.Fatalf("partial output = %q", out)
	}
	for _, want := range []string{"sidecar failed", "ray cli failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRayJobLogsReturnsSidecarPartialOutputWhenRayCLIFailsEmpty(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *`+raylogoffload.SidecarContainerName+`*) printf 'sidecar partial\n'; echo 'sidecar failed' >&2; exit 1 ;;
  *exec*) echo 'ray cli failed' >&2; exit 1 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, 20)
	if err == nil {
		t.Fatal("expected both reads to fail")
	}
	if out != "sidecar partial\n" {
		t.Fatalf("partial output = %q", out)
	}
}

func TestResolveRayJobLogTargetIdentifiesUnreadyRayJob(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in
  *status.jobId*) exit 0 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)
	_, _, err := resolveRayJobLogTarget(context.Background(), r, "ray", "demo-train")
	if !errors.Is(err, errRayJobNotReady) {
		t.Fatalf("error = %v, want errRayJobNotReady", err)
	}
	if !strings.Contains(err.Error(), "status.jobId") {
		t.Fatalf("error %q does not explain RayJob readiness", err)
	}
}

func TestRayJobFollowStreamsSidecarFromTailZero(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *"`+raylogoffload.SidecarContainerName+` --tail=0 -f"*) printf 'new line\n' ;;
  *exec*) echo 'follow unexpectedly used ray job logs' >&2; exit 4 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	var out strings.Builder
	if err := rayJobFollow(context.Background(), r, "pre-training-document", "demo-train", 0, &out); err != nil {
		t.Fatalf("expected sidecar follow to succeed, got: %v", err)
	}
	if got := out.String(); got != "new line\n" {
		t.Fatalf("output = %q, want only newly followed output", got)
	}
}

func TestRayJobFollowRejectsBoundedLegacyFallback(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *`+raylogoffload.SidecarContainerName+`*) echo 'container not found' >&2; exit 1 ;;
  *exec*) echo 'bounded follow must not discard its tail contract' >&2; exit 4 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	err := rayJobFollow(context.Background(), r, "pre-training-document", "demo-train", 20, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "retry with --tail=-1") {
		t.Fatalf("error = %v, want legacy bounded-follow guidance", err)
	}
}

func TestRayJobFollowSurfacesSidecarAndRayCLIErrors(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *`+raylogoffload.SidecarContainerName+`*) echo 'sidecar stream failed' >&2; exit 1 ;;
  *exec*) echo 'ray cli stream failed' >&2; exit 1 ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	err := rayJobFollow(context.Background(), r, "pre-training-document", "demo-train", -1, &strings.Builder{})
	if err == nil {
		t.Fatal("expected an error when both follow paths fail")
	}
	for _, want := range []string{"sidecar stream failed", "ray cli stream failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRayJobLogsTailsLegacyExecOutput(t *testing.T) {
	r := fakeKubectlRunner(t, `#!/bin/sh
case "$*" in`+headResolutionCases+`
  *`+raylogoffload.SidecarContainerName+`*) echo 'container not found' >&2; exit 1 ;;
  *exec*) printf 'first\nsecond\nthird\n' ;;
  *) echo "unexpected: $*" >&2; exit 3 ;;
esac
`)

	out, err := rayJobLogs(context.Background(), r, "pre-training-document", "demo-train", false, 2)
	if err != nil {
		t.Fatalf("expected legacy Ray CLI fallback to succeed, got: %v", err)
	}
	if out != "second\nthird\n" {
		t.Fatalf("output = %q, want final two lines", out)
	}
}

func TestTailLogOutputPreservesTrailingNewlineContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		logs string
		tail int
		want string
	}{
		{name: "all", logs: "one\ntwo\n", tail: -1, want: "one\ntwo\n"},
		{name: "zero", logs: "one\ntwo\n", tail: 0, want: ""},
		{name: "bounded with newline", logs: "one\ntwo\nthree\n", tail: 2, want: "two\nthree\n"},
		{name: "bounded without newline", logs: "one\ntwo\nthree", tail: 2, want: "two\nthree"},
		{name: "short", logs: "one\n", tail: 2, want: "one\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailLogOutput(tc.logs, tc.tail); got != tc.want {
				t.Fatalf("tailLogOutput(%q, %d) = %q, want %q", tc.logs, tc.tail, got, tc.want)
			}
		})
	}
}

func TestLineTailWriterBoundsCompleteLinesAcrossWrites(t *testing.T) {
	writer := &lineTailWriter{limit: 2}
	for _, chunk := range []string{"first\nsec", "ond\nthird", "\nfourth"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.String(); got != "third\nfourth" {
		t.Fatalf("output = %q, want final two lines", got)
	}
}

func TestLineTailWriterHandlesLargeUnterminatedLineIncrementally(t *testing.T) {
	writer := &lineTailWriter{limit: 2}
	for range 1000 {
		if _, err := writer.Write([]byte("chunk")); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.String(); len(got) != 5000 {
		t.Fatalf("output length = %d, want 5000", len(got))
	}
}
