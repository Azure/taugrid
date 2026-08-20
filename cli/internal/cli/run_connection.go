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
	namespace   string
	workspace   string
	kubeContext string
}

func (f *runLifecycleConnectionFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "TauWorkspace name to resolve the run namespace")
	cmd.Flags().StringVar(&f.kubeContext, "context", defaultKubeContext(), kubeContextHelp())
}

// lifecycleFlagsContextExplicit is what resolve passes as contextExplicit,
// named so a guard can assert on it without driving the real connection ensurer
// that resolve wires in. Keeping it a function rather than inlining the call
// means the two cannot drift.
func lifecycleFlagsContextExplicit(cmd *cobra.Command) bool {
	return runContextExplicit(cmd)
}

func (f *runLifecycleConnectionFlags) resolve(cmd *cobra.Command) (string, string, func(), error) {
	return resolveRunLifecycleConnectionWithWorkspace(
		cmd,
		f.kubeContext,
		f.namespace,
		f.workspace,
		lifecycleFlagsContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
		cmd.Flags().Changed("workspace"),
	)
}

type runLifecycleBaseResolver func(
	*cobra.Command,
	string,
	string,
	bool,
	bool,
) (string, string, func(), error)

type runLifecycleWorkspaceFetcher func(
	*cobra.Command,
	string,
	string,
	string,
) (tauworkspace.Workspace, error)

func resolveRunLifecycleConnectionWithWorkspace(
	cmd *cobra.Command,
	kubeContext, namespace, workspace string,
	contextExplicit, namespaceExplicit, workspaceExplicit bool,
) (string, string, func(), error) {
	return resolveRunLifecycleConnectionWithWorkspaceUsing(
		cmd,
		kubeContext,
		namespace,
		workspace,
		contextExplicit,
		namespaceExplicit,
		workspaceExplicit,
		resolveRunLifecycleConnection,
		fetchWorkspace,
	)
}

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
	routingNamespaceExplicit := namespaceExplicit
	if useWorkspace {
		routingNamespaceExplicit = true
		if strings.TrimSpace(routingNamespace) == "" {
			// A named workspace is sufficient routing information even outside a
			// repository. The value is replaced by the workspace's target after
			// the connection and kubeconfig are resolved.
			routingNamespace = workspaceName
		}
	}

	resolvedContext, resolvedNamespace, restore, err := resolveConnection(
		cmd,
		kubeContext,
		routingNamespace,
		contextExplicit,
		routingNamespaceExplicit,
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
	if !tauworkspace.Ready(workspaceStatus) {
		restore()
		return "", "", nil, fmt.Errorf(
			"workspace %q is not Ready (phase=%s)",
			workspaceStatus.Metadata.Name,
			workspaceStatus.Status.Phase,
		)
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
	authMode := strings.TrimSpace(os.Getenv("TAU_AUTH_MODE"))
	credentialFactory := clusteraccess.UserCredentialFactory{
		Mode:   authMode,
		Output: cmd.ErrOrStderr(),
	}
	return workspaceconnection.Manager{
		ConfigDir:   strings.TrimSpace(os.Getenv("TAU_CONFIG_DIR")),
		Interactive: stdinIsTerminal(cmd.InOrStdin()),
		Input:       cmd.InOrStdin(),

		Output: cmd.OutOrStdout(),
		Credentials: clusteraccess.AKSUserCredentialProvider{
			Credentials: credentialFactory,
			AuthMode:    authMode,
		},
		Verifier:   workspaceconnection.KubectlVerifier{},
		TauVersion: version.Version,
	}
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
	if options.workspaceExplicit || options.kubeContextExplicit {
		if err := checkDescriptorContextConflict(options.kubeContext, options.kubeContextFromFlag, descriptorFor(source)); err != nil {
			return options, workspaceconnection.ActiveConnection{}, err
		}
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if !source.Catalog && (options.workspace != "" || options.kubeContext != "") {
		return options, workspaceconnection.ActiveConnection{}, nil
	}
	if source.Catalog &&
		source.Discovery != nil &&
		options.workspace != "" &&
		options.workspace != source.Discovery.Descriptor.Workspace {
		project := ""
		if source.Project != "" {
			project = fmt.Sprintf(" for project %q", source.Project)
		}
		return options, workspaceconnection.ActiveConnection{}, fmt.Errorf(
			"run config policy.workspace %q conflicts with catalog connection workspace %q%s",
			options.workspace,
			source.Discovery.Descriptor.Workspace,
			project,
		)
	}
	connection, err := ensureRunConnection(ctx, ensurer, source)
	if err != nil {
		if !required && errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
			return options, workspaceconnection.ActiveConnection{}, nil
		}
		return options, workspaceconnection.ActiveConnection{}, err
	}
	options.workspace = connection.Workspace
	options.kubeContext = connection.ContextName
	return options, connection, nil
}

// descriptorFor resolves the workspace connection descriptor that governs this
// run, for use before the connection is activated.
//
// A catalog entry carries its descriptor on the source; a plain repository does
// not, and the descriptor is only read further down inside Ensure. Both shapes
// have to be compared against a caller-named context, so the plain case is
// discovered here. A repository with no descriptor is the ordinary case, not an
// error, and yields nothing to compare against.
func descriptorFor(source runConnectionSource) *workspaceconnection.Discovery {
	if source.Discovery != nil {
		return source.Discovery
	}
	startDir := strings.TrimSpace(source.StartDir)
	if startDir == "" {
		return nil
	}
	discovery, err := workspaceconnection.Discover(startDir)
	if err != nil {
		return nil
	}
	return &discovery
}

// descriptorForStartDir resolves the descriptor governing the current working
// directory, for callers that have no runConnectionSource to hand.
//
// The lifecycle verbs decide before building one, and an unreadable or absent
// descriptor is the ordinary case rather than an error: it simply leaves
// nothing to compare a caller-named context against.
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
		projectName,
		defaultRunConnectionEnsurer(cmd),
	)
}

func resolveRunLifecycleConnectionWithEnsurer(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit bool,
	namespaceExplicit bool,
	projectName string,
	ensurer runConnectionEnsurer,
) (string, string, func(), error) {
	if contextExplicit && kubeContext != "" {
		// The lifecycle verbs return here without activating a connection, so
		// this is the only place they can notice that the caller's context
		// disagrees with the repository's descriptor. Skipping it made a
		// forgotten TAU_CONTEXT silently read state from a different cluster
		// than `tau run` would submit to, and refusing the submit while quietly
		// answering the status query is worse than either alone.
		if err := checkDescriptorContextConflict(kubeContext, cmd.Flags().Changed("context"), descriptorForStartDir()); err != nil {
			return "", "", nil, err
		}
		ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
		if err != nil {
			return "", "", nil, err
		}
		return kubeContext, ns, func() {}, nil
	}
	startDir, err := os.Getwd()
	if err != nil {
		return "", "", nil, err
	}
	repository, err := projectcatalog.Discover(startDir)
	if err != nil {
		return "", "", nil, err
	}
	source := runConnectionSource{
		StartDir: startDir,
		Git:      repository.Boundary.Git,
	}
	if repository.Catalog != nil {
		project, err := repository.Catalog.SelectLifecycleProject(projectName, startDir)
		if err != nil {
			return "", "", nil, err
		}
		source.Catalog = true
		source.Project = project.Name
		source.Discovery = &project.Connection
	} else if strings.TrimSpace(projectName) != "" {
		return "", "", nil, fmt.Errorf("--project requires %s at the Git worktree root", projectcatalog.Filename)
	}
	return resolveRunLifecycleConnectionFromSource(
		cmd,
		kubeContext,
		namespace,
		contextExplicit,
		namespaceExplicit,
		source,
		ensurer,
	)
}

func resolveRunLifecycleConnectionFromSource(
	cmd *cobra.Command,
	kubeContext, namespace string,
	contextExplicit bool,
	namespaceExplicit bool,
	source runConnectionSource,
	ensurer runConnectionEnsurer,
) (string, string, func(), error) {
	if contextExplicit && kubeContext != "" {
		// Same early return as the caller above, reached when the source was
		// already built; compare against its descriptor for the same reason.
		if err := checkDescriptorContextConflict(kubeContext, cmd.Flags().Changed("context"), descriptorFor(source)); err != nil {
			return "", "", nil, err
		}
		ns, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
		if err != nil {
			return "", "", nil, err
		}
		return kubeContext, ns, func() {}, nil
	}
	if !source.Git {
		if namespaceExplicit {
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
	if !namespaceExplicit {
		namespace = connection.Namespace
	}
	resolvedNamespace, err := resolveWorkloadNamespace(cmd, kubeContext, namespace)
	if err != nil {
		return "", "", nil, err
	}
	restore, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return "", "", nil, err
	}
	return connection.ContextName, resolvedNamespace, restore, nil
}

// resolveWorkspaceControlPlaneConnection gives the `tau workspace` read verbs
// the same descriptor-first cluster resolution the `tau run` lifecycle verbs
// already have. Without it they silently query whatever kubectl's ambient
// current-context happens to be, which reports a repository's workspace as
// missing when the descriptor points somewhere else entirely.
//
// Unlike the run and data verbs it deliberately keeps the caller's namespace:
// TauWorkspace and TauQuotaRequest live in the system namespace, not in the
// workspace's workload namespace, so only the cluster context is taken from the
// descriptor.
func resolveWorkspaceControlPlaneConnection(
	cmd *cobra.Command,
	kubeContext, namespace string,
) (string, func(), error) {
	return resolveWorkspaceControlPlaneConnectionWithEnsurer(
		cmd,
		kubeContext,
		namespace,
		defaultRunConnectionEnsurer(cmd),
	)
}

func resolveWorkspaceControlPlaneConnectionWithEnsurer(
	cmd *cobra.Command,
	kubeContext, namespace string,
	ensurer runConnectionEnsurer,
) (string, func(), error) {
	resolvedContext, _, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		cmd,
		kubeContext,
		namespace,
		runContextExplicit(cmd),
		true,
		"",
		ensurer,
	)
	if err != nil {
		return "", nil, err
	}
	return resolvedContext, restore, nil
}

// resolveWorkloadDataConnection gives the registry-reading `tau data` verbs the
// same workspace-first namespace resolution as the `tau run` lifecycle verbs.
// They read files off the workload PVC, so they must look in the namespace the
// workspace actually submits into rather than assume one.
func resolveWorkloadDataConnection(cmd *cobra.Command, kubeContext, namespace string) (string, string, func(), error) {
	return resolveRunLifecycleConnection(
		cmd,
		kubeContext,
		namespace,
		runContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
	)
}
