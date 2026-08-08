// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/kube"
)

// The TAU_CONTEXT fallback is owned by taucore/kube so the tau CLI and the
// taugrid-portal binary cannot disagree about it.

// tauContextEnv is the environment variable that names the target cluster when
// --context is not passed.
const tauContextEnv = kube.ContextEnv

func defaultKubeContext() string {
	return kube.DefaultContext()
}

func kubeContextHelp() string {
	return kube.ContextHelp()
}

// runContextExplicit reports whether the caller named a target cluster.
//
// Not the same question as cobra's Changed("context"), which reports whether
// the flag was *typed*. Every --context flag is registered with
// defaultKubeContext() — $TAU_CONTEXT — as its default value, so a context
// supplied by the environment leaves Changed false while still naming a
// cluster as unambiguously as the flag does.
//
// The connection layer keys on this: when no context is named it takes over
// the connection and repoints KUBECONFIG at a cached workspace cluster. Asking
// the typed-ness question there meant `TAU_CONTEXT=a tau run` silently ran
// against whatever cluster was cached, or failed reporting that a workspace
// did not exist while looking at the wrong cluster entirely.
//
// Reading the resolved value keeps the documented equivalence between the flag
// and the environment variable, and matches what the guards downstream already
// pair this with (`contextExplicit && kubeContext != ""`).
func runContextExplicit(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("context")
	if flag == nil {
		return false
	}
	return strings.TrimSpace(flag.Value.String()) != ""
}

// checkDescriptorContextConflict reports an *ambient* target cluster that
// disagrees with the repository's checked-in workspace connection descriptor.
//
// Scoped to $TAU_CONTEXT on purpose. A descriptor is committed and shared, so
// letting a shell variable someone forgot they exported win would silently
// redirect a teammate's checkout. Letting the descriptor win just as silently
// would ignore the cluster the caller named. Both are a wrong cluster reached
// without being told, so the disagreement is reported.
//
// A typed --context is the way out, and therefore never conflicts: it is the
// one signal that is unambiguously about this invocation, and the error below
// would be useless advice if following it produced the same error.
func checkDescriptorContextConflict(kubeContext string, contextFromFlag bool, discovery *workspaceconnection.Discovery) error {
	kubeContext = strings.TrimSpace(kubeContext)
	if contextFromFlag || kubeContext == "" || discovery == nil {
		return nil
	}
	descriptorContext := strings.TrimSpace(discovery.Descriptor.Cluster.ContextName)
	if descriptorContext == "" || descriptorContext == kubeContext {
		return nil
	}
	location := strings.TrimSpace(discovery.Path)
	if location == "" {
		location = "the workspace connection descriptor"
	}
	return fmt.Errorf(
		"$%s names context %q but %s names %q; pass --context to choose one",
		tauContextEnv, kubeContext, location, descriptorContext,
	)
}

// contextFlag renders the " --context <name>" suffix used when echoing
// follow-up commands back to the user, or "" when the default context applies.
func contextFlag(kubeContext string) string {
	if kubeContext == "" {
		return ""
	}
	return " --context " + kubeContext
}
