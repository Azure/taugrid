// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/cli/internal/queueresolve"
)

// serveTarget is where a serve workload lands: the namespace it is created in
// and the LocalQueue Kueue admits it out of.
//
// The two travel together because a LocalQueue is namespaced. Resolving one
// without the other produces a queue name that does not exist in the namespace
// it was paired with, which fails as silently as no queue at all.
type serveTarget struct {
	Namespace string
	Queue     string
}

// resolveServeTarget selects the namespace and its platform-managed default
// LocalQueue. Researchers may disambiguate the namespace, but queue selection
// remains Kueue configuration rather than a serve flag.
//
// The resolver reads the kueue.x-k8s.io/default-local-queue namespace label,
// verifies the LocalQueue exists, and checks that the current identity can
// create the serving resource there.
//
// A client dry-run is contractually offline and must not contact a cluster, so
// it substitutes visible placeholders instead of discovering. Callers get the
// warning text to print; this function does no output of its own.
func resolveServeTarget(ctx context.Context, r queueresolve.RawRunner, namespace, dryRun, workloadResource string) (serveTarget, string, error) {
	target := serveTarget{
		Namespace: strings.TrimSpace(namespace),
	}
	if dryRun == "client" {
		unresolved := []string{"queue"}
		if target.Namespace == "" {
			target.Namespace = clientDryRunNamespacePlaceholder
			unresolved = append(unresolved, "namespace")
		}
		target.Queue = clientDryRunQueuePlaceholder
		return target, clientDryRunPlaceholderWarning(unresolved...), nil
	}
	if r == nil {
		return serveTarget{}, "", fmt.Errorf("resolve default Kueue LocalQueue: Kubernetes runner is required")
	}
	selected, candidates, err := queueresolve.ResolveAccessibleQueue(ctx, r, queueresolve.ResolveAccessibleQueueOptions{
		Namespace:        target.Namespace,
		WorkloadResource: workloadResource,
	})
	if err != nil {
		if len(candidates) > 1 {
			return serveTarget{}, "", fmt.Errorf(
				"multiple authorized Kueue queue namespaces found; pass --namespace to select one%s",
				formatAccessibleQueueCandidates(candidates),
			)
		}
		return serveTarget{}, "", fmt.Errorf("resolve default Kueue LocalQueue: %w", err)
	}
	return serveTarget{Namespace: selected.Namespace, Queue: selected.QueueName}, "", nil
}

func serveWorkloadResource(kind string) string {
	if kind == "deployment" {
		return "deployments.apps"
	}
	return "rayservices.ray.io"
}
