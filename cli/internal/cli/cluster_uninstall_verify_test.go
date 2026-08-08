// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func deployedMetadataFor(chart, version string) string {
	return `{"chart":"` + chart + `","version":"` + version + `"}`
}

// The drain is a real mutating `helm upgrade` that re-renders the whole release
// and, on Helm 4, forces field ownership. Running it from a chart other than
// the one the release was installed from would apply a different chart's
// manifests on the way out.
func TestClusterUninstallSkipsDrainOnChartMismatch(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t,
		installedRelease(queueEnabledValues, deployedMetadataFor("some-other-chart", "0.1.1")),
		recordHelm(&calls))

	out, err := runCluster(t, "uninstall", "--yes", "--chart", "./charts/taugrid")
	if err != nil {
		t.Fatalf("uninstall errored: %v\n%s", err, out)
	}
	if len(calls) != 1 || calls[0][0] != "uninstall" {
		t.Fatalf("a mismatched chart must skip the drain, got: %#v", calls)
	}
	for _, want := range []string{"some-other-chart", "taugrid"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mismatch report missing %q:\n%s", want, out)
		}
	}
}

// The deployed name is a bare chart name; --chart is a reference. Comparing
// them literally would reject every correct invocation.
func TestClusterUninstallDrainsWhenChartReferenceMatchesByName(t *testing.T) {
	for name, chart := range map[string]string{
		"local path":     "./charts/taugrid",
		"oci reference":  defaultTauGridChart,
		"trailing slash": "./charts/taugrid/",
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			installFakeHelmWithRelease(t,
				installedRelease(queueEnabledValues, deployedMetadataFor("taugrid", "0.1.1")),
				recordHelm(&calls))

			if _, err := runCluster(t, "uninstall", "--yes", "--chart", chart); err != nil {
				t.Fatalf("uninstall errored: %v", err)
			}
			if len(calls) != 2 || calls[0][0] != "upgrade" {
				t.Fatalf("%s must still drain, got: %#v", name, calls)
			}
		})
	}
}

// A release installed before this field was read has no chart name to compare
// against; refusing to drain there would regress every existing release.
func TestClusterUninstallDrainsWhenDeployedChartNameUnknown(t *testing.T) {
	var calls [][]string
	installFakeHelmWithRelease(t,
		installedRelease(queueEnabledValues, `{"version":"0.1.1"}`),
		recordHelm(&calls))

	if _, err := runCluster(t, "uninstall", "--yes", "--chart", "./charts/taugrid"); err != nil {
		t.Fatalf("uninstall errored: %v", err)
	}
	if len(calls) != 2 || calls[0][0] != "upgrade" {
		t.Fatalf("an unnamed deployed chart must not block the drain, got: %#v", calls)
	}
}

// The drain omits --wait, and Helm does not wait on resources it drops from a
// release, so it returns as soon as the deletes are accepted. On a cluster with
// active runs Kueue holds resource-in-use, the objects stay Terminating, and
// phase two then removes the controller underneath them — the exact state this
// command exists to prevent, except a claimed drain suppresses the recovery
// guidance that would tell the operator about it.
func TestClusterUninstallReportsLeftoversWhenDrainedObjectsRemain(t *testing.T) {
	installFakeHelmWithRelease(t,
		installedRelease(queueEnabledValues, deployedMetadataFor("taugrid", "0.1.1")),
		func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
			if args[0] == "uninstall" {
				return errors.New("context deadline exceeded")
			}
			return nil
		})
	stubQueuePolicyPresent(t, true)

	out, err := runCluster(t, "uninstall", "--yes", "--chart", "./charts/taugrid")
	if err == nil {
		t.Fatal("uninstall must still report the Helm failure")
	}
	if !strings.Contains(out, "clear the stranded finalizers") {
		t.Fatalf("objects that survived the drain must be reported as leftovers:\n%s", out)
	}
}

// When the drain really did remove them, naming them would send the operator
// after resources that no longer exist.
func TestClusterUninstallOmitsLeftoversWhenDrainedObjectsAreGone(t *testing.T) {
	installFakeHelmWithRelease(t,
		installedRelease(queueEnabledValues, deployedMetadataFor("taugrid", "0.1.1")),
		func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
			if args[0] == "uninstall" {
				return errors.New("context deadline exceeded")
			}
			return nil
		})
	stubQueuePolicyPresent(t, false)

	out, err := runCluster(t, "uninstall", "--yes", "--chart", "./charts/taugrid")
	if err == nil {
		t.Fatal("uninstall must still report the Helm failure")
	}
	if strings.Contains(out, "clear the stranded finalizers") {
		t.Fatalf("a confirmed drain must not name leftovers:\n%s", out)
	}
}

// A confirmation that cannot run must not upgrade a maybe into a yes.
func TestClusterUninstallReportsLeftoversWhenConfirmationFails(t *testing.T) {
	installFakeHelmWithRelease(t,
		installedRelease(queueEnabledValues, deployedMetadataFor("taugrid", "0.1.1")),
		func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
			if args[0] == "uninstall" {
				return errors.New("context deadline exceeded")
			}
			return nil
		})
	original := queuePolicyPresent
	queuePolicyPresent = func(context.Context, clusterUninstallSpec, baselineQueuePolicy) (bool, error) {
		return false, errors.New("cluster unreachable")
	}
	t.Cleanup(func() { queuePolicyPresent = original })

	out, _ := runCluster(t, "uninstall", "--yes", "--chart", "./charts/taugrid")
	if !strings.Contains(out, "clear the stranded finalizers") {
		t.Fatalf("an unverifiable drain must be treated as unconfirmed:\n%s", out)
	}
}

func stubQueuePolicyPresent(t *testing.T, present bool) {
	t.Helper()
	original := queuePolicyPresent
	queuePolicyPresent = func(context.Context, clusterUninstallSpec, baselineQueuePolicy) (bool, error) {
		return present, nil
	}
	t.Cleanup(func() { queuePolicyPresent = original })
}

// The confirmation shells to kubectl, so any test that drains successfully
// without stubbing it would query whatever cluster the runner happens to point
// at — slow, and green or red by accident. Failing here names the test instead.
func TestMain(m *testing.M) {
	queuePolicyPresent = func(context.Context, clusterUninstallSpec, baselineQueuePolicy) (bool, error) {
		return false, errors.New("queuePolicyPresent must be stubbed with stubQueuePolicyPresent in tests that reach a successful drain")
	}
	os.Exit(m.Run())
}
