// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

var errUnreadableRelease = errors.New("release not readable")

const (
	queueEnabledValues = `{"baselineQueue":{"enabled":true}}`
	deployedMetadata   = `{"version":"0.1.0"}`
)

func installedRelease(values, metadata string) releaseFixture {
	return releaseFixture{Release: defaultTauGridRelease, Values: values, Metadata: metadata}
}

// recordHelm captures every mutating Helm invocation in order.
func recordHelm(calls *[][]string) helmCommandRunner {
	return func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		*calls = append(*calls, append([]string(nil), args...))
		return nil
	}
}

// The bug: Helm's UninstallOrder deletes the Kueue controller Deployment before
// the Kueue custom resources, so nothing is left to clear their
// resource-in-use finalizers and `helm uninstall --wait` blocks until timeout.
// Draining the queue policy first, while the controller still runs, removes
// those objects from the release manifest entirely.
func TestClusterUninstallDrainsQueueBeforeUninstall(t *testing.T) {
	stubForceConflicts(t, false)
	var calls [][]string
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata), recordHelm(&calls))

	out, err := runCluster(t, "uninstall", "--context", "aks-dev", "--yes")
	if err != nil {
		t.Fatalf("uninstall errored: %v\n%s", err, out)
	}
	if len(calls) != 2 {
		t.Fatalf("mutating Helm calls = %d, want drain then uninstall: %#v", len(calls), calls)
	}

	wantDrain := []string{
		"upgrade", defaultTauGridRelease, defaultTauGridChart,
		"--namespace", defaultTauGridNamespace,
		"--version", "0.1.0",
		"--timeout", "15m",
		"--reuse-values",
		"--set", "baselineQueue.enabled=false",
		"--kube-context", "aks-dev",
	}
	if !reflect.DeepEqual(calls[0], wantDrain) {
		t.Fatalf("drain args:\n got: %#v\nwant: %#v", calls[0], wantDrain)
	}
	if calls[1][0] != "uninstall" {
		t.Fatalf("second call = %#v, want uninstall after drain", calls[1])
	}
}

func TestClusterUninstallExposesWaitAndTimeoutEscapeHatch(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata), recordHelm(&calls))

	if _, err := runCluster(t,
		"uninstall",
		"--yes",
		"--drain-queue=false",
		"--wait=false",
		"--timeout", "45s",
	); err != nil {
		t.Fatalf("uninstall errored: %v", err)
	}
	if len(calls) != 1 || calls[0][0] != "uninstall" {
		t.Fatalf("Helm calls = %#v, want uninstall only", calls)
	}
	if containsArg(calls[0], "--wait") {
		t.Fatalf("--wait=false still passed --wait: %#v", calls[0])
	}
	if !containsArgPair(calls[0], "--timeout", "45s") {
		t.Fatalf("custom timeout missing: %#v", calls[0])
	}
}

// Each of these flags turns the drain into a different bug: --install would
// install the chart during an uninstall, --atomic would roll back and recreate
// the queue just deleted, and --wait blocks on the new resource set rather than
// on the deletions, so it only costs time on the unhealthy clusters people are
// actually uninstalling.
func TestClusterUninstallDrainOmitsDangerousFlags(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata), recordHelm(&calls))

	if _, err := runCluster(t, "uninstall", "--yes"); err != nil {
		t.Fatalf("uninstall errored: %v", err)
	}
	for _, flag := range []string{"--install", "--atomic", "--wait", "--reset-values", "--create-namespace"} {
		if containsArg(calls[0], flag) {
			t.Fatalf("drain args must not carry %s: %#v", flag, calls[0])
		}
	}
}

// Re-rendering the release from a different chart version would diff far more
// than the queue policy. Helm honours --version only when it resolves the chart
// from a repository; for a local --chart path the files on disk win, so this
// guards the repository case.
func TestClusterUninstallPassesDeployedChartVersion(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, `{"version":"0.0.9"}`), recordHelm(&calls))

	if _, err := runCluster(t, "uninstall", "--yes"); err != nil {
		t.Fatalf("uninstall errored: %v", err)
	}
	if !containsArgPair(calls[0], "--version", "0.0.9") {
		t.Fatalf("drain must pass the deployed chart version, got: %#v", calls[0])
	}
	if containsArgPair(calls[0], "--version", defaultTauGridChartVersion) {
		t.Fatalf("drain used the compiled-in version instead of the deployed one: %#v", calls[0])
	}
}

func TestClusterUninstallSkipsDrain(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture releaseFixture
		args    []string
	}{
		"queue already disabled": {
			fixture: installedRelease(`{"baselineQueue":{"enabled":false}}`, deployedMetadata),
		},
		"release absent": {
			fixture: releaseFixture{Release: "other", Values: queueEnabledValues, Metadata: deployedMetadata},
		},
		"deployed version unreadable": {
			fixture: installedRelease(queueEnabledValues, ""),
		},
		"operator opted out": {
			fixture: installedRelease(queueEnabledValues, deployedMetadata),
			args:    []string{"--drain-queue=false"},
		},
		"dry run": {
			fixture: installedRelease(queueEnabledValues, deployedMetadata),
			args:    []string{"--dry-run"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			installFakeHelmWithRelease(t, tc.fixture, recordHelm(&calls))

			if _, err := runCluster(t, append([]string{"uninstall", "--yes"}, tc.args...)...); err != nil {
				t.Fatalf("uninstall errored: %v", err)
			}
			if len(calls) != 1 || calls[0][0] != "uninstall" {
				t.Fatalf("want uninstall only, got: %#v", calls)
			}
		})
	}
}

// A drain that cannot run must never block teardown: the outcome is exactly
// today's behaviour, which the guidance text then explains.
func TestClusterUninstallContinuesWhenDrainFails(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata),
		func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
			calls = append(calls, append([]string(nil), args...))
			if args[0] == "upgrade" {
				return errors.New("registry login required")
			}
			return nil
		})

	out, err := runCluster(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("drain failure must not fail uninstall: %v\n%s", err, out)
	}
	if len(calls) != 2 || calls[1][0] != "uninstall" {
		t.Fatalf("uninstall must still run after a failed drain: %#v", calls)
	}
	if !strings.Contains(out, "registry login required") {
		t.Fatalf("drain failure must be reported:\n%s", out)
	}
}

func TestClusterUninstallReportsLeftoversOnFailure(t *testing.T) {
	installFakeHelmWithRelease(t, installedRelease(`{"baselineQueue":{"enabled":false}}`, deployedMetadata),
		func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
			return errors.New("context deadline exceeded")
		})

	out, err := runCluster(t, "uninstall", "--yes")
	if err == nil {
		t.Fatal("uninstall must still report the Helm failure")
	}
	for _, want := range []string{"jobqueue", "taugrid-default", "default-node-topology", "finalizers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("recovery guidance missing %q:\n%s", want, out)
		}
	}
}

// Guidance that names the chart defaults would send an operator with a renamed
// queue to objects that do not exist.
func TestClusterUninstallGuidanceUsesConfiguredQueueNames(t *testing.T) {
	values := `{"baselineQueue":{"enabled":false,"name":"research-cq","flavor":{"name":"gpu-flavor"},"topology":{"enabled":true,"name":"rack-topology"}}}`
	installFakeHelmWithRelease(t, installedRelease(values, deployedMetadata),
		func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
			return errors.New("context deadline exceeded")
		})

	out, _ := runCluster(t, "uninstall", "--yes")
	for _, want := range []string{"research-cq", "gpu-flavor", "rack-topology"} {
		if !strings.Contains(out, want) {
			t.Fatalf("recovery guidance missing configured name %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "jobqueue") {
		t.Fatalf("recovery guidance used chart defaults over configured names:\n%s", out)
	}
}

// A Topology is only rendered when baselineQueue.topology.enabled is set, so
// naming it unconditionally would tell operators to delete a nonexistent object.
func TestClusterUninstallGuidanceOmitsDisabledTopology(t *testing.T) {
	values := `{"baselineQueue":{"enabled":false,"topology":{"enabled":false,"name":"rack-topology"}}}`
	installFakeHelmWithRelease(t, installedRelease(values, deployedMetadata),
		func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
			return errors.New("context deadline exceeded")
		})

	out, _ := runCluster(t, "uninstall", "--yes")
	if strings.Contains(out, "rack-topology") {
		t.Fatalf("guidance named a Topology the chart never rendered:\n%s", out)
	}
}

// Skipping in silence would read as "there was no queue to drain" when the
// truth is "could not tell", and those leave the cluster in different states.
func TestClusterUninstallReportsSkipWhenValuesUnreadable(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t, releaseFixture{Release: defaultTauGridRelease, Metadata: deployedMetadata}, recordHelm(&calls))

	out, err := runCluster(t, "uninstall", "--yes")
	if err != nil {
		t.Fatalf("uninstall errored: %v\n%s", err, out)
	}
	if len(calls) != 1 || calls[0][0] != "uninstall" {
		t.Fatalf("unreadable values must skip the drain: %#v", calls)
	}
	if !strings.Contains(out, "skipping the queue drain") {
		t.Fatalf("a skipped drain must be reported:\n%s", out)
	}
}

// After a successful drain those objects are gone, so naming them would send
// the operator after resources that no longer exist.
func TestClusterUninstallOmitsRecoveryAfterSuccessfulDrain(t *testing.T) {
	installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata),
		func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
			if args[0] == "uninstall" {
				return errors.New("context deadline exceeded")
			}
			return nil
		})
	// A drain counts as done only once the objects are confirmed gone, so this
	// case has to say so rather than reach for a live cluster to find out.
	stubQueuePolicyPresent(t, false)

	out, err := runCluster(t, "uninstall", "--yes")
	if err == nil {
		t.Fatal("uninstall must still report the Helm failure")
	}
	if strings.Contains(out, "clear the stranded finalizers") {
		t.Fatalf("drained objects must not be named as leftovers:\n%s", out)
	}
}
