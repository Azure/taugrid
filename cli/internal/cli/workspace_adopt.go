package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
)

var newWorkspaceAdoptRunner = func(kubeContext string) tauworkspace.AdoptRunner {
	return kube.New(kubeContext)
}

func newWorkspaceAdoptCmd() *cobra.Command {
	var options tauworkspace.AdoptOptions
	var kubeContext string
	var apply bool

	cmd := &cobra.Command{
		Use:   "adopt NAME",
		Short: "Adopt an existing platform-provisioned workspace",
		Long: `Validate and adopt an existing workload namespace by conditionally creating one native
TauWorkspace custom resource.

This is a platform-operator command for non-GitOps handoff. It requires the
Namespace and LocalQueue to exist, verifies that the LocalQueue references a
readable ClusterQueue, and requires the optional data PVC to exist and be
Bound. UID, StorageClass, and ClusterQueue guards can pin the exact resources
being adopted.

The default is a read-only preview. --apply repeats preflight, performs a
server-side dry-run, and creates the TauWorkspace only when it is still absent.
A compatible existing TauWorkspace is a no-op; conflicting intent is refused. The command
does not create Namespace, RBAC, LocalQueue, PVC, StorageClass, ClusterQueue,
Secret, or Azure resources.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.Name = args[0]
			if options.Namespace == "" {
				options.Namespace = options.Name
			}
			if options.OutputRoot == "" {
				options.OutputRoot = "/data/projects/" + options.Name + "/runs"
			}
			runner := newWorkspaceAdoptRunner(kubeContext)
			report, err := tauworkspace.PreflightAdoption(cmd.Context(), runner, options)
			if err != nil {
				return err
			}
			if !apply {
				manifest, err := tauworkspace.RenderAdoption(options)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", report.Summary())
				_, err = cmd.OutOrStdout().Write(manifest)
				return err
			}
			out, err := tauworkspace.ApplyAdoption(cmd.Context(), runner, options, report)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), report.Summary())
			if strings.TrimSpace(out) != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
				if !strings.HasSuffix(out, "\n") {
					fmt.Fprintln(cmd.OutOrStdout())
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&options.Namespace, "namespace", "n", "", "existing workload namespace (default: NAME)")
	cmd.Flags().StringVar(&options.Queue, "queue", tauworkspace.DefaultAdoptQueue, "existing LocalQueue in the workload namespace")
	cmd.Flags().StringVar(&options.PlatformNamespace, "platform-namespace", tauworkspace.PlatformNamespace, "namespace containing the TauWorkspace CR")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&options.DataPVC, "data-pvc", tauworkspace.DefaultAdoptDataPVC, "existing Bound data PVC to validate (empty skips PVC validation)")
	cmd.Flags().StringVar(&options.NamespaceUID, "namespace-uid", "", "expected immutable Namespace UID")
	cmd.Flags().StringVar(&options.QueueUID, "queue-uid", "", "expected immutable LocalQueue UID")
	cmd.Flags().StringVar(&options.PVCUID, "pvc-uid", "", "expected immutable PVC UID")
	cmd.Flags().StringVar(&options.StorageClass, "storage-class", "", "expected PVC StorageClass name")
	cmd.Flags().StringVar(&options.ClusterQueue, "cluster-queue", "", "expected ClusterQueue referenced by the LocalQueue")
	cmd.Flags().StringVar(&options.OutputRoot, "output-root", "", "workspace default output root (default: /data/projects/NAME/runs)")
	cmd.Flags().StringVar(&options.Priority, "priority", "", "workspace default priority tier: default|priority|normal")
	cmd.Flags().BoolVar(&apply, "apply", false, "create only the TauWorkspace after server-side preflight")
	return cmd
}
