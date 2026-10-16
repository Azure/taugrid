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

func TestMain(m *testing.M) {
	queuePolicyPresent = func(context.Context, clusterUninstallSpec, baselineQueuePolicy) (bool, error) {
		return false, errors.New("queuePolicyPresent must be stubbed with stubQueuePolicyPresent in tests that reach a successful drain")
	}
	os.Exit(m.Run())
}
