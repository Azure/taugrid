// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// helmSupportsForceConflicts is a probe, so tests must be able to answer it
// without a real Helm on PATH.
func stubForceConflicts(t *testing.T, supported bool) {
	t.Helper()
	original := helmSupportsForceConflicts
	helmSupportsForceConflicts = func() bool { return supported }
	t.Cleanup(func() { helmSupportsForceConflicts = original })
}

func stubRollbackOnFailure(t *testing.T, supported bool) {
	t.Helper()
	original := helmSupportsRollbackOnFailure
	helmSupportsRollbackOnFailure = func() bool { return supported }
	t.Cleanup(func() { helmSupportsRollbackOnFailure = original })
}

// AKS's admissionsenforcer addon co-owns .webhooks[*].namespaceSelector on
// Kueue's webhook configurations. Under Helm 4's server-side apply that makes
// every upgrade of this release fail on field conflicts, which blocks both the
// install and the uninstall drain.
func TestClusterHelmUpgradesForceConflictsOnHelm4(t *testing.T) {
	for name, args := range map[string][]string{
		"install":         {"install", "--chart", "./charts/taugrid"},
		"uninstall drain": {"uninstall", "--yes", "--chart", "./charts/taugrid"},
	} {
		t.Run(name, func(t *testing.T) {
			stubForceConflicts(t, true)
			var upgrades [][]string
			recordUpgrades(t, args[0], &upgrades)

			if _, err := runCluster(t, args...); err != nil {
				t.Fatalf("%s errored: %v", name, err)
			}
			if len(upgrades) == 0 {
				t.Fatalf("%s ran no helm upgrade", name)
			}
			for i, got := range upgrades {
				if !containsArg(got, "--force-conflicts") {
					t.Fatalf("%s upgrade[%d] missing --force-conflicts: %#v", name, i, got)
				}
				// Helm rejects "forceConflicts enabled when serverSideApply
				// disabled", and --server-side defaults to auto, which inherits
				// the previous release's method. Sending them apart turns the
				// fix into a hard error on any release ever applied client-side.
				if !containsArg(got, "--server-side=true") {
					t.Fatalf("%s upgrade[%d] must pair --force-conflicts with --server-side=true: %#v", name, i, got)
				}
			}
		})
	}
}

// Helm 3 rejects the flag outright with "unknown flag", so passing it there
// would break every Helm 3 user to fix a Helm 4 one.
func TestClusterHelmOmitsForceConflictsOnHelm3(t *testing.T) {
	for name, args := range map[string][]string{
		"install":         {"install", "--chart", "./charts/taugrid"},
		"uninstall drain": {"uninstall", "--yes", "--chart", "./charts/taugrid"},
	} {
		t.Run(name, func(t *testing.T) {
			stubForceConflicts(t, false)
			var upgrades [][]string
			recordUpgrades(t, args[0], &upgrades)

			if _, err := runCluster(t, args...); err != nil {
				t.Fatalf("%s errored: %v", name, err)
			}
			if len(upgrades) == 0 {
				t.Fatalf("%s ran no helm upgrade", name)
			}
			for i, got := range upgrades {
				if containsArg(got, "--force-conflicts") {
					t.Fatalf("%s upgrade[%d] must not carry --force-conflicts on Helm 3: %#v", name, i, got)
				}
			}
		})
	}
}

// The probe asks Helm what it accepts rather than parsing `helm version`:
// version strings carry build suffixes and prereleases, while the flag's
// presence is the property that actually matters.
func TestHelmForceConflictsProbeReadsUpgradeHelp(t *testing.T) {
	for name, tc := range map[string]struct {
		help string
		want bool
	}{
		"helm 4 lists the flag": {"      --force-conflicts   if set server-side apply will force changes", true},
		"helm 3 does not":       {"      --force   force resource updates through replacement", false},
	} {
		t.Run(name, func(t *testing.T) {
			stubHelmBinary(t, "#!/bin/sh\ncat <<'EOF'\n"+tc.help+"\nEOF\n")
			if got := probeHelmForceConflicts(t.Context()); got != tc.want {
				t.Fatalf("probe = %v, want %v", got, tc.want)
			}
		})
	}
}

// A probe that cannot run must not add a flag Helm 3 would reject.
func TestHelmForceConflictsProbeFailsClosed(t *testing.T) {
	stubHelmBinary(t, "#!/bin/sh\nexit 1\n")
	if probeHelmForceConflicts(t.Context()) {
		t.Fatal("an unreadable probe must report no support")
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
			stubHelmBinary(t, "#!/bin/sh\ncat <<'EOF'\n"+tc.help+"\nEOF\n")
			if got := probeHelmRollbackOnFailure(t.Context()); got != tc.want {
				t.Fatalf("probe = %v, want %v", got, tc.want)
			}
		})
	}
}

// The probe shells out directly rather than through runHelmCommand, so it needs
// a real executable on PATH to exercise. The fake is prepended rather than
// replacing PATH, or its own shebang could not resolve the shell.
func stubHelmBinary(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake helm: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordUpgrades captures only `helm upgrade` invocations, so assertions stay
// free of the preflight reads each verb makes.
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
