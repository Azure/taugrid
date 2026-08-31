// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

func newPythonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "python",
		Short: "Author decorator-based workflows with the optional Python SDK",
		Long: `Author decorator-based workflows through the optional Tau Python SDK.

Most repository projects should check in tau.yaml and execute it with "tau run";
they do not need this command or the Python SDK. Use "tau python" when Python
modules define @tau.train or @tau.eval handles and need SDK-specific inspection,
source staging, deterministic build artifacts, or chained train/eval submission.

These commands require the Tau Python SDK to be installed in the active Python
environment. Each subcommand proxies its [args...] straight through to
"python3 -m tau.cli <subcommand>"; flag parsing is disabled here so Python-side
flags reach the SDK unmodified.`,
		Example: `  tau python doctor
  tau python inspect train.py
  tau python submit train.py --namespace ray --queue dev`,
		Args: cobra.NoArgs,
		RunE: showGroupHelp,
	}

	cmd.AddCommand(
		newPythonProxyCmd(
			"inspect",
			"Print the YAML manifest a @tau.train / @tau.eval function would submit",
			`Print the semantic train/eval manifest tau.cli would submit for every
@tau.train / @tau.eval handle found in MODULE, before source staging or
command-line overrides.`,
			`  tau python inspect train.py`,
		),
		newPythonProxyCmd(
			"build",
			"Export a deterministic generated artifact for decorated train/eval workflows",
			`Stage MODULE's source and render a byte-stable generated build artifact
directory that "tau python submit-build" can later verify and submit.`,
			`  tau python build train.py --output dist/tau-build`,
		),
		newPythonProxyCmd(
			"submit",
			"Discover @tau.train and @tau.eval handles in MODULE and submit them as a chained pipeline",
			`Discover @tau.train and @tau.eval handles in MODULE and submit them as a
chained pipeline, in one step (stage, build, and submit). Flags after MODULE
are passed through to the SDK and can override the decorator's namespace,
context, queue, GPU class, and resource requests without editing source.`,
			`  tau python submit train.py --namespace ray
  tau python submit experiments/vision_probe/config.py --namespace ray --context research-westus --queue dev --gpu-class any`,
		),
		newPythonProxyCmd(
			"submit-build",
			"Verify and submit a generated decorator build artifact through Go Tau",
			`Verify a generated build artifact directory produced by "tau python build"
and submit it through the Go tau CLI, without re-staging or re-rendering it.`,
			`  tau python submit-build dist/tau-build
  tau python submit-build dist/tau-build --dry-run=client`,
		),
		newPythonProxyCmd(
			"doctor",
			"Verify local Tau Python SDK, tau CLI, and basic kubectl prerequisites",
			`Verify that the Tau Python SDK, the tau Go CLI, and basic kubectl
prerequisites are present and consistent in the active environment.`,
			`  tau python doctor
  tau python doctor --namespace ray`,
		),
	)

	return cmd
}

func newPythonProxyCmd(name, short, long, example string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [args...]",
		Short: short,
		Long: long + "\n\nEvery [args...] token is passed through to \"python3 -m tau.cli " + name +
			"\"\nunchanged; no separator is needed and none is added. Use --help for this\n" +
			"wrapper overview, or run \"python3 -m tau.cli " + name + " --help\" for the\n" +
			"Python SDK's full option reference.",
		Example:            example,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			return runTauPython(name, args)
		},
	}
}

func runTauPython(subcommand string, args []string) error {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
		if err != nil {
			return fmt.Errorf("Tau Python SDK requires python3 or python on PATH")
		}
	}

	argv := append([]string{"-m", "tau.cli", subcommand}, args...)
	c := exec.Command(python, argv...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = os.Environ()

	if err := c.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr
		}
		return fmt.Errorf("run Tau Python SDK command: %w", err)
	}
	return nil
}
