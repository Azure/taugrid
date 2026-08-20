// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
)

var newWorkspaceCreateRunner = func(kubeContext string) tauworkspace.AdoptRunner {
	return kube.New(kubeContext)
}

func newWorkspaceCreateCmd() *cobra.Command {
	var options tauworkspace.CreateOptions
	var kubeContext string
	var apply bool

	cmd := &cobra.Command{
		Use:   "create [NAME]",
		Short: "Create the single v0 researcher workspace",
		Long: `Create the single researcher workspace supported by TauGrid v0.

NAME is optional and defaults to "` + tauworkspace.DefaultWorkspaceName + `", so a stock install of
TauGrid looks the same on every cluster. Pass a name to override it; nothing in
Tau depends on the default being used, because "tau run" resolves whichever
workspace the cluster actually has.

Preview is read-only and prints the TauWorkspace manifest. With --apply, Tau
conditionally creates that one object. The Tau controller then creates or
reconciles the workload Namespace, researcher RBAC, and jobqueue LocalQueue.
The baseline jobqueue ClusterQueue must already exist from "tau cluster install".

Omitting --principal-name defaults it to NAME, so an Entra cluster can be
brought up before its identity group exists. Entra asserts groups by object ID,
so the subject then names a group nobody asserts and the workspace grants
nobody access until a real group is named. v0 permits one workspace, so once it
is created, naming the group is an edit of that object rather than a second
create:

  kubectl edit workspaces.tau.azure.com <name> -n tau-platform

Every other combination requires --principal-name. A GitHub team slug, an Entra
UPN, and a ServiceAccount name all share a shape with workspace names, so the
same fallback there could bind a subject that really exists.

Storage and Azure workload identity resources remain platform-owned. Optional
workload identity flags configure only the Kubernetes ServiceAccount.

--principal-name is required and identifies the external Entra group or GitHub
team that receives access to the workspace.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.Name = tauworkspace.DefaultWorkspaceName
			if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
				options.Name = strings.TrimSpace(args[0])
			}
			if options.Namespace == "" {
				options.Namespace = options.Name
			}
			if options.OutputRoot == "" {
				options.OutputRoot = "/data/projects/" + options.Name + "/runs"
			}
			// An explicitly-empty --principal-name is a shell variable that did
			// not expand, not a request for the default, so gate on presence.
			options.DefaultPrincipalToName = !cmd.Flags().Changed("principal-name")
			// Ask the options what they resolved to rather than re-deriving it
			// here: the notice has to describe the manifest that was rendered.
			inertSubject := options.PrincipalWasDefaulted()

			manifest, err := tauworkspace.RenderCreation(options)
			if err != nil {
				return err
			}
			runner := newWorkspaceCreateRunner(kubeContext)
			report, err := tauworkspace.PreflightCreation(cmd.Context(), runner, options)
			if err != nil {
				return err
			}
			if inertSubject {
				// A second create is refused as conflicting intent once the
				// object exists, so the remediation depends on whether it does
				// — which preflight already knows. Reading --apply alone would
				// send a repeated preview at a rerun that cannot work.
				exists := apply || report.ExistingWorkspaceName != ""
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", inertSubjectNotice(options, exists))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", report.Summary())
			if !apply {
				_, err := cmd.OutOrStdout().Write(manifest)
				return err
			}

			out, err := tauworkspace.ApplyCreation(cmd.Context(), runner, options, report, manifest)
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return err
		},
	}

	cmd.Flags().StringVar(&options.Namespace, "namespace", "", "researcher workload namespace (default NAME)")
	cmd.Flags().StringVar(&options.PlatformNamespace, "platform-namespace", tauworkspace.PlatformNamespace, "namespace containing TauWorkspace objects")
	cmd.Flags().StringVar(&options.Queue, "queue", tauworkspace.DefaultWorkspaceQueue, "baseline ClusterQueue and LocalQueue name")
	cmd.Flags().StringVar(&options.PrincipalProvider, "principal-provider", "entra", "external identity provider: entra|github")
	cmd.Flags().StringVar(&options.PrincipalName, "principal-name", "", "external researcher group or team name (default NAME for an Entra Group)")
	cmd.Flags().StringVar(&options.KubernetesSubjectKind, "subject-kind", "Group", "Kubernetes RBAC subject kind: Group|User|ServiceAccount")
	cmd.Flags().StringVar(&options.KubernetesSubjectName, "subject-name", "", "Kubernetes RBAC subject name (default --principal-name)")
	cmd.Flags().StringVar(&options.OutputRoot, "output-root", "", "default durable result path (default /data/projects/NAME/runs)")
	cmd.Flags().StringVar(&options.Priority, "priority", "normal", "default workload priority: default|priority|normal")
	cmd.Flags().StringVar(&options.ServiceAccountName, "service-account", "", "workload ServiceAccount to reconcile (requires --workload-identity-client-id)")
	cmd.Flags().StringVar(&options.WorkloadIdentityClientID, "workload-identity-client-id", "", "Azure workload identity client ID (requires --service-account)")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().BoolVar(&apply, "apply", false, "conditionally create the TauWorkspace after preflight and server dry-run")
	return cmd
}

// inertSubjectNotice names the remediation that works from where the operator
// is standing. The two differ because v0 permits one workspace: while nothing
// exists, rerunning with the group is the fix, but once the object is there a
// second create is refused as conflicting intent and only an edit gets there.
func inertSubjectNotice(options tauworkspace.CreateOptions, workspaceExists bool) string {
	remediation := "rerun with --principal-name <group>"
	if workspaceExists {
		remediation = fmt.Sprintf("name the real group with: kubectl edit workspaces.tau.azure.com %s -n %s",
			options.Name, options.ResolvedPlatformNamespace())
	}
	return fmt.Sprintf(
		"the RBAC subject defaulted to Group %q, which no identity provider asserts, so this workspace grants nobody access; %s",
		options.Name, remediation)
}
