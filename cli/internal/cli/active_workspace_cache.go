// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/fileutil"
)

const (
	activeWorkspaceCacheSchema   = "tau.active-workspace.v1"
	activeWorkspaceCacheFilename = "active-workspace.json"
)

type activeWorkspaceCache struct {
	Schema           string    `json:"schema"`
	Workspace        string    `json:"workspace"`
	WorkspaceUID     string    `json:"workspaceUID"`
	ContextName      string    `json:"contextName"`
	SystemNamespace  string    `json:"systemNamespace"`
	Namespace        string    `json:"namespace"`
	LocalQueue       string    `json:"localQueue"`
	ClusterQueue     string    `json:"clusterQueue"`
	RepositoryRoot   string    `json:"repositoryRoot,omitempty"`
	DescriptorPath   string    `json:"descriptorPath,omitempty"`
	DescriptorDigest string    `json:"descriptorDigest"`
	ResolvedAt       time.Time `json:"resolvedAt"`
}

func persistActiveWorkspaceCache(
	configDir string,
	discovery *workspaceconnection.Discovery,
	connection workspaceconnection.ActiveConnection,
	placement workspacePlacement,
	now time.Time,
) error {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return fmt.Errorf("Tau config directory is required")
	}
	cache := activeWorkspaceCache{
		Schema:          activeWorkspaceCacheSchema,
		Workspace:       placement.Workspace,
		WorkspaceUID:    connection.WorkspaceUID,
		ContextName:     connection.ContextName,
		SystemNamespace: connection.SystemNamespace,
		Namespace:       placement.Namespace,
		LocalQueue:      placement.LocalQueue,
		ClusterQueue:    placement.ClusterQueue,
		ResolvedAt:      now.UTC(),
	}
	if discovery != nil {
		cache.RepositoryRoot = discovery.RepositoryRoot
		cache.DescriptorPath = discovery.Path
		cache.DescriptorDigest = discovery.Digest
	}
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active workspace cache: %w", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(filepath.Clean(configDir), activeWorkspaceCacheFilename)
	if err := fileutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return fmt.Errorf("write active workspace cache %s: %w", path, err)
	}
	return nil
}
