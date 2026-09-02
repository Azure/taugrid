// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type activeWorkspaceRequest struct {
	Source                  runConnectionSource
	Workspace               string
	WorkspaceExplicit       bool
	KubeContext             string
	KubeContextExplicit     bool
	KubeContextFromFlag     bool
	Namespace               string
	Queue                   string
	RequireRepositoryTarget bool
}

type activeWorkspaceResolution struct {
	Connection workspaceconnection.ActiveConnection
	Workspace  tauworkspace.Workspace
	Placement  workspacePlacement
	Context    string
	Connected  bool
	restore    func()
}

func (r activeWorkspaceResolution) Restore() {
	if r.restore != nil {
		r.restore()
	}
}

type activeWorkspaceResolver struct {
	connectionFactory runConnectionEnsurerFactory
	fetchWorkspace    runLifecycleWorkspaceFetcher
	now               func() time.Time
}

type workspaceConfigDirectoryProvider interface {
	ConfigDirectory() (string, error)
}

func newActiveWorkspaceResolver(
	connectionFactory runConnectionEnsurerFactory,
	fetchWorkspace runLifecycleWorkspaceFetcher,
) activeWorkspaceResolver {
	return activeWorkspaceResolver{
		connectionFactory: connectionFactory,
		fetchWorkspace:    fetchWorkspace,
		now:               time.Now,
	}
}

func (r activeWorkspaceResolver) Resolve(cmd *cobra.Command, request activeWorkspaceRequest) (activeWorkspaceResolution, error) {
	if r.connectionFactory == nil {
		return activeWorkspaceResolution{}, fmt.Errorf("workspace connection resolver is required")
	}
	ensurer := r.connectionFactory(cmd)
	discovery := request.Source.Discovery
	if discovery == nil {
		discovery = descriptorFor(request.Source)
	}
	if discovery != nil {
		request.Source.Discovery = discovery
	}
	connection := workspaceconnection.ActiveConnection{}
	workspaceName := strings.TrimSpace(request.Workspace)
	kubeContext := strings.TrimSpace(request.KubeContext)

	if request.RequireRepositoryTarget {
		var err error
		connection, err = ensureRunConnection(cmd.Context(), ensurer, request.Source)
		if err != nil {
			return activeWorkspaceResolution{}, err
		}
		if workspaceName != "" &&
			request.WorkspaceExplicit &&
			workspaceName != strings.TrimSpace(connection.Workspace) {
			return activeWorkspaceResolution{}, fmt.Errorf(
				"workspace %q conflicts with active repository workspace connection %q",
				workspaceName,
				connection.Workspace,
			)
		}
		if connectedContext := strings.TrimSpace(connection.ContextName); kubeContext != "" &&
			request.KubeContextExplicit && connectedContext != "" && kubeContext != connectedContext {
			return activeWorkspaceResolution{}, fmt.Errorf(
				"context %q conflicts with active repository workspace connection context %q",
				kubeContext,
				connectedContext,
			)
		}
		workspaceName = strings.TrimSpace(connection.Workspace)
		kubeContext = firstNonEmpty(connection.ContextName, kubeContext)
	} else {
		options := defaultRunDispatchOptions()
		options.workspace = workspaceName
		options.workspaceExplicit = request.WorkspaceExplicit
		options.kubeContext = kubeContext
		options.kubeContextExplicit = request.KubeContextExplicit
		options.kubeContextFromFlag = request.KubeContextFromFlag
		var err error
		options, connection, err = applyLiveRunConnection(
			cmd.Context(),
			options,
			request.Source,
			ensurer,
		)
		if err != nil {
			return activeWorkspaceResolution{}, err
		}
		workspaceName = strings.TrimSpace(options.workspace)
		kubeContext = strings.TrimSpace(options.kubeContext)
	}

	resolution := activeWorkspaceResolution{
		Connection: connection,
		Context:    kubeContext,
		restore:    func() {},
	}
	if strings.TrimSpace(connection.Workspace) == "" {
		return resolution, nil
	}

	restore, err := useKubeconfig(connection.KubeconfigPath)
	if err != nil {
		return activeWorkspaceResolution{}, err
	}
	resolution.restore = restore
	if r.fetchWorkspace == nil {
		resolution.Restore()
		return activeWorkspaceResolution{}, fmt.Errorf("TauWorkspace fetcher is required")
	}
	workspaceStatus, err := r.fetchWorkspace(
		cmd,
		kubeContext,
		systemNamespaceForConnection(cmd, connection),
		workspaceName,
	)
	if err != nil {
		resolution.Restore()
		return activeWorkspaceResolution{}, err
	}
	placement, err := resolveWorkspacePlacement(workspaceStatus, connection)
	if err != nil {
		resolution.Restore()
		return activeWorkspaceResolution{}, err
	}
	if namespace := strings.TrimSpace(request.Namespace); namespace != "" && namespace != placement.Namespace {
		resolution.Restore()
		return activeWorkspaceResolution{}, fmt.Errorf(
			"namespace %q conflicts with TauWorkspace %q target namespace %q",
			namespace,
			placement.Workspace,
			placement.Namespace,
		)
	}
	if queue := strings.TrimSpace(request.Queue); queue != "" && !strings.EqualFold(queue, "auto") && queue != placement.LocalQueue {
		resolution.Restore()
		return activeWorkspaceResolution{}, fmt.Errorf(
			"queue %q conflicts with TauWorkspace %q LocalQueue %q",
			queue,
			placement.Workspace,
			placement.LocalQueue,
		)
	}
	resolution.Workspace = workspaceStatus
	resolution.Placement = placement
	resolution.Connected = true
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if provider, ok := ensurer.(workspaceConfigDirectoryProvider); ok {
		configDir, err := provider.ConfigDirectory()
		if err != nil {
			resolution.Restore()
			return activeWorkspaceResolution{}, err
		}
		if err := persistActiveWorkspaceCache(configDir, discovery, connection, placement, now); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not update active workspace cache: %v\n", err)
		}
	}
	return resolution, nil
}
