// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpuinventory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Azure/taugrid/core/workloadmeta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const maxOwnerDepth = 16

type jobsReader interface {
	ListJobs(ctx context.Context, namespace string) ([]byte, error)
}

type rayJobsReader interface {
	ListRayJobs(ctx context.Context, namespace string) ([]byte, error)
}

type rayServicesReader interface {
	ListRayServices(ctx context.Context, namespace string) ([]byte, error)
}

type rayClustersReader interface {
	ListRayClusters(ctx context.Context, namespace string) ([]byte, error)
}

type replicaSetsReader interface {
	ListReplicaSets(ctx context.Context, namespace string) ([]byte, error)
}

type deploymentsReader interface {
	ListDeployments(ctx context.Context, namespace string) ([]byte, error)
}

type statefulSetsReader interface {
	ListStatefulSets(ctx context.Context, namespace string) ([]byte, error)
}

type daemonSetsReader interface {
	ListDaemonSets(ctx context.Context) ([]byte, error)
}

type scopedDaemonSetsReader interface {
	ListDaemonSetsInNamespace(ctx context.Context, namespace string) ([]byte, error)
}

type workloadsReader interface {
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
}

type objectList struct {
	Items []ownerObject `json:"items"`
}

type ownerObject struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetadata `json:"metadata"`
}

type objectMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []ownerReference  `json:"ownerReferences"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}

type ownerSource struct {
	name string
	kind string
	load func(context.Context) ([]byte, error)
}

type loadedOwnerSource struct {
	source      ownerSource
	objects     []ownerObject
	diagnostic  SourceDiagnostic
	unavailable bool
}

type ownerIndex struct {
	objects       map[string]ownerObject
	sourceStates  map[string]string
	workloadAttrs map[string][]attribution
}

type resolvedOwner struct {
	owner      Owner
	object     *ownerObject
	chainUIDs  []string
	diagnostic *Diagnostic
}

type workloadList struct {
	Items []struct {
		Metadata objectMetadata `json:"metadata"`
		Spec     struct {
			QueueName string `json:"queueName"`
		} `json:"spec"`
	} `json:"items"`
}

type attribution struct {
	name      string
	queue     string
	managed   bool
	workspace string
	team      string
	runID     string
	lane      string
	profile   string
	gpuClass  string
}

func loadOwnerIndex(ctx context.Context, reader Reader, namespace string) (ownerIndex, []SourceDiagnostic, bool) {
	sources := ownerSources(reader, namespace)
	results := make([]loadedOwnerSource, len(sources))
	var wg sync.WaitGroup
	for i := range sources {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			source := sources[index]
			raw, err := source.load(ctx)
			if err != nil {
				state := "unavailable"
				message := source.name + " could not be read."
				if apierrors.IsForbidden(err) {
					state = "forbidden"
					message = source.name + " access is forbidden."
				}
				results[index] = loadedOwnerSource{
					source:      source,
					unavailable: true,
					diagnostic: SourceDiagnostic{
						Source:  source.name,
						State:   state,
						Message: message,
					},
				}
				return
			}
			objects, err := decodeOwnerObjects(raw, source.kind, namespace)
			if err != nil {
				results[index] = loadedOwnerSource{
					source:      source,
					unavailable: true,
					diagnostic: SourceDiagnostic{
						Source:  source.name,
						State:   "invalid",
						Message: source.name + " returned an invalid response.",
					},
				}
				return
			}
			results[index] = loadedOwnerSource{
				source:     source,
				objects:    objects,
				diagnostic: SourceDiagnostic{Source: source.name, State: "ready"},
			}
		}(i)
	}
	wg.Wait()

	index := ownerIndex{
		objects:       map[string]ownerObject{},
		sourceStates:  map[string]string{},
		workloadAttrs: map[string][]attribution{},
	}
	diagnostics := make([]SourceDiagnostic, 0, len(results))
	partial := false
	for _, result := range results {
		diagnostics = append(diagnostics, result.diagnostic)
		state := result.diagnostic.State
		index.sourceStates[normalizeKind(result.source.kind)] = state
		if result.unavailable {
			partial = true
			continue
		}
		for _, object := range result.objects {
			if object.Metadata.UID == "" {
				continue
			}
			index.objects[objectKey(object.Metadata.Namespace, object.Metadata.UID)] = object
		}
	}
	return index, diagnostics, partial
}

func ownerSources(reader Reader, namespace string) []ownerSource {
	notSupported := func(source string) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			return nil, fmt.Errorf("%s reader is unavailable", source)
		}
	}
	sources := []ownerSource{
		{name: "jobs", kind: "Job", load: notSupported("jobs")},
		{name: "rayJobs", kind: "RayJob", load: notSupported("rayJobs")},
		{name: "rayServices", kind: "RayService", load: notSupported("rayServices")},
		{name: "rayClusters", kind: "RayCluster", load: notSupported("rayClusters")},
		{name: "replicaSets", kind: "ReplicaSet", load: notSupported("replicaSets")},
		{name: "deployments", kind: "Deployment", load: notSupported("deployments")},
		{name: "statefulSets", kind: "StatefulSet", load: notSupported("statefulSets")},
		{name: "daemonSets", kind: "DaemonSet", load: notSupported("daemonSets")},
		{name: "workloads", kind: "Workload", load: notSupported("workloads")},
	}
	if value, ok := reader.(jobsReader); ok {
		sources[0].load = func(ctx context.Context) ([]byte, error) { return value.ListJobs(ctx, namespace) }
	}
	if value, ok := reader.(rayJobsReader); ok {
		sources[1].load = func(ctx context.Context) ([]byte, error) { return value.ListRayJobs(ctx, namespace) }
	}
	if value, ok := reader.(rayServicesReader); ok {
		sources[2].load = func(ctx context.Context) ([]byte, error) { return value.ListRayServices(ctx, namespace) }
	}
	if value, ok := reader.(rayClustersReader); ok {
		sources[3].load = func(ctx context.Context) ([]byte, error) { return value.ListRayClusters(ctx, namespace) }
	}
	if value, ok := reader.(replicaSetsReader); ok {
		sources[4].load = func(ctx context.Context) ([]byte, error) { return value.ListReplicaSets(ctx, namespace) }
	}
	if value, ok := reader.(deploymentsReader); ok {
		sources[5].load = func(ctx context.Context) ([]byte, error) { return value.ListDeployments(ctx, namespace) }
	}
	if value, ok := reader.(statefulSetsReader); ok {
		sources[6].load = func(ctx context.Context) ([]byte, error) { return value.ListStatefulSets(ctx, namespace) }
	}
	if value, ok := reader.(scopedDaemonSetsReader); ok {
		sources[7].load = func(ctx context.Context) ([]byte, error) {
			return value.ListDaemonSetsInNamespace(ctx, namespace)
		}
	} else if value, ok := reader.(daemonSetsReader); ok {
		sources[7].load = value.ListDaemonSets
	}
	if value, ok := reader.(workloadsReader); ok {
		sources[8].load = func(ctx context.Context) ([]byte, error) { return value.ListWorkloads(ctx, namespace) }
	}
	return sources
}

func decodeOwnerObjects(raw []byte, kind, namespace string) ([]ownerObject, error) {
	if kind == "Workload" {
		return decodeWorkloadObjects(raw, namespace)
	}
	var list objectList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]ownerObject, 0, len(list.Items))
	for _, object := range list.Items {
		if namespace != "" && object.Metadata.Namespace != "" && object.Metadata.Namespace != namespace {
			continue
		}
		if object.Kind == "" {
			object.Kind = kind
		}
		out = append(out, object)
	}
	return out, nil
}

func decodeWorkloadObjects(raw []byte, namespace string) ([]ownerObject, error) {
	var list workloadList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]ownerObject, 0, len(list.Items))
	for _, item := range list.Items {
		if namespace != "" && item.Metadata.Namespace != "" && item.Metadata.Namespace != namespace {
			continue
		}
		labels := cloneStrings(item.Metadata.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		if strings.TrimSpace(item.Spec.QueueName) != "" {
			labels[queueLabel] = strings.TrimSpace(item.Spec.QueueName)
		}
		out = append(out, ownerObject{
			Kind: "Workload",
			Metadata: objectMetadata{
				Name:            item.Metadata.Name,
				Namespace:       item.Metadata.Namespace,
				UID:             item.Metadata.UID,
				Labels:          labels,
				Annotations:     item.Metadata.Annotations,
				OwnerReferences: item.Metadata.OwnerReferences,
			},
		})
	}
	return out, nil
}

func (i *ownerIndex) indexWorkloadAttribution() {
	for _, object := range i.objects {
		if normalizeKind(object.Kind) != "workload" {
			continue
		}
		ref, ok := controllerOwner(object.Metadata.OwnerReferences)
		if !ok || ref.UID == "" {
			continue
		}
		value := attributionFromMetadata(object.Metadata)
		value.name = object.Metadata.Name
		key := objectKey(object.Metadata.Namespace, ref.UID)
		i.workloadAttrs[key] = append(i.workloadAttrs[key], value)
	}
	for key := range i.workloadAttrs {
		sort.Slice(i.workloadAttrs[key], func(a, b int) bool {
			return i.workloadAttrs[key][a].name < i.workloadAttrs[key][b].name
		})
	}
}

func (i ownerIndex) resolve(pod podObject) resolvedOwner {
	podOwner := Owner{
		Kind:       "Pod",
		Name:       pod.Metadata.Name,
		UID:        pod.Metadata.UID,
		Resolution: "standalone",
	}
	ref, ok := controllerOwner(pod.Metadata.OwnerReferences)
	if !ok {
		return resolvedOwner{owner: podOwner}
	}

	namespace := pod.Metadata.Namespace
	visited := map[string]struct{}{}
	chainUIDs := []string{}
	var current *ownerObject
	for depth := 0; depth < maxOwnerDepth; depth++ {
		refKind := canonicalKind(ref.Kind)
		key := objectKey(namespace, ref.UID)
		if _, found := visited[key]; found && ref.UID != "" {
			owner := ownerFromKnownOrRaw(current, ref, "cycle")
			return resolvedOwner{
				owner:     owner,
				object:    current,
				chainUIDs: chainUIDs,
				diagnostic: &Diagnostic{
					Code:    "owner_cycle",
					Message: "The controller owner chain contains a cycle.",
				},
			}
		}
		if ref.UID != "" {
			visited[key] = struct{}{}
			chainUIDs = append(chainUIDs, ref.UID)
		}
		state, supported := i.sourceStates[normalizeKind(refKind)]
		if !supported {
			owner := ownerFromKnownOrRaw(current, ref, "unknown_kind")
			return resolvedOwner{
				owner:     owner,
				object:    current,
				chainUIDs: chainUIDs,
				diagnostic: &Diagnostic{
					Code:    "owner_unknown_kind",
					Message: "The controller owner kind is not supported by the inventory.",
				},
			}
		}
		if state != "ready" {
			owner := ownerFromKnownOrRaw(current, ref, "source_unavailable")
			return resolvedOwner{
				owner:     owner,
				object:    current,
				chainUIDs: chainUIDs,
				diagnostic: &Diagnostic{
					Code:    "owner_source_unavailable",
					Message: "A controller owner source could not be read.",
				},
			}
		}
		object, found := i.objects[key]
		if !found || normalizeKind(object.Kind) != normalizeKind(refKind) {
			owner := ownerFromKnownOrRaw(current, ref, "dangling")
			return resolvedOwner{
				owner:     owner,
				object:    current,
				chainUIDs: chainUIDs,
				diagnostic: &Diagnostic{
					Code:    "owner_dangling",
					Message: "The controller owner reference did not resolve by UID.",
				},
			}
		}
		currentObject := object
		current = &currentObject
		if normalizeKind(object.Kind) == "job" {
			next, found := controllerOwner(object.Metadata.OwnerReferences)
			if found && normalizeKind(next.Kind) == "rayjob" {
				ref = next
				continue
			}
			return resolvedOwner{
				owner: Owner{
					Kind:       canonicalKind(object.Kind),
					Name:       object.Metadata.Name,
					UID:        object.Metadata.UID,
					Resolution: "resolved",
				},
				object:    current,
				chainUIDs: chainUIDs,
			}
		}
		if terminalOwnerKind(object.Kind) {
			return resolvedOwner{
				owner: Owner{
					Kind:       canonicalKind(object.Kind),
					Name:       object.Metadata.Name,
					UID:        object.Metadata.UID,
					Resolution: "resolved",
				},
				object:    current,
				chainUIDs: chainUIDs,
			}
		}
		next, found := controllerOwner(object.Metadata.OwnerReferences)
		if !found {
			return resolvedOwner{
				owner: Owner{
					Kind:       canonicalKind(object.Kind),
					Name:       object.Metadata.Name,
					UID:        object.Metadata.UID,
					Resolution: "standalone",
				},
				object:    current,
				chainUIDs: chainUIDs,
			}
		}
		ref = next
	}
	owner := ownerFromKnownOrRaw(current, ref, "depth_limited")
	return resolvedOwner{
		owner:     owner,
		object:    current,
		chainUIDs: chainUIDs,
		diagnostic: &Diagnostic{
			Code:    "owner_depth_limited",
			Message: "The controller owner chain exceeded the supported depth.",
		},
	}
}

func ownerFromKnownOrRaw(current *ownerObject, ref ownerReference, resolution string) Owner {
	if current != nil {
		return Owner{
			Kind:         canonicalKind(current.Kind),
			Name:         current.Metadata.Name,
			UID:          current.Metadata.UID,
			Resolution:   resolution,
			RawOwnerKind: canonicalKind(ref.Kind),
			RawOwnerName: ref.Name,
			RawOwnerUID:  ref.UID,
		}
	}
	return Owner{
		Kind:       canonicalKind(ref.Kind),
		Name:       ref.Name,
		UID:        ref.UID,
		Resolution: resolution,
	}
}

func terminalOwnerKind(kind string) bool {
	switch normalizeKind(kind) {
	case "rayjob", "rayservice", "deployment", "statefulset", "daemonset":
		return true
	default:
		return false
	}
}

func controllerOwner(refs []ownerReference) (ownerReference, bool) {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			return ref, true
		}
	}
	return ownerReference{}, false
}

func objectKey(namespace, uid string) string {
	return namespace + "\x00" + uid
}

func normalizeKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

func canonicalKind(kind string) string {
	switch normalizeKind(kind) {
	case "pod":
		return "Pod"
	case "job":
		return "Job"
	case "rayjob":
		return "RayJob"
	case "rayservice":
		return "RayService"
	case "raycluster":
		return "RayCluster"
	case "replicaset":
		return "ReplicaSet"
	case "deployment":
		return "Deployment"
	case "statefulset":
		return "StatefulSet"
	case "daemonset":
		return "DaemonSet"
	case "workload":
		return "Workload"
	default:
		return strings.TrimSpace(kind)
	}
}

func attributionFromMetadata(metadata objectMetadata) attribution {
	labels := metadata.Labels
	annotations := metadata.Annotations
	return attribution{
		queue: strings.TrimSpace(labels[queueLabel]),
		managed: strings.EqualFold(strings.TrimSpace(labels[workloadmeta.LabelManagedBy]), workloadmeta.ManagedByValue) ||
			strings.TrimSpace(labels[workloadmeta.LabelService]) != "",
		workspace: firstNonEmpty(strings.TrimSpace(annotations[workloadmeta.AnnotationWorkspaceID]), strings.TrimSpace(labels[workloadmeta.LabelWorkspace])),
		team:      strings.TrimSpace(labels[workloadmeta.LabelTeam]),
		runID:     firstNonEmpty(strings.TrimSpace(labels[workloadmeta.LabelRunID]), strings.TrimSpace(labels[workloadmeta.LabelRun])),
		lane:      strings.TrimSpace(labels[workloadmeta.LabelLane]),
		profile:   strings.TrimSpace(labels[workloadmeta.LabelProfile]),
		gpuClass:  strings.TrimSpace(labels[workloadmeta.LabelGPUClass]),
	}
}

func mergeAttribution(base, fallback attribution) attribution {
	base.managed = base.managed || fallback.managed
	if base.queue == "" {
		base.queue = fallback.queue
	}
	if base.workspace == "" {
		base.workspace = fallback.workspace
	}
	if base.team == "" {
		base.team = fallback.team
	}
	if base.runID == "" {
		base.runID = fallback.runID
	}
	if base.lane == "" {
		base.lane = fallback.lane
	}
	if base.profile == "" {
		base.profile = fallback.profile
	}
	if base.gpuClass == "" {
		base.gpuClass = fallback.gpuClass
	}
	return base
}

func (i ownerIndex) attribution(namespace string, resolved resolvedOwner) attribution {
	value := attribution{}
	if resolved.object != nil {
		value = attributionFromMetadata(resolved.object.Metadata)
	}
	for index := len(resolved.chainUIDs) - 1; index >= 0; index-- {
		attrs := i.workloadAttrs[objectKey(namespace, resolved.chainUIDs[index])]
		for _, candidate := range attrs {
			value = mergeAttribution(value, candidate)
		}
	}
	return value
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
