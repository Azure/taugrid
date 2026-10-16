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
		Example: `  tau run validate
  tau run validate train --config tau.yaml
  tau run validate --config experiments/mc-rl/tau.yaml`,
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
		Long: `Print the Tau run config JSON Schema as machine-readable JSON, for editor
autocomplete, config generators, or CI validation of tau.yaml files.`,
		Example: `  tau run schema
  tau run schema > tau.schema.json`,
		Args: cobra.NoArgs,
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
		Long: `Print the full field reference for the Tau run config (tau.yaml) as
Markdown, including every supported key, its type, default, and description.`,
		Example: `  tau run explain-config
  tau run explain-config > tau-config-reference.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), runconfig.ReferenceMarkdown())
			return nil
		},
	}
}

func validateRunDispatchOptions(o unresolvedRunOptions) error {
	if strings.TrimSpace(o.file) == "" && strings.EqualFold(strings.TrimSpace(o.launcher), "torchrun") {
		if err := jobrender.RequirePythonEntrypoint(o.script); err != nil {
			return err
		}
	}
	return checkEntrypointImports(o)
}

func checkEntrypointImports(o unresolvedRunOptions) error {
	if o.source != nil {
		return nil
	}
	if strings.TrimSpace(o.workingDir.rayProjectPath()) != "" {
		return nil
	}
	script := o.script
	if strings.TrimSpace(o.file) != "" {
		script = firstNonEmpty(o.mainScript, o.script)
	}
	script = strings.TrimSpace(script)
	if script == "" || !strings.HasSuffix(script, ".py") {
		return nil
	}
	if _, err := os.Stat(script); err != nil {
		return nil
	}
	shipped := []string{filepath.Base(script)}
	for _, spec := range o.extraScripts {
		src, dest := splitExtraScriptSpec(spec)
		if strings.TrimSpace(dest) == "" {
			dest = filepath.Base(src)
		}
		shipped = append(shipped, dest)
	}
	findings, err := pyimports.Check(script, shipped, o.configDir)
	if err != nil {
		return nil
	}
	return pyimports.Error(script, findings)
}
