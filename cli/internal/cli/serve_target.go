// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/cli/internal/queueresolve"
)

type serveTarget struct {
	Namespace    string
	Queue        string
	ClusterQueue string
}

func resolveServeTarget(
	ctx context.Context,
	r queueresolve.RawRunner,
	namespace, workloadResource string,
) (serveTarget, string, error) {
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
	}, "", nil
}

func resolveServeWorkspaceTarget(
	ctx context.Context,
	r queueresolve.RawRunner,
	namespace, queue, expectedClusterQueue, workloadResource string,
) (serveTarget, error) {
	target := serveTarget{
		Namespace: strings.TrimSpace(namespace),
		Queue:     strings.TrimSpace(queue),
	}
	selected, err := queueresolve.ResolveExactQueue(ctx, r, queueresolve.ResolveAccessibleQueueOptions{
		Namespace:        target.Namespace,
		QueueName:        target.Queue,
		WorkloadResource: workloadResource,
	})
	if err != nil {
		return serveTarget{}, fmt.Errorf("resolve TauWorkspace LocalQueue: %w", err)
	}
	if expected := strings.TrimSpace(expectedClusterQueue); expected != "" && selected.ClusterQueue != expected {
		return serveTarget{}, fmt.Errorf(
			"TauWorkspace expects LocalQueue %q to use ClusterQueue %q, but it uses %q",
			selected.QueueName,
			expected,
			selected.ClusterQueue,
		)
	}
	return serveTarget{
		Namespace:    selected.Namespace,
		Queue:        selected.QueueName,
		ClusterQueue: selected.ClusterQueue,
	}, nil
}

func serveWorkloadResource(kind string) string {
	if kind == "deployment" {
		return "deployments.apps"
	}
	return "rayservices.ray.io"
}
