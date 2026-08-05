package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
	"github.com/Azure/taugrid/core/kube"
)

const defaultInstallationValidationPollInterval = 5 * time.Second

var (
	newInstallationCheckRunner = func(kubeContext string) installationcheck.Runner {
		return kube.New(kubeContext)
	}
	waitForTauGridInstallation = installationcheck.Wait
)

func newClusterValidateInstallationCmd() *cobra.Command {
	var (
		kubeContext string
		release     = defaultTauGridRelease
		namespace   = defaultTauGridNamespace
		timeoutText = "5m"
		pollText    = defaultInstallationValidationPollInterval.String()
	)

	cmd := &cobra.Command{
		Use:   "installation",
		Short: "Validate a Helm-installed TauGrid control plane",
		Long: `Run the same read-only readiness checks used by tau cluster install.

Validation covers the supported Kubernetes version, Kueue, KubeRay,
tau-core-controller, the singleton TauCluster, the Helm-owned baseline
ClusterQueue, and the narrow fail-closed quota decision admission guard. It does
not submit a workload or mutate the cluster.

Components the release turned off through components.<name>.enabled are reported
as SKIP and do not fail validation, so a cluster that keeps its own Kueue or
KubeRay can still use this command as a gate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			timeout, err := time.ParseDuration(timeoutText)
			if err != nil {
				return fmt.Errorf("invalid --timeout: %w", err)
			}
			if timeout <= 0 {
				return fmt.Errorf("invalid --timeout: must be greater than zero")
			}
			pollInterval, err := time.ParseDuration(pollText)
			if err != nil {
				return fmt.Errorf("invalid --poll-interval: %w", err)
			}
			if pollInterval <= 0 {
				return fmt.Errorf("invalid --poll-interval: must be greater than zero")
			}
			disabled := disabledTauGridComponents(cmd, kubeContext, release, namespace)
			return runTauGridInstallationValidation(
				cmd.Context(),
				newInstallationCheckRunner(kubeContext),
				installationcheck.Options{
					Release:               release,
					ControlPlaneNamespace: namespace,
					Timeout:               timeout,
					PollInterval:          pollInterval,
					DisabledComponents:    disabled,
				},
				cmd.OutOrStdout(),
			)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	flags.StringVar(&release, "release", release, "Helm release name")
	flags.StringVar(&namespace, "namespace", namespace, "namespace containing Kueue and KubeRay")
	flags.StringVar(&timeoutText, "timeout", timeoutText, "maximum readiness wait")
	flags.StringVar(&pollText, "poll-interval", pollText, "readiness poll interval")
	return cmd
}

func runTauGridInstallationValidation(
	ctx context.Context,
	runner installationcheck.Runner,
	opts installationcheck.Options,
	out io.Writer,
) error {
	fmt.Fprintln(out, "Waiting for TauGrid installation readiness...")
	report, err := waitForTauGridInstallation(ctx, runner, opts)
	if len(report.Results) > 0 {
		fmt.Fprintln(out, report.Summary())
	}
	return err
}

// disabledTauGridComponents reads the release's coalesced Helm values so
// validation can skip components the operator turned off. A component switch
// can come from a values file or an earlier upgrade, so the live release is
// the only source that sees all of them.
//
// An unreadable release degrades to validating every component rather than
// aborting: the command still works without Helm on PATH, and a bad read can
// only produce a false failure, never a false pass.
func disabledTauGridComponents(cmd *cobra.Command, kubeContext, release, namespace string) []installationcheck.Component {
	var disabled []installationcheck.Component
	values, err := tauGridReleaseValues(cmd, kubeContext, release, namespace)
	if err == nil {
		disabled, err = installationcheck.DisabledComponents(values)
	}
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Cannot read Helm release %s values (%v); validating every component.\n", release, err)
		return nil
	}
	return disabled
}
