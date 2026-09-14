// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const maxServeAppArgsBytes = 1 << 20

func resolveServeAppArgs(cmd *cobra.Command, kind, importPath, filename string) (map[string]any, error) {
	flags := cmd.Flags()
	if !flags.Changed("app-args") {
		return nil, nil
	}
	if kind != "rayservice" {
		return nil, fmt.Errorf("--app-args requires --kind=rayservice")
	}
	if strings.TrimSpace(filename) == "" {
		return nil, fmt.Errorf("--app-args requires a file path")
	}
	if !flags.Changed("import-path") || strings.TrimSpace(importPath) == "" {
		return nil, fmt.Errorf("--app-args requires an explicit --import-path for the application builder")
	}
	for _, flag := range []string{
		"replicas", "min-replicas", "max-replicas", "target-qps", "scale-down-delay", "args",
	} {
		if flags.Changed(flag) {
			return nil, fmt.Errorf("--app-args conflicts with --%s; configure builder-owned deployments in the application arguments", flag)
		}
	}
	return loadServeAppArgs(filename)
}

func loadServeAppArgs(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read --app-args: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxServeAppArgsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read --app-args: %w", err)
	}
	if len(raw) > maxServeAppArgsBytes {
		return nil, fmt.Errorf("--app-args must not exceed 1 MiB")
	}
	var args map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("--app-args must be a JSON or YAML object: %w", err)
	}
	if args == nil {
		return nil, fmt.Errorf("--app-args must be a JSON or YAML object, not null")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("--app-args must contain exactly one document")
	}
	if _, err := json.Marshal(args); err != nil {
		return nil, fmt.Errorf("--app-args must contain JSON-compatible values: %w", err)
	}
	return args, nil
}
