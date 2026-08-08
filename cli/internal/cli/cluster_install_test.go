// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

func TestClusterInstallInvokesPinnedHelmRelease(t *testing.T) {
	stubForceConflicts(t, false)
	var got []string
	var upgrades int
	installFakeHelm(t, func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
		got = append([]string(nil), args...)
		upgrades++
		_, _ = io.WriteString(out, "helm ok\n")
		return nil
	})

	out, err := runCluster(t, "install",
		"--context", "aks-dev",
		"--values", "cluster.yaml",
		"--set", "baselineQueue.gpu.flavors[0].resources[0].nominalQuota=8",
		"--set-string", "kuberay-operator.labels.environment=dev")
	if err != nil {
		t.Fatalf("install errored: %v\n%s", err, out)
	}
	want := []string{
		"upgrade", "--install", defaultTauGridRelease, defaultTauGridChart,
		"--namespace", defaultTauGridNamespace,
		"--version", defaultTauGridChartVersion,
		"--timeout", "15m",
		"--reset-values",
		"--kube-context", "aks-dev",
		"--create-namespace",
		"--dependency-update",
		"--values", "cluster.yaml",
		"--set", "baselineQueue.gpu.flavors[0].resources[0].nominalQuota=8",
		"--set-string", "kuberay-operator.labels.environment=dev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helm args:\n got: %#v\nwant: %#v", got, want)
	}
	if upgrades != 2 {
		t.Fatalf("fresh install invoked Helm upgrade %d times, want CRD/control-plane bootstrap plus queue-policy upgrade", upgrades)
	}
	if !strings.Contains(out, "TauGrid installation plan") {
		t.Fatalf("install output missing plan:\n%s", out)
	}
	for _, want := range []string{
		"Helm wait:  disabled (Tau readiness validation still runs)",
		"Rollback:   disabled",
		"tau workspace create --principal-name <external-group-or-team> --apply",
		"kubectl get workspaces.tau.azure.com -n tau-platform",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("install output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kubectl get tauworkspace ") {
		t.Fatalf("install output names a nonexistent resource type:\n%s", out)
	}
	if !strings.Contains(out, "READY: 7/7 checks passed") {
		t.Fatalf("install output missing readiness report:\n%s", out)
	}
	if !strings.Contains(out, "TauGrid is installed and ready as Helm release") {
		t.Fatalf("install output missing next steps:\n%s", out)
	}
	if !strings.Contains(out, "pre-provision a Bound PVC") {
		t.Fatalf("install output missing external storage handoff:\n%s", out)
	}
}

func TestClusterInstallWaitAndRollbackAreOptIn(t *testing.T) {
	stubForceConflicts(t, false)
	stubRollbackOnFailure(t, true)
	var calls [][]string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	})

	out, err := runCluster(t, "install", "--wait", "--atomic", "--timeout", "45s")
	if err != nil {
		t.Fatalf("install errored: %v\n%s", err, out)
	}
	if len(calls) != 2 {
		t.Fatalf("Helm upgrade calls = %d, want 2", len(calls))
	}
	for _, args := range calls {
		if !containsArg(args, "--wait") || !containsArg(args, "--rollback-on-failure") {
			t.Fatalf("opt-in wait/rollback flags missing: %#v", args)
		}
		if !containsArgPair(args, "--timeout", "45s") {
			t.Fatalf("custom timeout missing: %#v", args)
		}
		if containsArg(args, "--atomic") {
			t.Fatalf("Helm 4 should receive --rollback-on-failure, not deprecated --atomic: %#v", args)
		}
	}
	for _, want := range []string{"Helm wait:  enabled", "Rollback:   enabled (implies Helm watcher wait)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install plan missing %q:\n%s", want, out)
		}
	}
}

func TestClusterInstallAtomicUsesHelm3Spelling(t *testing.T) {
	stubForceConflicts(t, false)
	stubRollbackOnFailure(t, false)
	var got []string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		got = append([]string(nil), args...)
		return nil
	})

	if _, err := runCluster(t, "install", "--atomic"); err != nil {
		t.Fatalf("install errored: %v", err)
	}
	if !containsArg(got, "--atomic") || containsArg(got, "--rollback-on-failure") {
		t.Fatalf("Helm 3 atomic args = %#v", got)
	}
}

func TestClusterInstallBootstrapsFreshReleaseWithoutQueuePolicy(t *testing.T) {
	var calls [][]string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	})

	if _, err := runCluster(t, "install"); err != nil {
		t.Fatalf("install errored: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("Helm upgrade calls = %d, want 2", len(calls))
	}
	if !containsArgPair(calls[0], "--set", "baselineQueue.enabled=false") {
		t.Fatalf("bootstrap args = %#v, want baseline queue disabled until Kueue CRDs exist", calls[0])
	}
	if containsArgPair(calls[1], "--set", "baselineQueue.enabled=false") {
		t.Fatalf("final args = %#v, baseline queue must use requested values", calls[1])
	}
	// Helm >=3.18 replays the previous release's values, so omitting the flag is
	// not enough to re-enable the baseline queue.
	if !containsArg(calls[1], "--reset-values") {
		t.Fatalf("final args = %#v, want --reset-values so the bootstrap override is not inherited", calls[1])
	}
}

func TestClusterInstallResetsValuesButKeepsRequestedOverrides(t *testing.T) {
	var calls [][]string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	})

	if _, err := runCluster(t, "install",
		"--values", "cluster.yaml",
		"--set", "baselineQueue.enabled=false"); err != nil {
		t.Fatalf("install errored: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("Helm upgrade calls = %d, want 2", len(calls))
	}
	// --reset-values only discards stored release values; every pass resends the
	// operator's own -f/--set, so opting out of the baseline queue still works.
	if !containsArgPair(calls[1], "--values", "cluster.yaml") ||
		!containsArgPair(calls[1], "--set", "baselineQueue.enabled=false") {
		t.Fatalf("final args = %#v, want requested values resent after reset", calls[1])
	}
}

func TestClusterInstallExistingReleaseSkipsBootstrap(t *testing.T) {
	original := runHelmCommand
	var upgrades int
	runHelmCommand = func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
		switch {
		case len(args) > 0 && args[0] == "list":
			_, _ = io.WriteString(out, `[{"name":"taugrid"}]`)
			return nil
		case len(args) > 1 && args[0] == "get" && args[1] == "values":
			_, _ = io.WriteString(out, "{}")
			return nil
		}
		upgrades++
		return nil
	}
	t.Cleanup(func() { runHelmCommand = original })
	installFakeInstallationValidation(t)

	if _, err := runCluster(t, "install"); err != nil {
		t.Fatalf("install errored: %v", err)
	}
	if upgrades != 1 {
		t.Fatalf("existing release invoked Helm upgrade %d times, want 1", upgrades)
	}
}

func TestClusterInstallListAvoidsHelm4RemovedAllFlag(t *testing.T) {
	// Helm 4 removed `--all` from `helm list`, which made `tau cluster install`
	// fail with a bare "unknown flag: --all". Assert we send the individual state
	// flags instead, which both Helm 3 and Helm 4 accept.
	original := runHelmCommand
	var listArgs []string
	runHelmCommand = func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
		switch {
		case len(args) > 0 && args[0] == "list":
			listArgs = append([]string(nil), args...)
			_, _ = io.WriteString(out, `[]`)
			return nil
		case len(args) > 1 && args[0] == "get" && args[1] == "values":
			_, _ = io.WriteString(out, "{}")
			return nil
		}
		return nil
	}
	t.Cleanup(func() { runHelmCommand = original })
	installFakeInstallationValidation(t)

	if _, err := runCluster(t, "install"); err != nil {
		t.Fatalf("install errored: %v", err)
	}
	if listArgs == nil {
		t.Fatal("helm list was never invoked")
	}
	for _, arg := range listArgs {
		if arg == "--all" {
			t.Errorf("helm list must not pass --all (removed in Helm 4); got %v", listArgs)
		}
	}
	for _, want := range helmListAllStateFlags {
		if !slices.Contains(listArgs, want) {
			t.Errorf("helm list missing state flag %q; got %v", want, listArgs)
		}
	}
}

func TestClusterInstallDryRunRendersOffline(t *testing.T) {
	// Record every Helm call, including `list`, so a regression that reads the
	// cluster before rendering fails the count assertion.
	original := runHelmCommand
	var calls [][]string
	runHelmCommand = func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "list" {
			_, _ = io.WriteString(out, "[]")
		}
		return nil
	}
	t.Cleanup(func() { runHelmCommand = original })
	installFakeInstallationValidation(t)

	out, err := runCluster(t, "install", "--dry-run", "--context", "aks-dev", "--values", "cluster.yaml")
	if err != nil {
		t.Fatalf("dry-run errored: %v\n%s", err, out)
	}
	if len(calls) != 1 {
		t.Fatalf("dry-run invoked Helm %#v, want a single render with no cluster read", calls)
	}
	want := []string{
		"template", defaultTauGridRelease, defaultTauGridChart,
		"--namespace", defaultTauGridNamespace,
		"--version", defaultTauGridChartVersion,
		"--include-crds",
		"--kube-context", "aks-dev",
		"--dependency-update",
		"--values", "cluster.yaml",
	}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("dry-run helm args:\n got: %#v\nwant: %#v", calls[0], want)
	}
	if !strings.Contains(out, "Rendered the TauGrid manifests offline") {
		t.Fatalf("dry-run output = %q", out)
	}
}

func TestClusterInstallDryRunSkipsDependencyUpdateBanner(t *testing.T) {
	installFakeHelm(t, func(_ context.Context, _ io.Reader, out, _ io.Writer, _ []string) error {
		// `helm template --dependency-update` prints this on stdout, ahead of
		// the first rendered document.
		_, _ = io.WriteString(out, `Hang tight while we grab the latest from your chart repositories...
...Successfully got an update from the "kuberay" chart repository
Update Complete. ⎈Happy Helming!⎈
Saving 4 charts
---
apiVersion: tau.azure.com/v1alpha1
kind: TauCluster
metadata:
  name: cluster
`)
		return nil
	})

	out, err := runCluster(t, "install", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run errored on the dependency-update banner: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Total          1 objects") {
		t.Fatalf("dry-run miscounted around the banner:\n%s", out)
	}
	if strings.Contains(out, "Hang tight") {
		t.Fatalf("dry-run echoed the dependency-update banner:\n%s", out)
	}
}

func TestClusterInstallDryRunSummarizesRenderedKinds(t *testing.T) {
	installFakeHelm(t, func(_ context.Context, _ io.Reader, out, _ io.Writer, _ []string) error {
		_, _ = io.WriteString(out, `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusters.tau.azure.com
spec:
  group: tau.azure.com
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: workspaces.tau.azure.com
---
apiVersion: tau.azure.com/v1alpha1
kind: TauCluster
metadata:
  name: cluster
---
`)
		return nil
	})

	out, err := runCluster(t, "install", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run errored: %v\n%s", err, out)
	}
	for _, want := range []string{
		"TauGrid render summary",
		"CustomResourceDefinition     2",
		"TauCluster                   1",
		"Total                        3 objects",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "clusters.tau.azure.com") {
		t.Fatalf("dry-run echoed the rendered manifest instead of summarizing it:\n%s", out)
	}
}

func TestClusterInstallDryRunSkipsReadinessValidation(t *testing.T) {
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		return nil
	})
	waitForTauGridInstallation = func(context.Context, installationcheck.Runner, installationcheck.Options) (installationcheck.Report, error) {
		t.Fatal("dry-run must not wait for installation readiness")
		return installationcheck.Report{}, nil
	}

	out, err := runCluster(t, "install", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run errored: %v\n%s", err, out)
	}
	if strings.Contains(out, "installed and ready") {
		t.Fatalf("dry-run reported a completed install:\n%s", out)
	}
}

func TestClusterInstallDryRunHonorsDependencyUpdateOptOut(t *testing.T) {
	var got []string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		got = append([]string(nil), args...)
		return nil
	})

	if _, err := runCluster(t, "install", "--dry-run", "--dependency-update=false"); err != nil {
		t.Fatalf("dry-run errored: %v", err)
	}
	if containsArg(got, "--dependency-update") {
		t.Fatalf("dry-run args = %#v, want no dependency update", got)
	}
}

func TestClusterUninstallRequiresConfirmation(t *testing.T) {
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		t.Fatal("Helm must not run without --yes")
		return nil
	})
	_, err := runCluster(t, "uninstall")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("uninstall error = %v, want --yes requirement", err)
	}
}

func TestClusterUninstallUsesHelm(t *testing.T) {
	var got []string
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, _ io.Writer, args []string) error {
		got = append([]string(nil), args...)
		return nil
	})
	out, err := runCluster(t, "uninstall", "--context", "aks-dev", "--yes", "--keep-history")
	if err != nil {
		t.Fatalf("uninstall errored: %v\n%s", err, out)
	}
	if len(got) == 0 || got[0] != "uninstall" || !containsArg(got, "--keep-history") {
		t.Fatalf("uninstall Helm args = %#v", got)
	}
}

func TestClusterInstallSurfacesHelmFailure(t *testing.T) {
	installFakeHelm(t, func(_ context.Context, _ io.Reader, _, errOut io.Writer, _ []string) error {
		fmt.Fprintln(errOut, `Error: failed to perform "FetchReference": repository not found`)
		return errors.New("exit status 1")
	})
	out, err := runCluster(t, "install")
	if err == nil {
		t.Fatal("install unexpectedly succeeded")
	}
	for _, want := range []string{
		defaultTauGridChart,
		defaultTauGridChartVersion,
		"verify that this version is published",
		"--chart <reference-or-local-path>",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("install error missing %q: %v", want, err)
		}
	}
	if !strings.Contains(out, "repository not found") {
		t.Fatalf("Helm registry diagnostic missing from output:\n%s", out)
	}
}

func TestClusterInstallPreservesNonRegistryHelmFailure(t *testing.T) {
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		return errors.New("Kubernetes cluster unreachable")
	})

	_, err := runCluster(t, "install")
	if err == nil || !strings.Contains(err.Error(), "Kubernetes cluster unreachable") {
		t.Fatalf("install error = %v", err)
	}
	if strings.Contains(err.Error(), "cannot resolve default TauGrid OCI chart") {
		t.Fatalf("cluster failure was mislabeled as a chart resolution error: %v", err)
	}
}

func TestClusterInstallRejectsNonPositiveTimeoutBeforeHelm(t *testing.T) {
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		t.Fatal("Helm must not run for an invalid timeout")
		return nil
	})
	_, err := runCluster(t, "install", "--timeout", "0s")
	if err == nil || !strings.Contains(err.Error(), "must be greater than zero") {
		t.Fatalf("install timeout error = %v", err)
	}
}

func TestClusterInstallSurfacesValidationFailure(t *testing.T) {
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		return nil
	})
	waitForTauGridInstallation = func(context.Context, installationcheck.Runner, installationcheck.Options) (installationcheck.Report, error) {
		report := installationcheck.Report{Results: []installationcheck.Result{
			{Name: "Kueue", Status: installationcheck.StatusFail, Detail: "Deployment is not ready"},
		}}
		return report, errors.New("TauGrid did not become ready")
	}

	out, err := runCluster(t, "install")
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("install validation error = %v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  Kueue") || !strings.Contains(out, "NOT READY") {
		t.Fatalf("install output missing failure report:\n%s", out)
	}
	if strings.Contains(out, "installed and ready") {
		t.Fatalf("install reported success after validation failure:\n%s", out)
	}
}

func TestClusterLifecycleRejectsPositionalArguments(t *testing.T) {
	for _, command := range []string{"install", "uninstall"} {
		t.Run(command, func(t *testing.T) {
			installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
				t.Fatal("Helm must not run when positional arguments are rejected")
				return nil
			})
			_, err := runCluster(t, command, "unexpected")
			if err == nil || !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "unknown argument") && !strings.Contains(err.Error(), "accepts 0 arg") {
				t.Fatalf("%s positional argument error = %v", command, err)
			}
		})
	}
}

func installFakeHelm(t *testing.T, fake helmCommandRunner) {
	t.Helper()
	original := runHelmCommand
	runHelmCommand = func(ctx context.Context, in io.Reader, out, errOut io.Writer, args []string) error {
		switch {
		case len(args) > 0 && args[0] == "list":
			_, _ = io.WriteString(out, "[]")
			return nil
		case len(args) > 1 && args[0] == "get" && args[1] == "values":
			_, _ = io.WriteString(out, "{}")
			return nil
		}
		return fake(ctx, in, out, errOut, args)
	}
	installFakeInstallationValidation(t)
	t.Cleanup(func() {
		runHelmCommand = original
	})
}

func installFakeInstallationValidation(t *testing.T) {
	t.Helper()
	originalWait := waitForTauGridInstallation
	originalRunner := newInstallationCheckRunner
	newInstallationCheckRunner = func(string) installationcheck.Runner {
		return nil
	}
	waitForTauGridInstallation = func(context.Context, installationcheck.Runner, installationcheck.Options) (installationcheck.Report, error) {
		return readyInstallationReport(), nil
	}
	t.Cleanup(func() {
		waitForTauGridInstallation = originalWait
		newInstallationCheckRunner = originalRunner
	})
}

func readyInstallationReport() installationcheck.Report {
	names := []string{"Kubernetes", "Kueue", "KubeRay", "Tau controller", "TauCluster", "Baseline queue", "Quota guard"}
	results := make([]installationcheck.Result, 0, len(names))
	for _, name := range names {
		results = append(results, installationcheck.Result{Name: name, Status: installationcheck.StatusPass, Detail: "ready"})
	}
	return installationcheck.Report{Results: results}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// The pinned chart version is always passed as --version, so for the default
// OCI chart it selects the published artifact. A stale constant silently
// installs the previous chart, and a local-path chart hides it because Helm
// ignores --version there.
func TestDefaultChartVersionMatchesChartYAML(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean("../../../charts/taugrid/Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var chart struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatalf("parse Chart.yaml: %v", err)
	}
	if chart.Version != defaultTauGridChartVersion {
		t.Fatalf("defaultTauGridChartVersion is %q but charts/taugrid/Chart.yaml is %q; bump the constant with the chart",
			defaultTauGridChartVersion, chart.Version)
	}
}

// GPU node health monitoring is linked to the Tau control plane rather than
// carrying an independent default: a cluster that installs the controller must
// not be able to end up with no GPU health signal. Helm evaluates only the
// first condition path that exists in values, so the linkage depends on two
// things that are easy to break silently -- the ordered condition on the
// dependency, and the continued absence of components.gpuMonitoring from
// values.yaml. Adding that key back would pin the subchart to its own default
// and sever the link without failing any template-rendering test.
func TestTauGridChartLinksGPUMonitoringToController(t *testing.T) {
	const wantCondition = "components.gpuMonitoring.enabled,components.tauCoreController.enabled"

	raw, err := os.ReadFile(filepath.Clean("../../../charts/taugrid/Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var chart struct {
		Dependencies []struct {
			Name      string `yaml:"name"`
			Condition string `yaml:"condition"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatalf("parse Chart.yaml: %v", err)
	}

	var found bool
	for _, dep := range chart.Dependencies {
		if dep.Name != "gpu-monitoring" {
			continue
		}
		found = true
		if dep.Condition != wantCondition {
			t.Fatalf("gpu-monitoring condition is %q, want %q; the controller path must come second so it acts as the fallback",
				dep.Condition, wantCondition)
		}
	}
	if !found {
		t.Fatal("charts/taugrid/Chart.yaml has no gpu-monitoring dependency; a TauGrid cluster must ship GPU node health monitoring")
	}

	valuesRaw, err := os.ReadFile(filepath.Clean("../../../charts/taugrid/values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var values struct {
		Components map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	if _, ok := values.Components["gpuMonitoring"]; ok {
		t.Fatal("components.gpuMonitoring must stay unset in values.yaml, otherwise Helm stops falling through to components.tauCoreController and GPU monitoring no longer follows the control plane")
	}
}
