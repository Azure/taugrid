// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/queue"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/topology"
)

type Reader interface {
	ListLocalQueues(ctx context.Context, namespace string) ([]byte, error)
	ListClusterQueues(ctx context.Context) ([]byte, error)
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
}

type ProfileReader interface {
	ProfileSet(ctx context.Context) (profile.ProfileSet, error)
}

type Scope struct {
	Team      string
	Namespace string
	Queue     string
}

type Options struct {
	Scopes   []Scope
	Profiles ProfileReader
	Team     string
	Lane     string
	GPUClass string
}

type ProfileAvailability struct {
	Available        bool             `json:"available"`
	Error            string           `json:"error,omitempty"`
	Generation       int64            `json:"tauClusterGeneration,omitempty"`
	ProfileSetHash   string           `json:"profileSetHash,omitempty"`
	ReadyProfiles    []ProfileSummary `json:"readyProfiles"`
	ReadOnly         bool             `json:"readOnly"`
	SelectionEnabled bool             `json:"selectionEnabled"`
}

type ProfileSummary struct {
	Name              string                  `json:"name"`
	ExecutionTarget   profile.ExecutionTarget `json:"executionTarget"`
	Placement         string                  `json:"placement"`
	DefaultLocalQueue string                  `json:"defaultLocalQueue"`
	GPUsPerWorker     int32                   `json:"gpusPerWorker"`
	WorkerCount       int32                   `json:"workerCount"`
}

type Snapshot struct {
	queue.Snapshot
	WorkloadProfiles ProfileAvailability `json:"workloadProfiles"`
}

type Summary struct {
	Pending                 int
	Admitted                int
	GPUUsed                 int64
	GPUHeadroom             int64
	GPUReserved             int64
	GPUNominal              int64
	GPUReservationAvailable bool
	GPUNominalAvailable     bool
}

func ValidateScopes(scopes []Scope) error {
	_, err := normalizeScopes(scopes)
	return err
}

func Board(ctx context.Context, r Reader, opts Options) (Snapshot, error) {
	scopes, err := normalizeScopes(opts.Scopes)
	if err != nil {
		return Snapshot{}, err
	}
	profileScopes := scopes
	if opts.Team != "" {
		profileScopes = nil
		for _, scope := range scopes {
			if normalize(opts.Team) == scope.Team {
				profileScopes = append(profileScopes, scope)
			}
		}
	}
	profiles := ReadProfiles(ctx, opts.Profiles, profileScopes, opts.Lane)
	clusterQueues, err := r.ListClusterQueues(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list clusterqueues: %w", err)
	}

	out := Snapshot{
		Snapshot:         queue.Snapshot{Groups: []queue.Group{}},
		WorkloadProfiles: profiles,
	}
	if len(scopes) == 1 {
		out.Snapshot.Namespace = scopes[0].Namespace
	}
	hints := map[string]struct{}{}
	for _, scope := range scopes {
		if opts.Team != "" && normalize(opts.Team) != scope.Team {
			continue
		}
		localQueues, err := r.ListLocalQueues(ctx, scope.Namespace)
		if err != nil {
			return Snapshot{}, fmt.Errorf("list localqueues in %s: %w", scope.Namespace, err)
		}
		workloads, err := r.ListWorkloads(ctx, scope.Namespace)
		if err != nil {
			return Snapshot{}, fmt.Errorf("list workloads in %s: %w", scope.Namespace, err)
		}
		clusterQueue, err := liveClusterQueue(localQueues, scope)
		if err != nil {
			return Snapshot{}, err
		}
		observed := topology.Policy{Presets: map[string]topology.Preset{
			"portal-observed-scope": {
				Name: "portal-observed-scope", Team: scope.Team,
				QueueName: scope.Queue, ClusterQueue: clusterQueue,
			},
		}}
		snapshot, err := queue.BuildSnapshot(scope.Namespace, observed, localQueues, clusterQueues, workloads, queue.Options{
			Namespace: scope.Namespace,
			Team:      scope.Team,
		})
		if err != nil {
			return Snapshot{}, err
		}
		groups := snapshot.Groups[:0]
		for _, group := range snapshot.Groups {
			if group.Queue == scope.Queue {
				groups = append(groups, group)
			}
		}
		out.Snapshot.Groups = append(out.Snapshot.Groups, groups...)
		for _, hint := range snapshot.Hints {
			hints[hint] = struct{}{}
		}
	}
	for hint := range hints {
		out.Snapshot.Hints = append(out.Snapshot.Hints, hint)
	}
	sort.Strings(out.Snapshot.Hints)
	sort.Slice(out.Snapshot.Groups, func(i, j int) bool {
		a, b := out.Snapshot.Groups[i], out.Snapshot.Groups[j]
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

func liveClusterQueue(raw []byte, scope Scope) (string, error) {
	var list kueueapi.LocalQueueList
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("decode LocalQueues in %s: %w", scope.Namespace, err)
	}
	clusterQueue := ""
	for _, localQueue := range list.Items {
		if localQueue.Metadata.Name != scope.Queue {
			continue
		}
		if localQueue.Metadata.Namespace != "" && localQueue.Metadata.Namespace != scope.Namespace {
			continue
		}
		clusterQueue = strings.TrimSpace(localQueue.Spec.ClusterQueue)
		break
	}
	if clusterQueue == "" {
		return "", fmt.Errorf("jobs scope %s/%s does not resolve to a live LocalQueue", scope.Namespace, scope.Queue)
	}
	return clusterQueue, nil
}

func ReadProfiles(ctx context.Context, reader ProfileReader, scopes []Scope, lane string) ProfileAvailability {
	out := ProfileAvailability{ReadyProfiles: []ProfileSummary{}, ReadOnly: true}
	if reader == nil {
		out.Error = "workload profile status unavailable: portal started without a TauCluster profile reader"
		return out
	}
	set, err := reader.ProfileSet(ctx)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Available = true
	out.Generation = set.Generation
	out.ProfileSetHash = set.ProfileSetHash
	for _, candidate := range set.Profiles {
		if profile.ValidateReady(candidate, set.Generation) != nil ||
			!profileMatchesAnyScope(candidate, scopes, lane) {
			continue
		}
		out.ReadyProfiles = append(out.ReadyProfiles, ProfileSummary{
			Name: candidate.Name, ExecutionTarget: candidate.ExecutionTarget,
			Placement:         candidate.Placement,
			DefaultLocalQueue: candidate.DefaultLocalQueue,
			GPUsPerWorker:     candidate.GPUsPerWorker, WorkerCount: candidate.WorkerCount,
		})
	}
	sort.Slice(out.ReadyProfiles, func(i, j int) bool {
		return out.ReadyProfiles[i].Name < out.ReadyProfiles[j].Name
	})
	return out
}

func profileMatchesAnyScope(candidate profile.ResolvedWorkloadProfile, scopes []Scope, lane string) bool {
	for _, scope := range scopes {
		requestLane := lane
		if requestLane == "" && len(candidate.Applicability.Lanes) > 0 {
			requestLane = candidate.Applicability.Lanes[0]
		}
		if profile.ValidateApplicability(candidate, profile.SelectionRequest{
			Name: candidate.Name, Namespace: scope.Namespace, Team: scope.Team, Lane: requestLane,
		}) == nil {
			return true
		}
	}
	return false
}

func Summarize(snapshot queue.Snapshot) Summary {
	out := Summary{GPUReservationAvailable: len(snapshot.Groups) > 0, GPUNominalAvailable: len(snapshot.Groups) > 0}
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
			out.GPUReserved += group.GPUReserved
			out.GPUNominal += group.GPUNominal
			out.GPUReservationAvailable = out.GPUReservationAvailable && group.GPUReservationAvailable
			out.GPUNominalAvailable = out.GPUNominalAvailable && group.GPUNominalAvailable
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
