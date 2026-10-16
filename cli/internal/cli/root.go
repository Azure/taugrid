// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/version"
)

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "tau",
		Short: "Repository-first AI workloads on Kubernetes",
		Long: `Tau is a repository-first, Kubernetes-native runtime for AI workloads.

Projects check in workload configs. Tau combines them with workspace policy,
renders upstream Kubernetes resources, submits through Kueue, and provides run
lifecycle and observability commands. The optional Python SDK generates the same
contract; the Go CLI remains the Kubernetes executor.`,
		Example: `  tau cluster install
  tau workspace create --principal-name research-team --apply
  tau run train
  tau run status train -n ray
  tau logs train -f
  tau serve deploy my-7b --profile model-serve --image vllm/vllm-openai:v0.6.3
  tau data dataset list`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}

	root.AddCommand(
		newClusterCmd(),
		newWorkspaceCmd(),
		newRunCmd(),
		newRootLogsCmd(),
		newServeCmd(),
		newDataCmd(),
		newPythonCmd(),
		newVersionCmd(),
		newPlatformCmd(),
	)

	return root
}
