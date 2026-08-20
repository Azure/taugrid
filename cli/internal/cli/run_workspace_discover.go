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

// discoverPrimaryWorkspace resolves the workspace a run should target when the
// researcher did not name one.
//
// TauGrid v0 activates exactly one workspace per cluster and the controller
// actively blocks any second one, so requiring `--workspace` made researchers
// supply a value that had only one correct answer. Worse, the name is
// operator-chosen, so the "obvious" guess differed per cluster. Discovering it
// from cluster state keeps the workspace an operator concept and out of the
// researcher's way.
//
// Every lookup below is a *named* get before any list. That ordering is a hard
// requirement, not an optimization: the Role the controller creates for a
// workspace-rbac researcher grants `get` on that one workspace by ResourceName
// and nothing more, and the tau-core Kind suite asserts `can-i list workspaces`
// is denied. A list-first discovery therefore fails Forbidden for precisely the
// persona this exists to serve.
//
// Because the name is what the Role is keyed on, guessing it is not enough. A
// cluster whose operator renamed the workspace off the default can only be
// discovered if something tells the CLI the name, so the workspace connection
// descriptor is consulted first: its `workspace` field is required and is
// already the supported way an operator hands a researcher a custom-named
// cluster. The default name is tried next for the ordinary v0 cluster that has
// no descriptor checked in.
//
// The list is attempted last, for a cluster-wide identity that can enumerate
// workspaces. Failures are returned unwrapped so the caller can decide whether
// a missing workspace is fatal; not every command needs one.
func discoverPrimaryWorkspace(cmd *cobra.Command, kubeContext string) (tauworkspace.Workspace, error) {
	r := kube.New(kubeContext)
	systemNamespace := systemNamespaceFromCommand(cmd)

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
		// A named get proves the object exists, not that the controller
		// selected it. SelectPrimary's non-terminating fallback is a claim
		// about a population, so a one-item list satisfies it vacuously and
		// any live workspace with a candidate name would win here — including
		// one the controller marked AdditionalWorkspaceBlocked, whose RBAC and
		// queue bindings have been torn down. Only a self-evident primary may
		// short-circuit; anything else falls through to the list below, which
		// can compare peers, and fails closed if that list is forbidden.
		if !tauworkspace.ProvablyPrimary(workspace) {
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
		return tauworkspace.Workspace{}, describeWorkspaceLookupError(err, kubeContext)
	}
	list, err := tauworkspace.ParseList([]byte(raw))
	if err != nil {
		return tauworkspace.Workspace{}, err
	}
	workspace, err := tauworkspace.SelectPrimary(list)
	if err != nil {
		if errors.Is(err, tauworkspace.ErrNoWorkspaces) {
			// This is a platform-setup gap, not a researcher mistake, so name
			// the command that fixes it rather than reporting an empty list.
			return tauworkspace.Workspace{}, fmt.Errorf(
				"no TauWorkspace found in namespace %s; a platform owner creates the cluster workspace with `tau workspace create <name> --apply`",
				systemNamespace,
			)
		}
		return tauworkspace.Workspace{}, err
	}
	return workspace, nil
}

// namedWorkspaceCandidates lists the workspace names to try as named gets,
// most specific first.
//
// A descriptor names the workspace explicitly, so it outranks the default: on a
// cluster whose operator chose a custom name, the default is not merely absent
// but forbidden, and trying it first would spend a round trip on a guaranteed
// 403. A descriptor is optional, so a missing or malformed one is not an error
// here — it simply contributes no candidate.
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
