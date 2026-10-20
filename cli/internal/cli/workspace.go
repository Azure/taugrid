// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/reposcaffold"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/version"
)

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Create, adopt, and inspect Tau workspaces",
		Long: `Create the v0 researcher workspace, adopt existing workspace resources,
inspect TauWorkspace status, create structured quota requests, and scaffold
Tau-ready research repositories.

TauWorkspace is platform-owned policy. Researchers can read their workspace
status, create quota requests, and generate workspace-first repos, but cannot
mutate workspace policy from the repo scaffold.`,
		Example: `  tau workspace create --principal-name research-team --apply
  tau workspace list
  tau workspace status research-team
  tau workspace connection assign research-team
  tau workspace init-repo my-project --image myregistry.azurecr.io/my-project:latest`,
		Args: cobra.NoArgs,
		RunE: showGroupHelp,
	}
	cmd.AddCommand(
		newWorkspaceCreateCmd(),
		newWorkspaceAdoptCmd(),
		newWorkspaceConnectionCmd(),
		newWorkspaceListCmd(),
		newWorkspaceStatusCmd(false),
		newWorkspaceStatusCmd(true),
		newWorkspaceQuotaCmd(),
		newWorkspaceInitRepoCmd(),
	)
	return cmd
}

func NewRepoGenRoot() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tau-gen",
		Short:   "Generate Tau-ready research workspaces",
		Version: version.Version,
		Long: `tau-gen generates Tau-ready research repository workspaces.

It is the standalone generator surface for the same scaffold available through
` + "`tau workspace init-repo`" + `. It writes local files only; connected
scaffolds resolve TauWorkspace authorization from the selected cluster unless
--authorization-mode is supplied for offline generation.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newRepoGenInitCmd())
	return cmd
}

func newRepoGenInitCmd() *cobra.Command {
	return newRepoScaffoldCmd("init NAME", "Generate a Python + uv Tau workspace repo scaffold", "tau-gen init")
}

func newWorkspaceInitRepoCmd() *cobra.Command {
	return newRepoScaffoldCmd("init-repo NAME", "Generate a Python + uv Tau repo scaffold", "tau workspace init-repo")
}

func newRepoScaffoldCmd(use, short, invocation string) *cobra.Command {
	var opts reposcaffold.Options
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: `Generate a Python + uv repository scaffold that is ready for Tau jobs.

The generated repo includes README.md, tau/smoke.yaml, tau/train.yaml,
tau/train-gpu.yaml,
images/train.Dockerfile, scripts/configure.sh, scripts/smoke.sh,
scripts/train.sh, .gitignore, and supporting Python/Azure/agent setup files.

This command does not create cloud resources and does not add policy.workspace to
committed Tau configs. Provide --workspace with --kube-context to resolve the live
TauWorkspace authorization and generate a provider-neutral connection descriptor
that uses existing kubeconfig credentials. For offline generation, provide
--authorization-mode explicitly. Add all AKS flags to let Tau acquire AKS
credentials automatically.`,
		Example: `  ` + invocation + ` my-project --image myregistry.azurecr.io/my-project:latest
  ` + invocation + ` my-project --image myregistry.azurecr.io/my-project:latest \
      --workspace research-a --kube-context aks-research`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			if err := resolveRepoScaffoldAuthorization(cmd, &opts); err != nil {
				return err
			}
			result, err := reposcaffold.Render(opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "generated Tau Python repo: %s\n", result.OutputDir)
			for _, file := range result.Files {
				fmt.Fprintf(out, "  %s\n", file)
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "next steps:")
			fmt.Fprintf(out, "  cd %s\n", result.OutputDir)
			fmt.Fprintln(out, "  cp .env.example .env && ${EDITOR:-vi} .env")
			fmt.Fprintln(out, "  ./scripts/setup.sh")
			fmt.Fprintln(out, "  source ./.env")
			fmt.Fprintln(out, "  docker build -f images/train.Dockerfile -t \"$IMAGE\" .")
			fmt.Fprintln(out, "  docker push \"$IMAGE\"")
			fmt.Fprintln(out, "  ./scripts/configure.sh --image \"$IMAGE\"")
			fmt.Fprintln(out, "  tau run validate --config tau/train.yaml")
			if slices.Contains(result.Files, "tau/workspace.connection.yaml") {
				fmt.Fprintln(out, "  tau run train")
			} else {
				fmt.Fprintln(out, "  # Ask the platform owner to add tau/workspace.connection.yaml before cluster runs.")
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "note: workspace policy remains cluster-owned; do not commit policy.workspace to tau/*.yaml.")
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Template, "template", reposcaffold.TemplatePython, "template: python|external-github|dpr")
	cmd.Flags().StringVar(&opts.OutputDir, "output", "", "destination directory (default: ./NAME)")
	cmd.Flags().StringVar(&opts.Image, "image", "", "runtime image to write into generated Tau configs (required)")
	cmd.Flags().StringVar(&opts.PythonVersion, "python-version", "", "Python version for uv and image base (default: 3.12)")
	cmd.Flags().StringVar(&opts.Package, "package", "", "local Python package name under src/ (default: sanitized NAME)")
	cmd.Flags().StringVar(&opts.TauRev, "tau-rev", "", "Tau Python SDK git ref for pyproject.toml (default: main)")
	cmd.Flags().StringVar(&opts.Workspace, "workspace", "", "TauWorkspace name for the generated connection descriptor")
	cmd.Flags().StringVar(&opts.AzureSubscriptionID, "azure-subscription-id", "", "Azure subscription containing the AKS cluster")
	cmd.Flags().StringVar(&opts.AzureTenantID, "azure-tenant-id", "", "Microsoft Entra tenant ID for the workspace connection descriptor")
	cmd.Flags().StringVar(&opts.AKSResourceGroup, "aks-resource-group", "", "AKS resource group for the connection descriptor")
	cmd.Flags().StringVar(&opts.AKSCluster, "aks-cluster", "", "AKS cluster name for the connection descriptor")
	cmd.Flags().StringVar(&opts.KubeContext, "kube-context", "", "Kubernetes context for the connection descriptor (defaults to --aks-cluster for AKS access)")
	cmd.Flags().StringVar(&opts.SystemNamespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().StringVar(&opts.AuthorizationMode, "authorization-mode", "", "authorization mode for offline connected scaffold generation: cluster-wide|workspace-rbac")
	cmd.Flags().StringVar(&opts.ACRName, "acr-name", "", "ACR name placeholder for .env.example and setup-azure.sh")
	cmd.Flags().StringVar(&opts.UpstreamRepo, "upstream", "", "open GitHub repo URL for external-github/dpr templates")
	cmd.Flags().StringVar(&opts.UpstreamRef, "ref", "", "upstream branch, tag, or commit for external-github/dpr templates")
	cmd.Flags().StringVar(&opts.PackageImport, "package-import", "", "optional Python import checked by open-repo smoke")
	cmd.Flags().StringVar(&opts.SmokeCommand, "smoke-command", "", "cheap smoke command for open-repo templates")
	cmd.Flags().StringVar(&opts.TrainCommand, "train-command", "", "initial train scaffold command")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "overwrite generator-managed files if they already exist")
	return cmd
}

func resolveRepoScaffoldAuthorization(cmd *cobra.Command, opts *reposcaffold.Options) error {
	if strings.TrimSpace(opts.AuthorizationMode) != "" {
		return nil
	}
	workspaceName := strings.TrimSpace(opts.Workspace)
	kubeContext := strings.TrimSpace(opts.KubeContext)
	if kubeContext == "" &&
		strings.TrimSpace(opts.AzureSubscriptionID) != "" &&
		strings.TrimSpace(opts.AzureTenantID) != "" &&
		strings.TrimSpace(opts.AKSResourceGroup) != "" &&
		strings.TrimSpace(opts.AKSCluster) != "" {
		kubeContext = strings.TrimSpace(opts.AKSCluster)
	}
	if workspaceName == "" || kubeContext == "" {
		return nil
	}
	systemNamespace := strings.TrimSpace(opts.SystemNamespace)
	if systemNamespace == "" {
		systemNamespace = defaultSystemNamespace()
	}
	workspace, err := fetchWorkspace(cmd, kubeContext, systemNamespace, workspaceName)
	if err != nil {
		return fmt.Errorf(
			"resolve authorization for TauWorkspace %q: %w; for offline generation, pass --authorization-mode",
			workspaceName,
			err,
		)
	}
	opts.AuthorizationMode = tauworkspace.AuthorizationModeWorkspaceRBAC
	if workspace.Spec.Authorization != nil && strings.TrimSpace(workspace.Spec.Authorization.Mode) != "" {
		opts.AuthorizationMode = strings.TrimSpace(workspace.Spec.Authorization.Mode)
	}
	if opts.AuthorizationMode == tauworkspace.AuthorizationModeWorkspaceRBAC {
		opts.RequiredRole = strings.TrimSpace(workspace.Spec.Role)
		if opts.RequiredRole == "" {
			opts.RequiredRole = tauworkspace.DefaultResearcherRole
		}
	}
	return nil
}

func newWorkspaceListCmd() *cobra.Command {
	var namespace, legacyNamespace, kubeContext, output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List visible Tau workspaces",
		Long: `List every TauWorkspace visible in the platform system namespace. Use -o
json for machine-readable output when scripting.`,
		Example: `  tau workspace list
  tau workspace list --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			namespace, err = resolveSystemNamespaceAlias(cmd, namespace, "namespace", legacyNamespace)
			if err != nil {
				return err
			}
			if output != "table" && output != "json" {
				return fmt.Errorf("-o/--output must be one of: table, json")
			}
			resolvedContext, restore, err := resolveWorkspaceControlPlaneConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			raw, err := r.Raw(cmd.Context(), []string{"-n", namespace, "get", "workspaces.tau.azure.com", "-o", "json"}, nil)
			if err != nil {
				return describeWorkspaceLookupError(err, resolvedContext)
			}
			list, err := tauworkspace.ParseList([]byte(raw))
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), list)
			}
			fmt.Fprint(cmd.OutOrStdout(), tauworkspace.RenderList(list))
			return nil
		},
	}
	cmd.Flags().StringVar(&namespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().StringVarP(&legacyNamespace, "namespace", "n", "", "deprecated alias for --system-namespace")
	_ = cmd.Flags().MarkDeprecated("namespace", "use --system-namespace")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table (human-readable) or json (machine-readable)")
	return cmd
}

func newWorkspaceStatusCmd(check bool) *cobra.Command {
	var namespace, legacyNamespace, kubeContext, output, dataPVC string
	use := "status <name>"
	short := "Show Tau workspace status"
	invocation := "tau workspace status"
	long := `Show the TauWorkspace status for NAME: phase, namespace, LocalQueue, and
Service Account.`
	if check {
		use = "check <name>"
		short = "Show Tau workspace status and exit non-zero unless it is Ready"
		invocation = "tau workspace check"
		long = `Show the TauWorkspace status for NAME, then exit non-zero unless it is
Ready. With --data-pvc, also warns if the named durable data PVC is missing so
a storage-backed run fails fast with a clear diagnostic.`
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Example: `  ` + invocation + ` research-team
  ` + invocation + ` research-team --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			namespace, err = resolveSystemNamespaceAlias(cmd, namespace, "namespace", legacyNamespace)
			if err != nil {
				return err
			}
			if output != "table" && output != "json" {
				return fmt.Errorf("-o/--output must be one of: table, json")
			}
			resolvedContext, restore, err := resolveWorkspaceControlPlaneConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			workspace, err := fetchWorkspace(cmd, resolvedContext, namespace, args[0])
			if err != nil {
				return err
			}
			if output == "json" {
				if err := writeJSON(cmd.OutOrStdout(), tauworkspace.CLIView(workspace)); err != nil {
					return err
				}
			} else {
				fmt.Fprint(cmd.OutOrStdout(), tauworkspace.RenderStatus(workspace))
			}
			if check {
				if !tauworkspace.Ready(workspace) {
					return fmt.Errorf("workspace %q is not Ready (phase=%s)", args[0], workspace.Status.Phase)
				}
				out := cmd.ErrOrStderr()
				if output == "table" {
					out = cmd.OutOrStdout()
				}
				if err := reportWorkspaceDataPVC(
					cmd.Context(),
					kube.New(resolvedContext),
					out,
					workspace,
					dataPVC,
				); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&namespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().StringVarP(&legacyNamespace, "namespace", "n", "", "deprecated alias for --system-namespace")
	_ = cmd.Flags().MarkDeprecated("namespace", "use --system-namespace")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table (human-readable) or json (machine-readable)")
	if check {
		cmd.Flags().StringVar(&dataPVC, "data-pvc", "", "durable /data PVC to verify is Bound in the workspace namespace")
	}
	return cmd
}

func workspaceTargetNamespace(w tauworkspace.Workspace) string {
	return tauworkspace.ResolvedNamespace(w)
}

func reportWorkspaceDataPVC(
	ctx context.Context,
	runner validateNodesRunner,
	out io.Writer,
	w tauworkspace.Workspace,
	pvcName string,
) error {
	pvcName = strings.TrimSpace(pvcName)
	targetNS := workspaceTargetNamespace(w)
	if pvcName == "" || targetNS == "" {
		return nil
	}

	raw, err := runner.Raw(ctx, []string{"-n", targetNS, "get", "pvc", pvcName, "-o", "json"}, nil)
	if err != nil {
		if !isPVCNotFoundError(err) {
			return fmt.Errorf("cannot verify durable /data PVC %s/%s: %s", targetNS, pvcName, firstErrorLine(err))
		}
		fmt.Fprintf(out, "\nwarning: durable /data PVC %q not found in namespace %q.\n", pvcName, targetNS)
		fmt.Fprintf(out, "  Configs that set storage.data_pvc will stay Pending until the platform provisions it.\n")
		fmt.Fprintf(out, "  CPU configs that do not mount /data are unaffected.\n")
		return nil
	}
	var doc pvcDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("cannot verify durable /data PVC %s/%s: unexpected kubectl output: %w", targetNS, pvcName, err)
	}
	if phase := strings.TrimSpace(doc.Status.Phase); phase != "Bound" {
		fmt.Fprintf(out, "\nwarning: durable /data PVC %s/%s is %s, not Bound.\n", targetNS, pvcName, dash(phase))
		fmt.Fprintf(out, "  Storage-backed configs will not start until it binds.\n")
	}
	return nil
}

func isPVCNotFoundError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "notfound") ||
		strings.Contains(strings.ToLower(err.Error()), "not found")
}

func firstErrorLine(err error) string {
	text := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[:i])
	}
	return text
}

func newWorkspaceQuotaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Inspect and request Tau workspace quota",
		Long: `Inspect a workspace's current quota decision and create structured
requests to raise it. Quota is platform-owned policy; request creates a
QuotaRequest object for the platform owner to review, it does not grant quota
directly.`,
		Example: `  tau workspace quota show research-team
  tau workspace quota request research-team --resource gpu-h200 --requested 8 --reason "scale up training"`,
		Args: cobra.NoArgs,
		RunE: showGroupHelp,
	}
	cmd.AddCommand(newWorkspaceQuotaRequestCmd())
	cmd.AddCommand(newWorkspaceQuotaShowCmd())
	return cmd
}

func newWorkspaceQuotaRequestCmd() *cobra.Command {
	var namespace, legacyNamespace, kubeContext, resource, duration, reason, mutationMode string
	var current, requested int64
	var apply bool
	cmd := &cobra.Command{
		Use:   "request <workspace>",
		Short: "Create a structured workspace quota request",
		Long: `Create a structured QuotaRequest object recording a request to raise a
workspace's quota for one resource. This does not grant quota; the platform
owner reviews and applies the request out of band.`,
		Example: `  tau workspace quota request research-team --resource gpu-h200 --current 4 \
      --requested 8 --duration 720h --reason "scale up training" --apply
  tau workspace quota request research-team --resource gpu-h200 --requested 8 \
      --reason "scale up training" > quota-request.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			namespace, err = resolveSystemNamespaceAlias(cmd, namespace, "namespace", legacyNamespace)
			if err != nil {
				return err
			}
			if resource == "" {
				return fmt.Errorf("--resource is required")
			}
			if requested <= 0 {
				return fmt.Errorf("--requested must be > 0")
			}
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			if mutationMode != "" && mutationMode != "ReportOnly" && mutationMode != "ControllerManagedBurst" {
				return fmt.Errorf("--mutation-mode must be one of: ReportOnly, ControllerManagedBurst")
			}
			workspace := args[0]
			name := workspace + "-" + resource + "-quota-request"
			request := tauworkspace.NewQuotaRequest(name, namespace, tauworkspace.QuotaRequestSpec{
				Workspace:    workspace,
				Resource:     resource,
				Current:      current,
				Requested:    requested,
				Duration:     duration,
				Reason:       reason,
				MutationMode: mutationMode,
			})
			manifest, err := yaml.Marshal(request)
			if err != nil {
				return err
			}
			if !apply {
				_, err := cmd.OutOrStdout().Write(manifest)
				return err
			}
			resolvedContext, restore, err := resolveWorkspaceControlPlaneConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			out, err := r.Raw(cmd.Context(), []string{"apply", "-n", namespace, "-f", "-"}, manifest)
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&namespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().StringVarP(&legacyNamespace, "namespace", "n", "", "deprecated alias for --system-namespace")
	_ = cmd.Flags().MarkDeprecated("namespace", "use --system-namespace")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&resource, "resource", "", "quota resource being requested, e.g. h200")
	cmd.Flags().Int64Var(&current, "current", 0, "current workspace quota for the resource")
	cmd.Flags().Int64Var(&requested, "requested", 0, "requested workspace quota for the resource")
	cmd.Flags().StringVar(&duration, "duration", "", "requested duration, e.g. 14d")
	cmd.Flags().StringVar(&reason, "reason", "", "reason for the quota increase")
	cmd.Flags().StringVar(&mutationMode, "mutation-mode", "ReportOnly", "quota mutation mode: ReportOnly|ControllerManagedBurst")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the request instead of printing YAML")
	return cmd
}

func fetchWorkspace(cmd *cobra.Command, kubeContext, namespace, name string) (tauworkspace.Workspace, error) {
	r := kube.New(kubeContext)
	raw, err := r.Raw(cmd.Context(), []string{"-n", namespace, "get", "workspaces.tau.azure.com", name, "-o", "json"}, nil)
	if err != nil {
		return tauworkspace.Workspace{}, describeWorkspaceLookupError(err, kubeContext)
	}
	return tauworkspace.Parse([]byte(raw))
}

func describeWorkspaceLookupError(err error, kubeContext string) error {
	if err == nil || !isMissingTauCRDError(err) {
		return err
	}
	where := "the current kubectl context"
	if ctx := strings.TrimSpace(kubeContext); ctx != "" {
		where = fmt.Sprintf("kubectl context %q", ctx)
	}
	return fmt.Errorf(
		"%s has no %s resources, so it does not appear to be a TauGrid cluster; "+
			"check your repository's tau/workspace.connection.yaml or pass --context: %w",
		where,
		tauworkspace.KindWorkspace,
		err,
	)
}

func isMissingTauCRDError(err error) bool {
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "workspace") {
		return false
	}
	return strings.Contains(text, `server doesn't have a resource type`) ||
		strings.Contains(text, "the server could not find the requested resource") ||
		strings.Contains(text, "no matches for kind")
}
