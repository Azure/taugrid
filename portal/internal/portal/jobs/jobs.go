// Package jobs builds the portal's Jobs/Queue board.
//
// It is the client-go analogue of queue.Fetch(): it reads the same three Kueue
// object lists, but through internal/portal/kubeclient instead of kubectl, then
// reuses the pure aggregator queue.BuildSnapshot() unchanged. Only the fetch
// layer differs; the Kueue-parsing and GPU-quota math are shared verbatim with
// `tau queue status`.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/queue"
	"github.com/Azure/taugrid/core/topology"
)

// Reader fetches the raw Kueue list JSON the board needs. kubeclient.Client
// satisfies this; tests supply a fake so no live API is required.
type Reader interface {
	ListLocalQueues(ctx context.Context, namespace string) ([]byte, error)
	ListClusterQueues(ctx context.Context) ([]byte, error)
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
}

// Scope is one explicitly configured or authorized LocalQueue view.
type Scope struct {
	Team      string
	Namespace string
	Queue     string
}

// Options controls the board fetch and post-fetch filtering.
type Options struct {
	Scopes     []Scope
	PolicyPath string
	Team       string
	Lane       string
	GPUClass   string
}

// Summary is the deduplicated Overview headline for a queue Snapshot.
type Summary struct {
	Pending     int
	Admitted    int
	GPUUsed     int64
	GPUHeadroom int64
}

// ValidateScopes verifies that scopes are complete and unambiguous.
func ValidateScopes(scopes []Scope) error {
	_, err := normalizeScopes(scopes)
	return err
}

// Board fetches each explicit scope and aggregates it with queue.BuildSnapshot.
// LocalQueue names never select a namespace or team; callers must provide both.
func Board(ctx context.Context, r Reader, opts Options) (queue.Snapshot, error) {
	scopes, err := normalizeScopes(opts.Scopes)
	if err != nil {
		return queue.Snapshot{}, err
	}
	pol, err := topology.LoadPolicy(opts.PolicyPath)
	if err != nil {
		return queue.Snapshot{}, err
	}
	clusterQueues, err := r.ListClusterQueues(ctx)
	if err != nil {
		return queue.Snapshot{}, fmt.Errorf("list clusterqueues: %w", err)
	}

	out := queue.Snapshot{
		PolicySource: pol.SourceFile,
		Groups:       []queue.Group{},
	}
	if len(scopes) == 1 {
		out.Namespace = scopes[0].Namespace
	}
	hints := map[string]struct{}{}
	for _, scope := range scopes {
		if opts.Team != "" && normalize(opts.Team) != scope.Team {
			continue
		}
		localQueues, err := r.ListLocalQueues(ctx, scope.Namespace)
		if err != nil {
			return queue.Snapshot{}, fmt.Errorf("list localqueues in %s: %w", scope.Namespace, err)
		}
		workloads, err := r.ListWorkloads(ctx, scope.Namespace)
		if err != nil {
			return queue.Snapshot{}, fmt.Errorf("list workloads in %s: %w", scope.Namespace, err)
		}
		snapshot, err := queue.BuildSnapshot(scope.Namespace, pol, localQueues, clusterQueues, workloads, queue.Options{
			Namespace:  scope.Namespace,
			PolicyPath: opts.PolicyPath,
			Team:       scope.Team,
			Lane:       opts.Lane,
			GPUClass:   opts.GPUClass,
		})
		if err != nil {
			return queue.Snapshot{}, err
		}
		groups := snapshot.Groups[:0]
		for _, group := range snapshot.Groups {
			if group.Queue == scope.Queue {
				groups = append(groups, group)
			}
		}
		if len(groups) == 0 {
			return queue.Snapshot{}, fmt.Errorf("jobs scope %s/%s for team %s has no matching topology policy group", scope.Namespace, scope.Queue, scope.Team)
		}
		if err := validateScopeBinding(localQueues, scope, groups); err != nil {
			return queue.Snapshot{}, err
		}
		out.Groups = append(out.Groups, groups...)
		for _, hint := range snapshot.Hints {
			hints[hint] = struct{}{}
		}
	}
	for hint := range hints {
		out.Hints = append(out.Hints, hint)
	}
	sort.Strings(out.Hints)
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := out.Groups[i], out.Groups[j]
		for _, pair := range [][2]string{
			{a.Namespace, b.Namespace},
			{a.Team, b.Team},
			{a.Lane, b.Lane},
			{a.GPUClass, b.GPUClass},
			{a.Queue, b.Queue},
			{a.ResourceFlavor, b.ResourceFlavor},
		} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return out, nil
}

func validateScopeBinding(raw []byte, scope Scope, groups []queue.Group) error {
	var list kueueapi.LocalQueueList
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("decode LocalQueues in %s: %w", scope.Namespace, err)
	}
	liveClusterQueue := ""
	for _, localQueue := range list.Items {
		if localQueue.Metadata.Name != scope.Queue {
			continue
		}
		if localQueue.Metadata.Namespace != "" && localQueue.Metadata.Namespace != scope.Namespace {
			continue
		}
		liveClusterQueue = strings.TrimSpace(localQueue.Spec.ClusterQueue)
		break
	}
	if liveClusterQueue == "" {
		return fmt.Errorf("jobs scope %s/%s does not resolve to a live LocalQueue", scope.Namespace, scope.Queue)
	}
	policyClusterQueue := ""
	for _, group := range groups {
		if policyClusterQueue == "" {
			policyClusterQueue = group.ClusterQueue
			continue
		}
		if group.ClusterQueue != policyClusterQueue {
			return fmt.Errorf("jobs scope %s/%s maps to multiple policy ClusterQueues", scope.Namespace, scope.Queue)
		}
	}
	if policyClusterQueue == "" || policyClusterQueue != liveClusterQueue {
		return fmt.Errorf(
			"jobs scope %s/%s policy ClusterQueue %q does not match live LocalQueue ClusterQueue %q",
			scope.Namespace, scope.Queue, policyClusterQueue, liveClusterQueue,
		)
	}
	return nil
}

// Summarize counts each LocalQueue and ClusterQueue flavor once even when
// several topology groups project the same underlying Kueue object.
func Summarize(snapshot queue.Snapshot) Summary {
	var out Summary
	queueSeen := map[string]struct{}{}
	quotaSeen := map[string]struct{}{}
	aggregateQuota := map[string]struct{}{}
	for _, group := range snapshot.Groups {
		if group.ResourceFlavor == "" {
			aggregateQuota[group.ClusterQueue] = struct{}{}
		}
	}
	for _, group := range snapshot.Groups {
		queueKey := group.Namespace + "\x00" + group.Queue
		if _, ok := queueSeen[queueKey]; !ok {
			queueSeen[queueKey] = struct{}{}
			out.Pending += group.Pending
			out.Admitted += group.Admitted
		}
		if _, ok := aggregateQuota[group.ClusterQueue]; ok && group.ResourceFlavor != "" {
			continue
		}
		quotaKey := group.ClusterQueue + "\x00" + group.ResourceFlavor
		if _, ok := quotaSeen[quotaKey]; !ok {
			quotaSeen[quotaKey] = struct{}{}
			out.GPUUsed += group.GPUUsed
			out.GPUHeadroom += group.GPUHeadroom
		}
	}
	return out
}

func normalizeScopes(in []Scope) ([]Scope, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one jobs scope is required")
	}
	out := make([]Scope, 0, len(in))
	teamsByQueue := map[string]string{}
	for i, scope := range in {
		scope.Team = normalize(scope.Team)
		scope.Namespace = strings.TrimSpace(scope.Namespace)
		scope.Queue = strings.TrimSpace(scope.Queue)
		if scope.Team == "" || scope.Namespace == "" || scope.Queue == "" {
			return nil, fmt.Errorf("jobs scope %d requires team, namespace, and local queue", i)
		}
		key := scope.Namespace + "\x00" + scope.Queue
		if team, ok := teamsByQueue[key]; ok {
			if team != scope.Team {
				return nil, fmt.Errorf("jobs scope %s/%s maps to conflicting teams %s and %s", scope.Namespace, scope.Queue, team, scope.Team)
			}
			return nil, fmt.Errorf("duplicate jobs scope %s/%s for team %s", scope.Namespace, scope.Queue, scope.Team)
		}
		teamsByQueue[key] = scope.Team
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Queue != out[j].Queue {
			return out[i].Queue < out[j].Queue
		}
		return out[i].Team < out[j].Team
	})
	return out, nil
}

func normalize(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "_", "-")
	return strings.ReplaceAll(value, " ", "-")
}
