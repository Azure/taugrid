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
		Short: "Run Tau Python SDK helper commands",
		Long: `Run Tau Python SDK helper commands through the canonical tau CLI.

These commands require the Tau Python SDK to be installed in the active Python
environment.`,
	}

	cmd.AddCommand(
		newPythonProxyCmd("inspect", "Print the YAML manifest a @tau.train / @tau.eval function would submit"),
		newPythonProxyCmd("build", "Export a deterministic generated artifact for decorated train/eval workflows"),
		newPythonProxyCmd("submit", "Discover @tau.train and @tau.eval handles in <module> and submit them as a chained pipeline"),
		newPythonProxyCmd("submit-build", "Verify and submit a generated decorator build artifact through Go Tau"),
		newPythonProxyCmd("bootstrap", "Download and install the matching tau Go CLI release binary"),
		newPythonProxyCmd("doctor", "Verify local Tau Python SDK, tau CLI, and basic kubectl prerequisites"),
	)

	return cmd
}

func newPythonProxyCmd(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              short,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
