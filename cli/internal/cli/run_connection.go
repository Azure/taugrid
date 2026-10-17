// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Azure/taugrid/cli/internal/clusteraccess"
	"github.com/Azure/taugrid/cli/internal/projectcatalog"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/version"
)

type runConnectionEnsurer interface {
	Ensure(context.Context, string) (workspaceconnection.ActiveConnection, error)
}

type exactRunConnectionEnsurer interface {
	EnsureDiscovery(context.Context, workspaceconnection.Discovery) (workspaceconnection.ActiveConnection, error)
}

type runConnectionEnsurerFactory func(*cobra.Command) runConnectionEnsurer

type runLifecycleConnectionFlags struct {
	namespace       string
	workspace       string
	kubeContext     string
	systemNamespace string
}

func (f *runLifecycleConnectionFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "TauWorkspace name to resolve the run namespace")
	cmd.Flags().StringVar(&f.kubeContext, "context", defaultKubeContext(), kubeContextHelp())
}

func lifecycleFlagsContextExplicit(cmd *cobra.Command) bool {
	return runContextExplicit(cmd)
}

func (f *runLifecycleConnectionFlags) resolve(cmd *cobra.Command) (string, string, func(), error) {
	return f.resolveWithEnsurer(cmd, defaultRunLifecycleConnectionManager(cmd))
}

func (f *runLifecycleConnectionFlags) resolveWithEnsurer(cmd *cobra.Command, underlying runConnectionEnsurer) (string, string, func(), error) {
	f.systemNamespace = systemNamespaceFromCommand(cmd)
	ensurer := &observedRunConnectionEnsurer{
		underlying: underlying,
		connected: func(connection workspaceconnection.ActiveConnection) {
			f.systemNamespace = systemNamespaceForConnection(cmd, connection)
		},
	}
	return resolveRunLifecycleConnectionWithWorkspaceUsing(
		cmd,
		f.kubeContext,
		f.namespace,
		f.workspace,
		lifecycleFlagsContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
		cmd.Flags().Changed("workspace"),
		func(cmd *cobra.Command, kubeContext, namespace string, contextExplicit, namespaceExplicit, workspaceRouting bool) (string, string, func(), error) {
			return resolveRunLifecycleConnectionUsing(
				cmd,
				kubeContext,
				namespace,
				contextExplicit,
				namespaceExplicit,
				workspaceRouting,
				ensurer,
			)
		},
		func(cmd *cobra.Command, kubeContext, _ string, name string) (tauworkspace.Workspace, error) {
			return fetchWorkspace(cmd, kubeContext, f.systemNamespace, name)
		},
	)
}

type observedRunConnectionEnsurer struct {
	underlying runConnectionEnsurer
	connected  func(workspaceconnection.ActiveConnection)
}

func (e *observedRunConnectionEnsurer) Ensure(ctx context.Context, startDir string) (workspaceconnection.ActiveConnection, error) {
	connection, err := e.underlying.Ensure(ctx, startDir)
	if err == nil {
		e.connected(connection)
	}
	return connection, err
}

func (e *observedRunConnectionEnsurer) EnsureDiscovery(ctx context.Context, discovery workspaceconnection.Discovery) (workspaceconnection.ActiveConnection, error) {
	connection, err := ensureRunConnection(ctx, e.underlying, runConnectionSource{Discovery: &discovery})
	if err == nil {
		e.connected(connection)
	}
	return connection, err
}

type runLifecycleBaseResolver func(
	*cobra.Command,
	string,
	string,
	bool,
	bool,
	bool,
) (string, string, func(), error)

type runLifecycleWorkspaceFetcher func(
	*cobra.Command,
	string,
	string,
	string,
) (tauworkspace.Workspace, error)

func resolveRunLifecycleConnectionWithWorkspaceUsing(
	cmd *cobra.Command,
	kubeContext, namespace, workspace string,
	contextExplicit, namespaceExplicit, workspaceExplicit bool,
	resolveConnection runLifecycleBaseResolver,
	fetch runLifecycleWorkspaceFetcher,
) (string, string, func(), error) {
	workspaceName := strings.TrimSpace(workspace)
	useWorkspace := workspaceExplicit && workspaceName != ""

	routingNamespace := namespace
	if useWorkspace {
		if strings.TrimSpace(routingNamespace) == "" {
			routingNamespace = workspaceName
		}
	}

	resolvedContext, resolvedNamespace, restore, err := resolveConnection(
		cmd,
		kubeContext,
		routingNamespace,
		contextExplicit,
		namespaceExplicit,
		useWorkspace,
	)
	if err != nil {
		return "", "", nil, err
	}
	if !useWorkspace {
		return resolvedContext, resolvedNamespace, restore, nil
	}

	workspaceStatus, err := fetch(cmd, resolvedContext, systemNamespaceFromCommand(cmd), workspaceName)
	if err != nil {
		restore()
		return "", "", nil, err
	}
	workspaceNamespace := firstNonEmpty(
		workspaceTargetNamespace(workspaceStatus),
		workspaceStatus.Metadata.Name,
	)
	if explicitNamespace := strings.TrimSpace(namespace); namespaceExplicit &&
		explicitNamespace != "" &&
		explicitNamespace != workspaceNamespace {
		restore()
		return "", "", nil, fmt.Errorf(
			"namespace %q conflicts with TauWorkspace %q target namespace %q",
			explicitNamespace,
			workspaceStatus.Metadata.Name,
			workspaceNamespace,
		)
	}
	return resolvedContext, workspaceNamespace, restore, nil
}

func defaultRunConnectionEnsurer(cmd *cobra.Command) runConnectionEnsurer {
	return defaultRunConnectionManager(cmd)
}

func defaultRunConnectionManager(cmd *cobra.Command) workspaceconnection.Manager {
	authMode := strings.TrimSpace(os.Getenv("TAU_AUTH_MODE"))
	credentialFactory := clusteraccess.UserCredentialFactory{
		Mode:   authMode,
		Output: cmd.ErrOrStderr(),
	}
	credentialProvider := clusteraccess.NewDefaultProvider(
		clusteraccess.AKSUserCredentialProvider{
			Credentials: credentialFactory,
			AuthMode:    authMode,
		},
	)
	return workspaceconnection.Manager{
		ConfigDir:   strings.TrimSpace(os.Getenv("TAU_CONFIG_DIR")),
		Interactive: stdinIsTerminal(cmd.InOrStdin()),
		Input:       cmd.InOrStdin(),

		Output:      cmd.OutOrStdout(),
		Credentials: credentialProvider,
		Verifier:    workspaceconnection.KubectlVerifier{},
		TauVersion:  version.Version,
	}
}

func defaultRunLifecycleConnectionManager(cmd *cobra.Command) workspaceconnection.Manager {
	manager := defaultRunConnectionManager(cmd)
	manager.AllowUnreadyWorkspace = true
	manager.Verifier = workspaceconnection.KubectlVerifier{AllowUnreadyWorkspace: true}
	return manager
}

func stdinIsTerminal(input io.Reader) bool {
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func applyAutomaticRunConnection(
	ctx context.Context,
	options unresolvedRunOptions,
	source runConnectionSource,
	required bool,
	ensurer runConnectionEnsurer,
) (unresolvedRunOptions, workspaceconnection.ActiveConnection, error) {
	if options.dryRun == "client" {
		return applyOfflineRunConnection(options, source)
	}
	return applyActivatedRunConnection(ctx, options, source, required, ensurer)
}

func applyLiveRunConnection(
	ctx context.Context,
	options unresolvedRunOptions,
	source runConnectionSource,
	ensurer runConnectionEnsurer,
) (unresolvedRunOptions, workspaceconnection.ActiveConnection, error) {
	discovery := descriptorFor(source)
	if !source.Catalog && discovery == nil && (options.workspace != "" || options.kubeContext != "") {
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if err := checkDescriptorContextConflict(options.kubeContext, options.kubeContextFromFlag, discovery); err != nil {
		return options, workspaceconnection.ActiveConnection{}, err
	}
	if !options.workspaceExplicit {
		if err := checkCatalogWorkspaceConflict(options, source, source.Discovery); err != nil {
			return options, workspaceconnection.ActiveConnection{}, err
		}
	}
	connection, err := ensureRunConnection(ctx, ensurer, source)
	if err != nil {
		return options, workspaceconnection.ActiveConnection{}, err
	}
	expectedWorkspace := workspaceForSource(source, connection.Workspace)
	if requested := strings.TrimSpace(options.workspace); requested != "" && requested != expectedWorkspace {
		if strings.TrimSpace(source.Workspace) == "" {
			return options, workspaceconnection.ActiveConnection{}, fmt.Errorf(
				"run workspace %q conflicts with active repository workspace connection %q",
				requested,
				expectedWorkspace,
			)
		}
		return options, workspaceconnection.ActiveConnection{}, fmt.Errorf(
			"run workspace %q conflicts with repository workspace %q",
			requested,
			expectedWorkspace,
		)
	}
	if requested, connected := strings.TrimSpace(options.kubeContext), strings.TrimSpace(connection.ContextName); requested != "" && connected != "" && requested != connected {
		return options, workspaceconnection.ActiveConnection{}, fmt.Errorf(
			"run context %q conflicts with active repository workspace connection context %q",
			requested,
			connected,
		)
	}
	connection = connectionForWorkspaceSelection(connection, expectedWorkspace)
	options.workspace = expectedWorkspace
	options.kubeContext = connection.ContextName
	return options, connection, nil
}

func connectionForWorkspaceSelection(
	connection workspaceconnection.ActiveConnection,
	workspace string,
) workspaceconnection.ActiveConnection {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || workspace == strings.TrimSpace(connection.Workspace) {
		return connection
	}
	connection.Workspace = workspace
	connection.WorkspaceUID = ""
	connection.Namespace = ""
	connection.Queue = ""
	return connection
}

func applyActivatedRunConnection(
	ctx context.Context,
	options unresolvedRunOptions,
	source runConnectionSource,
	required bool,
	ensurer runConnectionEnsurer,
) (unresolvedRunOptions, workspaceconnection.ActiveConnection, error) {
	if options.kubeContextExplicit ||
		(options.workspaceExplicit && strings.TrimSpace(source.Workspace) == "") {
		if err := checkDescriptorContextConflict(options.kubeContext, options.kubeContextFromFlag, descriptorFor(source)); err != nil {
			return options, workspaceconnection.ActiveConnection{}, err
		}
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if !source.Catalog && (options.workspace != "" || options.kubeContext != "") {
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if err := checkCatalogWorkspaceConflict(options, source, source.Discovery); err != nil {
		return options, workspaceconnection.ActiveConnection{}, err
	}
	connection, err := ensureRunConnection(ctx, ensurer, source)
	if err != nil {
		if !required && errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
			return options, workspaceconnection.ActiveConnection{}, nil
		}
		return options, workspaceconnection.ActiveConnection{}, err
	}
	options.workspace = workspaceForSource(source, connection.Workspace)
	options.kubeContext = connection.ContextName
	return options, connection, nil
}

func applyOfflineRunConnection(
	options unresolvedRunOptions,
	source runConnectionSource,
) (unresolvedRunOptions, workspaceconnection.ActiveConnection, error) {
	if options.kubeContextExplicit ||
		(options.workspaceExplicit && strings.TrimSpace(source.Workspace) == "") {
		if err := checkDescriptorContextConflict(options.kubeContext, options.kubeContextFromFlag, descriptorFor(source)); err != nil {
			return options, workspaceconnection.ActiveConnection{}, err
		}
		return options, workspaceconnection.ActiveConnection{}, nil
	}

	discovery := source.Discovery
	if discovery == nil && strings.TrimSpace(source.StartDir) != "" {
		found, err := workspaceconnection.Discover(source.StartDir)
		if err != nil {
			if errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
				return options, workspaceconnection.ActiveConnection{}, nil
			}
			return options, workspaceconnection.ActiveConnection{}, err
		}
		discovery = &found
	}
	if discovery == nil {
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if err := checkCatalogWorkspaceConflict(options, source, discovery); err != nil {
		return options, workspaceconnection.ActiveConnection{}, err
	}
	if options.workspace == "" {
		options.workspace = workspaceForSource(source, discovery.Descriptor.Workspace)
	}
	if options.kubeContext == "" {
		options.kubeContext = discovery.Descriptor.Cluster.ContextName
	}
	return options, workspaceconnection.ActiveConnection{
		Workspace:   discovery.Descriptor.Workspace,
		ContextName: discovery.Descriptor.Cluster.ContextName,
	}, nil
}

func checkCatalogWorkspaceConflict(options unresolvedRunOptions, source runConnectionSource, discovery *workspaceconnection.Discovery) error {
	if !source.Catalog || options.workspace == "" {
		return nil
	}
	expected := strings.TrimSpace(source.Workspace)
	if expected == "" && discovery != nil {
		expected = strings.TrimSpace(discovery.Descriptor.Workspace)
	}
	if expected == "" || options.workspace == expected {
		return nil
	}
	project := ""
	if source.Project != "" {
		project = fmt.Sprintf(" for project %q", source.Project)
	}
	return fmt.Errorf(
		"run config policy.workspace %q conflicts with catalog workspace %q%s",
		options.workspace,
		expected,
		project,
	)
}

func workspaceForSource(source runConnectionSource, fallback string) string {
	if workspace := strings.TrimSpace(source.Workspace); workspace != "" {
		return workspace
	}
	return strings.TrimSpace(fallback)
}

func validateWorkspaceSelection(source runConnectionSource, policyWorkspace, flagWorkspace string) error {
	policyWorkspace = strings.TrimSpace(policyWorkspace)
	flagWorkspace = strings.TrimSpace(flagWorkspace)
	projectWorkspace := strings.TrimSpace(source.Workspace)
	if !source.Catalog || projectWorkspace == "" {
		return nil
	}
	if policyWorkspace != "" && flagWorkspace != "" && policyWorkspace != flagWorkspace {
		return fmt.Errorf(
			"--workspace %q conflicts with run config policy.workspace %q",
			flagWorkspace,
			policyWorkspace,
		)
	}
	if policyWorkspace != "" && policyWorkspace != projectWorkspace {
		return fmt.Errorf(
			"run config policy.workspace %q conflicts with catalog workspace %q for project %q",
			policyWorkspace,
			projectWorkspace,
			source.Project,
		)
	}
	if flagWorkspace != "" && flagWorkspace != projectWorkspace {
		return fmt.Errorf(
			"--workspace %q conflicts with catalog workspace %q for project %q",
			flagWorkspace,
			projectWorkspace,
			source.Project,
		)
	}
	return nil
}

func descriptorFor(source runConnectionSource) *workspaceconnection.Discovery {
	if source.Discovery != nil {
		return source.Discovery
	}
	startDir := strings.TrimSpace(source.StartDir)
	if startDir == "" {
		return nil
	}
	_, discovery, _, err := effectiveWorkspaceConnection(startDir, source.Project)
	if err != nil {
		return nil
	}
	return &discovery
}

func descriptorForStartDir() *workspaceconnection.Discovery {
	startDir, err := os.Getwd()
	if err != nil {
		return nil
	}
	return descriptorFor(runConnectionSource{StartDir: startDir})
}

func ensureRunConnection(
	ctx context.Context,
	ensurer runConnectionEnsurer,
	source runConnectionSource,
) (workspaceconnection.ActiveConnection, error) {
	if source.Discovery == nil {
		return ensurer.Ensure(ctx, source.StartDir)
	}
	exact, ok := ensurer.(exactRunConnectionEnsurer)
	if !ok {
		return workspaceconnection.ActiveConnection{}, fmt.Errorf("run connection ensurer does not support exact catalog descriptors")
	}
	return exact.EnsureDiscovery(ctx, *source.Discovery)
}

func useKubeconfig(path string) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return func() {}, nil
	}

	previous, existed := os.LookupEnv("KUBECONFIG")
	if err := os.Setenv("KUBECONFIG", path); err != nil {
		return nil, err
	}
	return func() {
		if existed {
			_ = os.Setenv("KUBECONFIG", previous)
		} else {
			_ = os.Unsetenv("KUBECONFIG")
		}
	}, nil
}

func resolveRunLifecycleConnection(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit bool,
	namespaceExplicit bool,
) (string, string, func(), error) {
	return resolveRunLifecycleConnectionUsing(
		cmd,
		kubeContext,
		namespace,
		contextExplicit,
		namespaceExplicit,
		false,
		defaultRunLifecycleConnectionManager(cmd),
	)
}

func resolveRunLifecycleConnectionUsing(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit, namespaceExplicit, workspaceRouting bool,
	ensurer runConnectionEnsurer,
) (string, string, func(), error) {
	projectName := ""
	if cmd.Flags().Lookup("project") != nil {
		var err error
		projectName, err = cmd.Flags().GetString("project")
		if err != nil {
			return "", "", nil, fmt.Errorf("read inherited --project flag: %w", err)
		}
	}
	return resolveRunLifecycleConnectionWithEnsurer(
		cmd,
		kubeContext,
		namespace,
		contextExplicit,
		namespaceExplicit,
		workspaceRouting,
		projectName,
		ensurer,
	)
}

func resolveRunLifecycleConnectionWithEnsurer(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit bool,
	namespaceExplicit bool,
	workspaceRouting bool,
	projectName string,
	ensurer runConnectionEnsurer,
) (string, string, func(), error) {
	if contextExplicit && kubeContext != "" && strings.TrimSpace(projectName) == "" {
		source, err := discoverRunLifecycleConnectionSource(projectName)
		if err == nil {
			return resolveRunLifecycleConnectionFromSource(
				cmd,
				kubeContext,
				namespace,
				contextExplicit,
				namespaceExplicit,
				workspaceRouting,
				source,
				ensurer,
			)
		}
		if !strings.Contains(err.Error(), "lifecycle routing is ambiguous") {
			return "", "", nil, err
		}
		if err := checkDescriptorContextConflict(kubeContext, cmd.Flags().Changed("context"), descriptorForStartDir()); err != nil {
			return "", "", nil, err
		}
		ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
		if err != nil {
			return "", "", nil, err
		}
		return kubeContext, ns, func() {}, nil
	}
	source, err := discoverRunLifecycleConnectionSource(projectName)
	if err != nil {
		return "", "", nil, err
	}
	return resolveRunLifecycleConnectionFromSource(
		cmd,
		kubeContext,
		namespace,
		contextExplicit,
		namespaceExplicit,
		workspaceRouting,
		source,
		ensurer,
	)
}

func discoverRunLifecycleConnectionSource(projectName string) (runConnectionSource, error) {
	startDir, err := os.Getwd()
	if err != nil {
		return runConnectionSource{}, err
	}
	repository, err := projectcatalog.Discover(startDir)
	if err != nil {
		return runConnectionSource{}, err
	}
	source := runConnectionSource{
		StartDir: startDir,
		Git:      repository.Boundary.Git,
	}
	if repository.Catalog != nil {
		project, err := repository.Catalog.SelectLifecycleProject(projectName, startDir)
		if err != nil {
			return runConnectionSource{}, err
		}
		source.Catalog = true
		source.Project = project.Name
		source.Workspace = project.Workspace
		discovery, err := assignedCatalogProjectConnection(repository.Catalog, project)
		if err != nil {
			return runConnectionSource{}, err
		}
		source.Discovery = discovery
	} else if strings.TrimSpace(projectName) != "" {
		return runConnectionSource{}, fmt.Errorf("--project requires %s at the Git worktree root", projectcatalog.Filename)
	}
	return source, nil
}

func resolveRunLifecycleConnectionFromSource(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit bool,
	namespaceExplicit bool,
	workspaceRouting bool,
	source runConnectionSource,
	ensurer runConnectionEnsurer,
) (string, string, func(), error) {
	if contextExplicit && kubeContext != "" {
		if err := checkDescriptorContextConflict(kubeContext, cmd.Flags().Changed("context"), descriptorFor(source)); err != nil {
			return "", "", nil, err
		}
		if workspaceName := strings.TrimSpace(source.Workspace); workspaceName != "" {
			placement, err := fetchLifecycleWorkspacePlacement(
				cmd,
				kubeContext,
				systemNamespaceFromCommand(cmd),
				workspaceName,
				workspaceconnection.ActiveConnection{Workspace: workspaceName, ContextName: kubeContext},
			)
			if err != nil {
				return "", "", nil, err
			}
			if explicitNamespace := strings.TrimSpace(namespace); namespaceExplicit &&
				explicitNamespace != "" &&
				explicitNamespace != placement.Namespace {
				return "", "", nil, fmt.Errorf(
					"namespace %q conflicts with TauWorkspace %q target namespace %q",
					explicitNamespace,
					placement.Workspace,
					placement.Namespace,
				)
			}
			return kubeContext, placement.Namespace, func() {}, nil
		}
		ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
		if err != nil {
			return "", "", nil, err
		}
		return kubeContext, ns, func() {}, nil
	}
	if !source.Git {
		if namespaceExplicit || workspaceRouting {
			ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
			if err != nil {
				return "", "", nil, err
			}
			return kubeContext, ns, func() {}, nil
		}
		return "", "", nil, fmt.Errorf("lifecycle commands outside a Git repository require explicit --context or --namespace")
	}
	connection, err := ensureRunConnection(cmd.Context(), ensurer, source)
	if err != nil {
		if !source.Catalog && errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
			ns, nsErr := resolveWorkloadNamespace(cmd, kubeContext, namespace)
			if nsErr != nil {
				return "", "", nil, nsErr
			}
			return kubeContext, ns, func() {}, nil
		}
		return "", "", nil, err
	}
	restore, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return "", "", nil, err
	}
	if workspaceName := strings.TrimSpace(source.Workspace); workspaceName != "" {
		selectedConnection := connectionForWorkspaceSelection(connection, workspaceName)
		placement, placementErr := fetchLifecycleWorkspacePlacement(
			cmd,
			connection.ContextName,
			systemNamespaceForConnection(cmd, connection),
			workspaceName,
			selectedConnection,
		)
		if placementErr != nil {
			restore()
			return "", "", nil, placementErr
		}
		if explicitNamespace := strings.TrimSpace(namespace); namespaceExplicit &&
			explicitNamespace != "" &&
			explicitNamespace != placement.Namespace {
			restore()
			return "", "", nil, fmt.Errorf(
				"namespace %q conflicts with TauWorkspace %q target namespace %q",
				explicitNamespace,
				placement.Workspace,
				placement.Namespace,
			)
		}
		namespace = placement.Namespace
	} else if !namespaceExplicit {
		namespace = connection.Namespace
	}
	resolvedNamespace, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
	if err != nil {
		restore()
		return "", "", nil, err
	}
	return connection.ContextName, resolvedNamespace, restore, nil
}

var runLifecycleWorkspaceFetcherOverride runLifecycleWorkspaceFetcher

func fetchLifecycleWorkspacePlacement(
	cmd *cobra.Command,
	kubeContext, systemNamespace, workspaceName string,
	connection workspaceconnection.ActiveConnection,
) (workspacePlacement, error) {
	fetch := fetchWorkspace
	if runLifecycleWorkspaceFetcherOverride != nil {
		fetch = runLifecycleWorkspaceFetcherOverride
	}
	workspaceStatus, err := fetch(cmd, kubeContext, systemNamespace, workspaceName)
	if err != nil {
		return workspacePlacement{}, err
	}
	return resolveExistingWorkspacePlacement(workspaceStatus, connection)
}

func fetchReadyWorkspacePlacement(
	cmd *cobra.Command,
	kubeContext, systemNamespace, workspaceName string,
	connection workspaceconnection.ActiveConnection,
) (workspacePlacement, error) {
	fetch := fetchWorkspace
	if runLifecycleWorkspaceFetcherOverride != nil {
		fetch = runLifecycleWorkspaceFetcherOverride
	}
	workspaceStatus, err := fetch(cmd, kubeContext, systemNamespace, workspaceName)
	if err != nil {
		return workspacePlacement{}, err
	}
	return resolveWorkspacePlacement(workspaceStatus, connection)
}

func resolveWorkspaceControlPlaneConnection(
	cmd *cobra.Command,
	kubeContext, namespace string,
) (string, func(), error) {
	return resolveWorkspaceControlPlaneConnectionWithEnsurer(
		cmd,
		kubeContext,
		namespace,
		defaultRunLifecycleConnectionManager(cmd),
	)
}

func resolveWorkspaceControlPlaneConnectionWithEnsurer(
	cmd *cobra.Command,
	kubeContext, namespace string,
	ensurer runConnectionEnsurer,
) (string, func(), error) {
	_ = namespace
	source, err := discoverRunLifecycleConnectionSource("")
	if err != nil {
		if runContextExplicit(cmd) &&
			strings.TrimSpace(kubeContext) != "" &&
			strings.Contains(err.Error(), "lifecycle routing is ambiguous") {
			if err := checkDescriptorContextConflict(
				kubeContext,
				cmd.Flags().Changed("context"),
				descriptorForStartDir(),
			); err != nil {
				return "", nil, err
			}
			return kubeContext, func() {}, nil
		}
		return "", nil, err
	}
	if runContextExplicit(cmd) && strings.TrimSpace(kubeContext) != "" {
		if err := checkDescriptorContextConflict(
			kubeContext,
			cmd.Flags().Changed("context"),
			descriptorFor(source),
		); err != nil {
			return "", nil, err
		}
		return kubeContext, func() {}, nil
	}
	if !source.Git {
		return kubeContext, func() {}, nil
	}
	connection, err := ensureRunConnection(cmd.Context(), ensurer, source)
	if err != nil {
		if !source.Catalog && errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
			return kubeContext, func() {}, nil
		}
		return "", nil, err
	}
	restore, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return "", nil, err
	}
	return connection.ContextName, restore, nil
}

func resolveWorkloadDataConnection(cmd *cobra.Command, kubeContext, namespace string) (string, string, func(), error) {
	return resolveRunLifecycleConnection(
		cmd,
		kubeContext,
		namespace,
		runContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
	)
}
