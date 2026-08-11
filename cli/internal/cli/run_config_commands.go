// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/pyimports"
	"github.com/Azure/taugrid/core/runconfig"
)

func newRunValidateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "validate [NAME]",
		Short: "Validate a direct Tau run config without submitting",
		Long: `Validate a direct Tau run config without submitting to Kubernetes.

This command covers direct tau run --config files. SDK-generated managed
workflow manifests are intentionally outside this schema/reference path.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedConfigPath, explicitConfig, err := discoverRunConfig(configPath)
			if err != nil {
				return err
			}
			if resolvedConfigPath == "" {
				if explicitConfig {
					return fmt.Errorf("--config is required")
				}
				return fmt.Errorf("no Tau run config found; create tau.yaml or pass --config")
			}
			cfg, targetOptions, configName, warnings, err := readRunConfig(resolvedConfigPath)
			if err != nil {
				return err
			}
			emitConfigWarnings(cmd.ErrOrStderr(), warnings)
			if cfg.LooksLikeManagedWorkflow() {
				return fmt.Errorf("managed workflow manifests are outside tau run validate; validate direct tau run --config files with this command")
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				name = configName
				targetOptions.nameFromConfig = name != ""
			}
			if err := validateRunDispatchOptions(targetOptions); err != nil {
				return err
			}
			if _, err := resolveRunTarget(targetOptions, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid\n", resolvedConfigPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to Tau experiment config (default: tau.yaml, tau.yml, or .tau.yaml)")
	return cmd
}

func newRunSchemaCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the Tau run config JSON Schema (machine-readable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(output) != "json" {
				return fmt.Errorf("--output must be json")
			}
			schema, err := runconfig.JSONSchema()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(schema))
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "json", "output format: json")
	return cmd
}

func newRunExplainConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain-config",
		Short: "Print the Tau run config field reference (human-readable Markdown)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), runconfig.ReferenceMarkdown())
			return nil
		},
	}
}

func validateRunDispatchOptions(o runDispatchOptions) error {
	// Only the direct engines execute run.entrypoint. Managed workflows embed
	// run.main_script at /script/train.py and always invoke python3 on that, so
	// their entrypoint is not what the launcher runs.
	if strings.TrimSpace(o.file) == "" && strings.EqualFold(strings.TrimSpace(o.launcher), "torchrun") {
		if err := jobrender.RequirePythonEntrypoint(o.script); err != nil {
			return err
		}
	}
	return checkEntrypointImports(o)
}

// checkEntrypointImports fails at submit time when the entrypoint imports a
// local module the run will not carry. Tau ships the entrypoint plus any
// explicitly listed extra scripts and nothing else, so an ordinary sibling
// import otherwise survives validation and admission and only fails inside a
// Ray worker once the cluster is already up.
func checkEntrypointImports(o runDispatchOptions) error {
	if o.source != nil {
		// The immutable source image supplies the complete project tree and the
		// main container runs from its staged root.
		return nil
	}
	if strings.TrimSpace(o.workingDir) != "" {
		// The whole project directory ships and Ray puts it on PYTHONPATH, so
		// local imports resolve on the driver and on every worker.
		return nil
	}
	// Managed workflows execute the main script, which a config may set
	// independently of run.entrypoint; runManagedWorkflow resolves it the same
	// way. The direct engines execute the entrypoint, so checking mainScript
	// there would validate a file the run never runs.
	script := o.script
	if strings.TrimSpace(o.file) != "" {
		script = firstNonEmpty(o.mainScript, o.script)
	}
	script = strings.TrimSpace(script)
	if script == "" || !strings.HasSuffix(script, ".py") {
		return nil
	}
	if _, err := os.Stat(script); err != nil {
		// Missing or unreadable entrypoints are reported by the dispatch path
		// with better context; do not duplicate that failure here.
		return nil
	}
	// Extra scripts are SRC[:DEST] specs but stage under DEST, so compare
	// against the destination name the module will actually have at runtime.
	shipped := []string{filepath.Base(script)}
	for _, spec := range o.extraScripts {
		src, dest := splitExtraScriptSpec(spec)
		if strings.TrimSpace(dest) == "" {
			dest = filepath.Base(src)
		}
		shipped = append(shipped, dest)
	}
	// Also search the project root: a common layout puts the entrypoint in a
	// subdirectory and shared modules at the top level, which resolve at
	// author time but are still not shipped.
	findings, err := pyimports.Check(script, shipped, o.configDir)
	if err != nil {
		return nil
	}
	return pyimports.Error(script, findings)
}
