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
	Namespace    string
	Queue        string
	ClusterQueue string
	Team         string
}

// resolveServeTarget selects the namespace and its platform-managed default
// LocalQueue. Researchers may disambiguate the namespace, but queue selection
// remains Kueue configuration rather than a serve flag.
//
// The resolver reads the kueue.x-k8s.io/default-local-queue namespace label,
// verifies the LocalQueue exists, and checks that the current identity can
// create the serving resource there.
//
// Client and server dry-runs use this same connected resolution path as apply.
// Serving cannot safely render a queue or authorization placeholder.
func resolveServeTarget(ctx context.Context, r queueresolve.RawRunner, namespace, workloadResource string) (serveTarget, string, error) {
	target := serveTarget{
		Namespace: strings.TrimSpace(namespace),
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
	return serveTarget{
		Namespace:    selected.Namespace,
		Queue:        selected.QueueName,
		ClusterQueue: selected.ClusterQueue,
		Team:         selected.Team,
	}, "", nil
}

func serveWorkloadResource(kind string) string {
	if kind == "deployment" {
		return "deployments.apps"
	}
	return "rayservices.ray.io"
}
