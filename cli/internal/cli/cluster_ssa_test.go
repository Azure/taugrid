// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"io"
	"testing"
)

func stubRollbackOnFailure(t *testing.T, supported bool) {
	t.Helper()
	original := helmSupportsRollbackOnFailure
	helmSupportsRollbackOnFailure = func() bool { return supported }
	t.Cleanup(func() { helmSupportsRollbackOnFailure = original })
}

func TestClusterHelmUpgradesNeverForceOwnershipConflicts(t *testing.T) {
	for name, args := range map[string][]string{
		"install":         {"install", "--mode", "helm", "--chart", "./charts/taugrid"},
		"uninstall drain": {"uninstall", "--yes", "--chart", "./charts/taugrid"},
	} {
		t.Run(name, func(t *testing.T) {
			var upgrades [][]string
			recordUpgrades(t, args[0], &upgrades)

			if _, err := runCluster(t, args...); err != nil {
				t.Fatalf("%s errored: %v", name, err)
			}
			if len(upgrades) == 0 {
				t.Fatalf("%s ran no helm upgrade", name)
			}
			for i, got := range upgrades {
				if containsArg(got, "--force-conflicts") || containsArg(got, "--server-side=true") {
					t.Fatalf("%s upgrade[%d] must not force Kubernetes ownership conflicts: %#v", name, i, got)
				}
			}
		})
	}
}

func TestHelmRollbackOnFailureProbeReadsUpgradeHelp(t *testing.T) {
	for name, tc := range map[string]struct {
		help string
		want bool
	}{
		"helm 4 lists the flag": {"      --rollback-on-failure   rollback the release on failure", true},
		"helm 3 does not":       {"      --atomic   rollback the release on failure", false},
	} {
		t.Run(name, func(t *testing.T) {
			stubHelmUpgradeHelp(t, []byte(tc.help), nil)
			if got := probeHelmRollbackOnFailure(t.Context()); got != tc.want {
				t.Fatalf("probe = %v, want %v", got, tc.want)
			}
		})
	}
}

func stubHelmUpgradeHelp(t *testing.T, output []byte, err error) {
	t.Helper()
	original := helmUpgradeHelp
	helmUpgradeHelp = func(context.Context) ([]byte, error) { return output, err }
	t.Cleanup(func() { helmUpgradeHelp = original })
}

func recordUpgrades(t *testing.T, verb string, upgrades *[][]string) {
	t.Helper()
	capture := func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		if args[0] == "upgrade" {
			*upgrades = append(*upgrades, append([]string(nil), args...))
		}
		return nil
	}
	if verb == "uninstall" {
		installFakeHelmWithRelease(t, installedRelease(queueEnabledValues, deployedMetadata), capture)
		return
	}
	installFakeHelm(t, capture)
}
