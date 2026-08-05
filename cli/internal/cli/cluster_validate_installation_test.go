package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

func TestClusterValidateInstallationUsesSharedReadinessPath(t *testing.T) {
	var got installationcheck.Options
	installFakeHelm(t, func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		t.Fatal("validate installation must not run Helm beyond reading release values")
		return nil
	})
	newInstallationCheckRunner = func(contextName string) installationcheck.Runner {
		if contextName != "aks-dev" {
			t.Fatalf("context = %q, want aks-dev", contextName)
		}
		return nil
	}
	waitForTauGridInstallation = func(_ context.Context, _ installationcheck.Runner, opts installationcheck.Options) (installationcheck.Report, error) {
		got = opts
		return readyInstallationReport(), nil
	}

	out, err := runCluster(t,
		"validate", "installation",
		"--context", "aks-dev",
		"--release", "research-grid",
		"--namespace", "tau-control",
		"--timeout", "3m",
		"--poll-interval", "2s",
	)
	if err != nil {
		t.Fatalf("validate installation errored: %v\n%s", err, out)
	}
	if got.Release != "research-grid" ||
		got.ControlPlaneNamespace != "tau-control" ||
		got.Timeout != 3*time.Minute ||
		got.PollInterval != 2*time.Second {
		t.Fatalf("validation options = %+v", got)
	}
	if !strings.Contains(out, "Waiting for TauGrid installation readiness") ||
		!strings.Contains(out, "READY: 7/7 checks passed") {
		t.Fatalf("validation output missing readiness report:\n%s", out)
	}
}

func TestClusterValidateInstallationSkipsDisabledComponents(t *testing.T) {
	var helmArgs []string
	original := runHelmCommand
	runHelmCommand = func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
		helmArgs = append([]string(nil), args...)
		_, _ = io.WriteString(out, `{"components":{"kueue":{"enabled":false},"kuberayOperator":{"enabled":false}}}`)
		return nil
	}
	t.Cleanup(func() { runHelmCommand = original })
	installFakeInstallationValidation(t)

	var got installationcheck.Options
	waitForTauGridInstallation = func(_ context.Context, _ installationcheck.Runner, opts installationcheck.Options) (installationcheck.Report, error) {
		got = opts
		return readyInstallationReport(), nil
	}

	if out, err := runCluster(t, "validate", "installation", "--context", "aks-dev"); err != nil {
		t.Fatalf("validate installation errored: %v\n%s", err, out)
	}
	want := []string{"get", "values", defaultTauGridRelease, "--namespace", defaultTauGridNamespace, "--all", "--output", "json", "--kube-context", "aks-dev"}
	if !slices.Equal(helmArgs, want) {
		t.Fatalf("helm args:\n got: %#v\nwant: %#v", helmArgs, want)
	}
	if !slices.Equal(got.DisabledComponents, []installationcheck.Component{installationcheck.ComponentKueue, installationcheck.ComponentKubeRay}) {
		t.Fatalf("disabled components = %v, want both chart components", got.DisabledComponents)
	}
}

func TestClusterValidateInstallationValidatesEverythingWhenReleaseValuesAreUnreadable(t *testing.T) {
	original := runHelmCommand
	runHelmCommand = func(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
		return errors.New("release: not found")
	}
	t.Cleanup(func() { runHelmCommand = original })
	installFakeInstallationValidation(t)

	var got installationcheck.Options
	waitForTauGridInstallation = func(_ context.Context, _ installationcheck.Runner, opts installationcheck.Options) (installationcheck.Report, error) {
		got = opts
		return readyInstallationReport(), nil
	}

	out, err := runCluster(t, "validate", "installation")
	if err != nil {
		t.Fatalf("unreadable release values aborted validation: %v\n%s", err, out)
	}
	if got.DisabledComponents != nil {
		t.Fatalf("disabled components = %v, want every component validated", got.DisabledComponents)
	}
	if !strings.Contains(out, "validating every component") || !strings.Contains(out, "release: not found") {
		t.Fatalf("output did not explain the degraded read:\n%s", out)
	}
}

func TestClusterValidateInstallationRejectsInvalidTimeout(t *testing.T) {
	out, err := runCluster(t, "validate", "installation", "--timeout", "later")
	if err == nil || !strings.Contains(err.Error(), "invalid --timeout") {
		t.Fatalf("invalid timeout error = %v\n%s", err, out)
	}
}

func TestClusterValidateInstallationRejectsNonPositivePollInterval(t *testing.T) {
	out, err := runCluster(t, "validate", "installation", "--poll-interval", "0s")
	if err == nil || !strings.Contains(err.Error(), "invalid --poll-interval") {
		t.Fatalf("invalid poll interval error = %v\n%s", err, out)
	}
}
