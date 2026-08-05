package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/onboarding"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

type builtinSmokeRunner interface {
	Run(context.Context, onboarding.SmokeOptions) (onboarding.SmokeResult, error)
}

type smokeWorkspaceFetcher func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error)

type smokeWorkspaceDiscoverer func(*cobra.Command, string) (tauworkspace.Workspace, error)

type builtinSmokeCLIOptions struct {
	Workspace string
	Namespace string
	// KubeContext is the resolved target cluster, and KubeContextExplicit
	// records whether one was named at all. They are separate because the
	// resolved value carries the $TAU_CONTEXT default, so an empty string no
	// longer distinguishes "not named" from "named as empty".
	KubeContext         string
	KubeContextExplicit bool
	KubeContextFromFlag bool
	DryRun              string
	Connection          runConnectionSource
	ConnectionFactory   runConnectionEnsurerFactory
	WorkspaceFetcher    smokeWorkspaceFetcher
	WorkspaceDiscoverer smokeWorkspaceDiscoverer
	SmokeRunner         builtinSmokeRunner
}

func executeBuiltinSmoke(cmd *cobra.Command, cliOptions builtinSmokeCLIOptions) error {
	dispatch := defaultRunDispatchOptions()
	dispatch.workspace = cliOptions.Workspace
	dispatch.namespace = cliOptions.Namespace
	dispatch.kubeContext = cliOptions.KubeContext
	dispatch.workspaceExplicit = cliOptions.Workspace != ""
	dispatch.kubeContextExplicit = cliOptions.KubeContextExplicit
	dispatch.kubeContextFromFlag = cliOptions.KubeContextFromFlag
	dispatch.dryRun = cliOptions.DryRun

	ensurer := cliOptions.ConnectionFactory(cmd)
	resolved, connection, err := applyAutomaticRunConnection(
		cmd.Context(),
		dispatch,
		cliOptions.Connection,
		true,
		ensurer,
	)
	if err != nil {
		return err
	}
	restoreKubeconfig, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return err
	}
	defer restoreKubeconfig()

	if resolved.workspace == "" {
		// v0 has one workspace per cluster, so resolve it instead of asking
		// the researcher for a value with only one correct answer. Unlike
		// `tau run`, smoke cannot proceed without a workspace, so a discovery
		// failure is fatal here and its message is the one the user sees.
		discoverer := cliOptions.WorkspaceDiscoverer
		if discoverer == nil {
			discoverer = discoverPrimaryWorkspace
		}
		discovered, derr := discoverer(cmd, resolved.kubeContext)
		if derr != nil {
			return derr
		}
		resolved.workspace = discovered.Metadata.Name
	}
	workspaceFetcher := cliOptions.WorkspaceFetcher
	if workspaceFetcher == nil {
		workspaceFetcher = fetchWorkspace
	}
	workspace, err := workspaceFetcher(cmd, resolved.kubeContext, tauworkspace.PlatformNamespace, resolved.workspace)
	if err != nil {
		return err
	}
	resolved, err = applyWorkspaceDefaults(resolved, workspace, "smoke")
	if err != nil {
		return err
	}
	serviceAccount := connection.ServiceAccount
	if serviceAccount == "" && workspace.Spec.WorkloadIdentity != nil {
		serviceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
	}
	smokeRunner := cliOptions.SmokeRunner
	if smokeRunner == nil {
		smokeRunner = onboarding.NewSmokeRunner(resolved.kubeContext)
	}
	result, err := smokeRunner.Run(cmd.Context(), onboarding.SmokeOptions{
		Namespace:      resolved.namespace,
		Queue:          resolved.queue,
		ServiceAccount: serviceAccount,
		Workspace:      resolved.workspace,
		ResultScope:    resolved.workspaceResultScope,
		DryRun:         resolved.dryRun,
	})
	if err != nil {
		return err
	}
	if result.Phase == "DryRun" {
		_, err := cmd.OutOrStdout().Write(result.Manifest)
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Run: %s\n", result.RunID)
	fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n", resolved.workspace)
	fmt.Fprintln(cmd.OutOrStdout(), "Phase: Succeeded")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Validated  True")
	fmt.Fprintln(cmd.OutOrStdout(), "Admitted   True")
	fmt.Fprintln(cmd.OutOrStdout(), "Scheduled  True")
	fmt.Fprintln(cmd.OutOrStdout(), "Executed   True")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintf(cmd.OutOrStdout(), "Logs:    tau run logs %s\n", result.RunID)
	fmt.Fprintf(cmd.OutOrStdout(), "Details: tau run status %s\n", result.RunID)
	fmt.Fprintln(cmd.OutOrStdout())
	// Be explicit about scope. This probe runs a public base image and a
	// trivial command, so a green result proves the platform path only. Saying
	// "onboarding complete" here sent researchers to debug their own code when
	// the real failure was an unexercised image pull, GPU, or storage path.
	fmt.Fprintln(cmd.OutOrStdout(), "Platform reachable: workspace, queue admission, and pod execution work.")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Not covered by this probe: your container image pull, GPU capacity,")
	fmt.Fprintln(cmd.OutOrStdout(), "and durable /data storage. Run your own config next, for example:")
	fmt.Fprintln(cmd.OutOrStdout(), "  tau run --config tau/smoke.yaml")
	return nil
}
