// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

func requireWorkloadNamespace(namespace string) (string, error) {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		return "", fmt.Errorf(
			"no namespace resolved for this workload: connect a workspace (see `tau workspace list`) or pass --namespace",
		)
	}
	return ns, nil
}

const workloadNamespaceHelp = "namespace containing the run workload (default: from the connected workspace)"

const (
	clientDryRunQueuePlaceholder          = "<unresolved-queue>"
	clientDryRunNamespacePlaceholder      = "<unresolved-namespace>"
	clientDryRunWorkspacePlaceholder      = "<unresolved-workspace>"
	clientDryRunServiceAccountPlaceholder = "<unresolved-service-account>"
)

func clientDryRunPlaceholderWarning(unresolved ...string) string {
	return fmt.Sprintf(
		"client dry-run does not contact the cluster: %s shown as a placeholder and resolved at submit; use --dry-run=server to see the real value",
		strings.Join(unresolved, " and "))
}

const defaultRenderNamespace = "default"

type workspaceDiscoverer func(*cobra.Command, string) (tauworkspace.Workspace, error)

var workloadNamespaceDiscoverer workspaceDiscoverer

func resolveWorkloadNamespace(cmd *cobra.Command, kubeContext, namespace string) (string, error) {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns, nil
	}
	discoverer := workloadNamespaceDiscoverer
	if discoverer == nil {
		discoverer = discoverPrimaryWorkspace
	}
	discovered, err := discoverer(cmd, kubeContext)
	if err != nil {
		return requireWorkloadNamespace(namespace)
	}
	ns := strings.TrimSpace(firstNonEmpty(
		discovered.Status.Target.ResolvedNamespace,
		discovered.Spec.Target.Namespace,
		discovered.Metadata.Name,
	))
	return requireWorkloadNamespace(ns)
}
