// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

const systemNamespaceEnvironment = "TAU_SYSTEM_NAMESPACE"

func defaultSystemNamespace() string {
	if namespace := strings.TrimSpace(os.Getenv(systemNamespaceEnvironment)); namespace != "" {
		return namespace
	}
	return tauworkspace.SystemNamespace
}

func systemNamespaceHelp() string {
	return "namespace containing TauGrid system objects (default: repository workspace connection, $" + systemNamespaceEnvironment + ", or " + tauworkspace.SystemNamespace + ")"
}

func systemNamespaceFromCommand(cmd *cobra.Command) string {
	if flag := cmd.Flag("system-namespace"); flag != nil {
		if namespace := strings.TrimSpace(flag.Value.String()); flag.Changed && namespace != "" {
			return namespace
		}
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		if discovery, err := workspaceconnection.Discover(workingDirectory); err == nil {
			return discovery.Descriptor.ResolvedSystemNamespace()
		}
	}
	if flag := cmd.Flag("system-namespace"); flag != nil {
		if namespace := strings.TrimSpace(flag.Value.String()); namespace != "" {
			return namespace
		}
	}
	return defaultSystemNamespace()
}

func systemNamespaceForConnection(cmd *cobra.Command, connection workspaceconnection.ActiveConnection) string {
	if flag := cmd.Flag("system-namespace"); flag != nil && flag.Changed {
		return systemNamespaceFromCommand(cmd)
	}
	if namespace := strings.TrimSpace(connection.SystemNamespace); namespace != "" {
		return namespace
	}
	return systemNamespaceFromCommand(cmd)
}

func resolveSystemNamespaceAlias(cmd *cobra.Command, systemNamespace, legacyFlag, legacyNamespace string) (string, error) {
	systemNamespace = strings.TrimSpace(systemNamespace)
	legacyNamespace = strings.TrimSpace(legacyNamespace)
	systemChanged := cmd.Flags().Changed("system-namespace")
	legacyChanged := cmd.Flags().Changed(legacyFlag)
	if systemChanged && legacyChanged && systemNamespace != legacyNamespace {
		return "", fmt.Errorf("--system-namespace %q conflicts with deprecated --%s %q", systemNamespace, legacyFlag, legacyNamespace)
	}
	if legacyChanged {
		return legacyNamespace, nil
	}
	if systemChanged {
		return systemNamespace, nil
	}
	return systemNamespaceFromCommand(cmd), nil
}
