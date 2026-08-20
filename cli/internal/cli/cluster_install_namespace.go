// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

type namespacedObjectList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	} `json:"items"`
}

// ensureSystemNamespaceMigrationSafe prevents an upgrade from moving the
// controller away from existing namespaced Tau system objects. Helm can
// recreate Deployments and RBAC in the release namespace, but it cannot move
// TauWorkspace or TauQuotaRequest objects between namespaces.
func ensureSystemNamespaceMigrationSafe(ctx context.Context, runner installationcheck.Runner, targetNamespace string) error {
	var outside []string
	for _, resource := range []struct {
		name string
		kind string
	}{
		{name: "workspaces.tau.azure.com", kind: "TauWorkspace"},
		{name: "quotarequests.tau.azure.com", kind: "TauQuotaRequest"},
	} {
		output, err := runner.Raw(ctx, []string{"get", resource.name, "--all-namespaces", "--output=json"}, nil)
		if err != nil {
			if isUnknownResourceError(err) || strings.Contains(strings.ToLower(err.Error()), "the server could not find the requested resource") {
				continue
			}
			return fmt.Errorf("inspect existing %s objects before changing the system namespace: %w", resource.kind, err)
		}
		var list namespacedObjectList
		if err := json.Unmarshal([]byte(output), &list); err != nil {
			return fmt.Errorf("decode existing %s objects: %w", resource.kind, err)
		}
		for _, item := range list.Items {
			if item.Metadata.Namespace != targetNamespace {
				outside = append(outside, fmt.Sprintf("%s %s/%s", resource.kind, item.Metadata.Namespace, item.Metadata.Name))
			}
		}
	}
	if len(outside) == 0 {
		return nil
	}
	sort.Strings(outside)
	return fmt.Errorf(
		"cannot move the TauGrid system namespace to %s while namespaced Tau system objects still exist elsewhere: %s; back up and migrate these objects before upgrading, or keep using the existing release version",
		targetNamespace,
		strings.Join(outside, ", "),
	)
}
