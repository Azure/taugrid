// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cli wires the tau cobra command tree.
//
// One file per public command group. Tau v0.5 keeps the visible surface small:
//
//	cluster    install, uninstall, and validate the TauGrid distribution
//	workspace  inspect workspaces, request quota, scaffold repos
//	run        submit a workload from a config, then drive its lifecycle
//	logs       discover a run through configured workspaces and stream its logs
//	serve      deploy, scale, inspect, and delete model endpoints
//	data       dataset registry and model checkpoint registry
//	python     proxy to the Tau Python SDK helper commands
//	version    print build metadata
//
// `platform` is registered but hidden: operator-only, read-only tooling that is
// never invoked by the researcher path.
//
// Experiment tracking (`experiment`/Stellar) and the observability `portal` are
// deliberately NOT in this binary — they ship as `taugrid-portal`.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/version"
)

// NewRoot constructs the top-level `tau` command with all subcommands attached.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "tau",
		Short: "Repository-first AI workloads on Kubernetes",
		Long: `Tau is a repository-first, Kubernetes-native runtime for AI workloads.

Projects check in workload configs. Tau combines them with workspace policy,
renders upstream Kubernetes resources, submits through Kueue, and provides run
lifecycle and observability commands. The optional Python SDK generates the same
contract; the Go CLI remains the Kubernetes executor.`,
		Example: `  tau cluster install --version 0.3.2
  tau workspace create --principal-name research-team --apply
  tau run train
  tau run status train -n ray
  tau logs train -f
  tau serve deploy my-7b --profile model-serve --image vllm/vllm-openai:v0.6.3
  tau data dataset list`,
		SilenceUsage: true,
		// main() prints `error: %v` and exits 1, so cobra must not print the
		// error too — without this every failure is reported twice.
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
