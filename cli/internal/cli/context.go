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

const tauContextEnv = kube.ContextEnv

func defaultKubeContext() string {
	return kube.DefaultContext()
}

func kubeContextHelp() string {
	return kube.ContextHelp()
}

func runContextExplicit(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("context")
	if flag == nil {
		return false
	}
	return strings.TrimSpace(flag.Value.String()) != ""
}

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

func contextFlag(kubeContext string) string {
	if kubeContext == "" {
		return ""
	}
	return " --context " + kubeContext
}
