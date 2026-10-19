// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func discoverPrimaryWorkspace(cmd *cobra.Command, kubeContext string) (tauworkspace.Workspace, error) {
	r := kube.New(kubeContext)
	systemNamespace := systemNamespaceFromCommand(cmd)
	var namedFallbacks []tauworkspace.Workspace

	for _, name := range namedWorkspaceCandidates() {
		raw, err := r.Raw(cmd.Context(), []string{
			"-n", systemNamespace,
			"get", "workspaces.tau.azure.com", name, "-o", "json",
		}, nil)
		if err != nil {
			continue
		}
		workspace, parseErr := tauworkspace.Parse([]byte(raw))
		if parseErr != nil {
			return tauworkspace.Workspace{}, parseErr
		}
		if !tauworkspace.ProvablyPrimary(workspace) {
			if tauworkspace.NamedGetFallback(workspace) {
				namedFallbacks = append(namedFallbacks, workspace)
			}
			continue
		}
		selected, selErr := tauworkspace.SelectPrimary(
			tauworkspace.WorkspaceList{Items: []tauworkspace.Workspace{workspace}},
		)
		if selErr == nil {
			return selected, nil
		}
		if errors.Is(selErr, tauworkspace.ErrPrimaryTerminating) {
			return tauworkspace.Workspace{}, selErr
		}
	}

	raw, err := r.Raw(cmd.Context(), []string{
		"-n", systemNamespace,
		"get", "workspaces.tau.azure.com", "-o", "json",
	}, nil)
	if err != nil {
		if len(namedFallbacks) == 1 {
			return namedFallbacks[0], nil
		}
		if len(namedFallbacks) > 1 {
			return tauworkspace.Workspace{}, fmt.Errorf(
				"multiple Ready TauWorkspaces were found by name but listing is unavailable; pass --workspace explicitly",
			)
		}
		return tauworkspace.Workspace{}, describeWorkspaceLookupError(err, kubeContext)
	}
	list, err := tauworkspace.ParseList([]byte(raw))
	if err != nil {
		return tauworkspace.Workspace{}, err
	}
	workspace, err := tauworkspace.SelectPrimary(list)
	if err != nil {
		if errors.Is(err, tauworkspace.ErrNoWorkspaces) {
			return tauworkspace.Workspace{}, fmt.Errorf(
				"no TauWorkspace found in namespace %s; a platform owner creates the cluster workspace with `tau workspace create <name> --apply`",
				systemNamespace,
			)
		}
		return tauworkspace.Workspace{}, err
	}
	return workspace, nil
}

func namedWorkspaceCandidates() []string {
	candidates := make([]string, 0, 2)
	if wd, err := os.Getwd(); err == nil {
		if discovery, err := workspaceconnection.Discover(wd); err == nil {
			if name := strings.TrimSpace(discovery.Descriptor.Workspace); name != "" {
				candidates = append(candidates, name)
			}
		}
	}
	if len(candidates) == 0 || candidates[0] != workloadmeta.DefaultWorkspaceName {
		candidates = append(candidates, workloadmeta.DefaultWorkspaceName)
	}
	return candidates
}
