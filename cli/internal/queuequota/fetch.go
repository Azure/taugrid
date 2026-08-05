package queuequota

import (
	"context"
	"fmt"
	"strings"
)

// RawRunner is the kubectl seam. It matches core/kube.Runner's Raw method so
// the CLI can pass the real runner and tests can pass a recorder.
type RawRunner interface {
	Raw(ctx context.Context, args []string, stdin []byte) (string, error)
}

// FetchOptions names the objects to read. ClusterQueue is required; LocalQueue
// and Namespace are optional and only used for the workload census.
type FetchOptions struct {
	Workspace    string
	Namespace    string
	LocalQueue   string
	ClusterQueue string
}

// Fetch reads the ClusterQueue, its ResourceFlavors, and the workspace's
// LocalQueue. Only the ClusterQueue read is fatal: without it there is no quota
// to report at all, whereas a missing flavor or LocalQueue degrades to a
// warning so a researcher with partial RBAC still gets the numbers.
func Fetch(ctx context.Context, r RawRunner, opts FetchOptions) (Report, error) {
	cqName := strings.TrimSpace(opts.ClusterQueue)
	if cqName == "" {
		return Report{}, fmt.Errorf("no ClusterQueue resolved for this workspace: the workspace status has no queue.clusterQueue, so there is no quota to report")
	}

	cqRaw, err := r.Raw(ctx, []string{"get", "clusterqueue.kueue.x-k8s.io", cqName, "-o", "json"}, nil)
	if err != nil {
		return Report{}, fmt.Errorf("read ClusterQueue %s: %w", cqName, err)
	}

	in := Input{
		Workspace:       opts.Workspace,
		Namespace:       opts.Namespace,
		LocalQueue:      strings.TrimSpace(opts.LocalQueue),
		ClusterQueue:    cqName,
		ClusterQueueRaw: []byte(cqRaw),
		FlavorsRaw:      map[string][]byte{},
	}

	if in.LocalQueue != "" && strings.TrimSpace(opts.Namespace) != "" {
		lqRaw, err := r.Raw(ctx, []string{
			"-n", opts.Namespace, "get", "localqueue.kueue.x-k8s.io", in.LocalQueue, "-o", "json",
		}, nil)
		if err == nil {
			in.LocalQueueRaw = []byte(lqRaw)
		}
	}

	names, err := flavorNamesFrom([]byte(cqRaw))
	if err != nil {
		return Report{}, fmt.Errorf("parse ClusterQueue %s: %w", cqName, err)
	}
	for _, name := range names {
		raw, err := r.Raw(ctx, []string{"get", "resourceflavor.kueue.x-k8s.io", name, "-o", "json"}, nil)
		if err != nil {
			continue
		}
		in.FlavorsRaw[name] = []byte(raw)
	}
	return Build(in)
}
