// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Namespace resolution is workspace-first, and the workspace is the only
// authority. `tau run` takes the namespace from the TauWorkspace
// (applyWorkspaceDefaults) or from the active workspace connection descriptor;
// the lifecycle and data subcommands take it from the same connection via
// resolveRunLifecycleConnection. `--namespace` overrides all of it.
//
// There is deliberately no fallback namespace. A hardcoded default is a silent
// wrong answer: it sends a submit into one namespace while the workspace points
// somewhere else, and the query verbs then look in a third place. That is
// exactly the divergence that made a submitted run unobservable by
// `tau run status`. When nothing resolves, say so.
//
// The commands' own `--namespace` flags default to the empty string so `--help`
// advertises the workspace rather than a literal, and so
// `cmd.Flags().Changed("namespace")` keeps meaning "the user chose this".
func requireWorkloadNamespace(namespace string) (string, error) {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		return "", fmt.Errorf(
			"no namespace resolved for this workload: connect a workspace (see `tau workspace list`) or pass --namespace",
		)
	}
	return ns, nil
}

// workloadNamespaceHelp is the shared `--namespace` flag description, so every
// command advertises the same resolution rule.
const workloadNamespaceHelp = "namespace containing the run workload (default: from the connected workspace)"

// clientDryRunQueuePlaceholder and clientDryRunNamespacePlaceholder stand in for
// values that only the submit path can resolve.
//
// A client dry-run is contractually offline, so queue and namespace resolution —
// which both require SelfSubjectAccessReview against a live cluster — are
// skipped. The renderers still need non-empty values, and jobrender fails closed
// on an empty queue so a permanently-suspended Job cannot be submitted.
//
// These read as unresolved on sight. That is the point: the alternative is
// filling in something plausible like `default`, which renders a document that
// looks authoritative and is wrong. An angle-bracketed marker cannot be mistaken
// for a queue or namespace that exists.
const (
	clientDryRunQueuePlaceholder          = "<unresolved-queue>"
	clientDryRunNamespacePlaceholder      = "<unresolved-namespace>"
	clientDryRunWorkspacePlaceholder      = "<unresolved-workspace>"
	clientDryRunServiceAccountPlaceholder = "<unresolved-service-account>"
)

// clientDryRunPlaceholderWarning names only the fields that were actually
// substituted. Warning about a field the researcher supplied themselves reads
// as "your --namespace was ignored", which is both false and alarming, so
// callers skip the warning entirely when nothing was substituted.
func clientDryRunPlaceholderWarning(unresolved ...string) string {
	return fmt.Sprintf(
		"client dry-run does not contact the cluster: %s shown as a placeholder and resolved at submit; use --dry-run=server to see the real value",
		strings.Join(unresolved, " and "))
}

// defaultRenderNamespace is the pre-resolution seed the render options carry
// before requireWorkloadNamespace runs. On the submit path it is always
// overwritten by workspace or RBAC resolution, so it is never the namespace a
// workload lands in — which is exactly why a client dry-run must not print it.
const defaultRenderNamespace = "default"

// workloadNamespaceDiscoverer is the test seam for resolveWorkloadNamespace.
// Production leaves it nil so the real cluster lookup is used.
var workloadNamespaceDiscoverer smokeWorkspaceDiscoverer

// resolveWorkloadNamespace is requireWorkloadNamespace plus the same v0
// workspace discovery `tau run` and `tau run smoke` already perform.
//
// Without it the researcher experience is incoherent: a run submitted with no
// `--workspace` lands in the cluster's single workspace namespace, but every
// query verb that follows (`status`, `logs`, `get`, `cancel`) then demands the
// namespace the researcher was deliberately never told. Submitting and
// observing must resolve the target the same way.
//
// Discovery is only attempted when nothing else resolved a namespace, so an
// explicit `--namespace`, a workspace connection descriptor, and the catalog
// all keep priority and no extra cluster call is made on the common path.
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
		// Report the original "no namespace resolved" guidance rather than the
		// discovery failure: the caller may simply not be on a TauGrid cluster,
		// and `--namespace` remains the answer either way.
		return requireWorkloadNamespace(namespace)
	}
	ns := strings.TrimSpace(firstNonEmpty(
		discovered.Status.Target.ResolvedNamespace,
		discovered.Spec.Target.Namespace,
		discovered.Metadata.Name,
	))
	return requireWorkloadNamespace(ns)
}
