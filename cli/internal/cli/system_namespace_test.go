// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDefaultSystemNamespaceUsesEnvironment(t *testing.T) {
	t.Setenv(systemNamespaceEnvironment, "custom-system")
	if got := defaultSystemNamespace(); got != "custom-system" {
		t.Fatalf("defaultSystemNamespace() = %q, want custom-system", got)
	}
}

func TestResolveSystemNamespaceAliasUsesDeprecatedFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("system-namespace", "tau-system", systemNamespaceHelp())
	cmd.Flags().String("platform-namespace", "", "deprecated alias")
	if err := cmd.Flags().Set("platform-namespace", "legacy-system"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSystemNamespaceAlias(cmd, "tau-system", "platform-namespace", "legacy-system")
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy-system" {
		t.Fatalf("resolved namespace = %q, want legacy-system", got)
	}
}

func TestResolveSystemNamespaceAliasRejectsConflict(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("system-namespace", "tau-system", systemNamespaceHelp())
	cmd.Flags().String("platform-namespace", "", "deprecated alias")
	if err := cmd.Flags().Set("system-namespace", "custom-system"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("platform-namespace", "legacy-system"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSystemNamespaceAlias(cmd, "custom-system", "platform-namespace", "legacy-system")
	if err == nil || !strings.Contains(err.Error(), "conflicts with deprecated --platform-namespace") {
		t.Fatalf("resolveSystemNamespaceAlias() error = %v, want conflict", err)
	}
}

func TestSystemNamespaceFromCommandPrefersExplicitFlag(t *testing.T) {
	t.Setenv(systemNamespaceEnvironment, "environment-system")
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	if err := cmd.Flags().Set("system-namespace", "flag-system"); err != nil {
		t.Fatal(err)
	}
	if got := systemNamespaceFromCommand(cmd); got != "flag-system" {
		t.Fatalf("systemNamespaceFromCommand() = %q, want flag-system", got)
	}
}
