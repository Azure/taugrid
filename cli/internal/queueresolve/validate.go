package queueresolve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/topology"
)

// RawRunner is the read-only kubectl surface used by queue validation.
type RawRunner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

// ValidationOptions describes the resolved queue/lane selection a run path is
// about to hand to Kubernetes.
type ValidationOptions struct {
	Namespace               string
	QueueName               string
	Preset                  *topology.ResolvedPreset
	Team                    string
	Lane                    string
	GPUClass                string
	ClusterQueue            string
	ResourceFlavor          string
	TopologyName            string
	CatalogTopologyContract bool
	TopologyRequest         bool
	NodeSelector            map[string]string
	PodTolerations          [][]kueueapi.Toleration
	GPUCount                int
	GPUResourceName         string
}

// ValidationReport is the read-only queue topology observed from the cluster.
type ValidationReport struct {
	Namespace        string   `json:"namespace"`
	QueueName        string   `json:"queueName,omitempty"`
	ClusterQueue     string   `json:"clusterQueue,omitempty"`
	ResourceFlavor   string   `json:"resourceFlavor,omitempty"`
	TopologyName     string   `json:"topologyName,omitempty"`
	RequiredTopology string   `json:"requiredTopology,omitempty"`
	GPUMax           int64    `json:"gpuMax,omitempty"`
	Preset           string   `json:"preset,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

type validationTarget struct {
	Namespace               string
	QueueName               string
	Preset                  *topology.ResolvedPreset
	Team                    string
	Lane                    string
	GPUClass                string
	ClusterQueue            string
	ResourceFlavor          string
	TopologyName            string
	CatalogTopologyContract bool
	TopologyRequest         bool
	NodeSelector            map[string]string
	PodTolerations          [][]kueueapi.Toleration
	GPUCount                int
	GPUResourceName         string
}

// ValidateSelection checks the selected LocalQueue and its backing Kueue
// objects. It only performs kubectl get calls; it never creates, patches, or
// repairs queue resources.
func ValidateSelection(ctx context.Context, r RawRunner, opts ValidationOptions) (ValidationReport, error) {
	target := opts.resolve()
	report := ValidationReport{
		Namespace:      target.Namespace,
		QueueName:      target.QueueName,
		ClusterQueue:   target.ClusterQueue,
		ResourceFlavor: target.ResourceFlavor,
	}
	if target.Preset != nil {
		report.Preset = target.Preset.Preset.Name
	}
	if target.QueueName == "" {
		return report, nil
	}
	lq, err := getLocalQueue(ctx, r, target.Namespace, target.QueueName)
	if err != nil {
		return report, missingLocalQueueError(target, err.Error())
	}
	if err := validateOptionalTopologyLabels("LocalQueue", target.QueueName, lq.Metadata.Labels, target); err != nil {
		return report, err
	}
	actualClusterQueue := strings.TrimSpace(lq.Spec.ClusterQueue)
	if actualClusterQueue == "" {
		return report, fmt.Errorf("LocalQueue %q in namespace %q has empty spec.clusterQueue; ask the platform owner to inspect that LocalQueue on the workspace cluster and repair it", target.QueueName, target.Namespace)
	}
	if target.ClusterQueue != "" && actualClusterQueue != target.ClusterQueue {
		return report, fmt.Errorf("LocalQueue %q in namespace %q points to ClusterQueue %q, but the selected preset expects %q; ask the platform owner to inspect that LocalQueue on the workspace cluster, choose a different preset, or pass a matching --queue/--team override", target.QueueName, target.Namespace, actualClusterQueue, target.ClusterQueue)
	}
	target.ClusterQueue = firstNonEmpty(target.ClusterQueue, actualClusterQueue)
	report.ClusterQueue = target.ClusterQueue

	cq, err := getClusterQueue(ctx, r, target.ClusterQueue)
	if err != nil {
		return report, fmt.Errorf("LocalQueue %q in namespace %q points to ClusterQueue %q, but it was not found (%s); ask the platform owner to inspect and repair that ClusterQueue on the workspace cluster", target.QueueName, target.Namespace, target.ClusterQueue, err)
	}
	if err := validateOptionalTopologyLabels("ClusterQueue", target.ClusterQueue, cq.Metadata.Labels, target); err != nil {
		return report, err
	}
	policyTopologyContract := target.CatalogTopologyContract
	if policyTopologyContract {
		capabilityFlavor, err := findCatalogTopologyFlavor(ctx, r, cq, target)
		if err != nil {
			return report, err
		}
		if capabilityFlavor == "" {
			return report, fmt.Errorf(
				"ClusterQueue %q does not provide topology %q through any ResourceFlavor with quota for resource %q that matches gpu_class %q and the rendered pod constraints; choose a topology-capable workspace queue or ask the platform owner to update its ResourceFlavors",
				target.ClusterQueue, target.TopologyName, target.GPUResourceName, target.GPUClass)
		}
		target.ResourceFlavor = capabilityFlavor
		report.ResourceFlavor = capabilityFlavor
		report.TopologyName = target.TopologyName
		if target.GPUCount > 0 && !target.TopologyRequest {
			rf, err := getResourceFlavor(ctx, r, capabilityFlavor)
			if err != nil {
				return report, fmt.Errorf("ResourceFlavor %q selected for topology %q could not be read (%s)", capabilityFlavor, target.TopologyName, err)
			}
			required, err := requiredTopologyForFlavor(rf, target.ClusterQueue)
			if err != nil {
				return report, err
			}
			report.RequiredTopology = required
			target.TopologyRequest = true
		}
	}
	if !policyTopologyContract &&
		target.ResourceFlavor == "" &&
		target.GPUCount > 0 &&
		target.GPUResourceName != "" {
		required, err := validateQueueTopologyIntent(ctx, r, cq, target)
		if err != nil {
			return report, err
		}
		if required != "" {
			report.RequiredTopology = required
			target.TopologyRequest = true
		}
		allowedFlavors, err := gpuClassAllowedFlavors(
			ctx, r, cq, target.GPUClass, target.GPUResourceName, target.NodeSelector, target.PodTolerations, target.TopologyRequest)
		if err != nil {
			return report, err
		}
		flavor, _, ok := cq.BestGPUFlavorFor(allowedFlavors, target.GPUResourceName)
		if !ok {
			if target.GPUClass == "" || target.GPUClass == topology.GPUClassAny {
				return report, fmt.Errorf(
					"ClusterQueue %q has no GPU quota flavor for resource %q compatible with the rendered pod selectors, tolerations, and topology policy; choose another queue or ask the platform owner to repair its ResourceFlavors",
					target.ClusterQueue, target.GPUResourceName)
			}
			return report, fmt.Errorf(
				"ClusterQueue %q has no compatible GPU quota flavor with exact node label %s=%q for resource %q and the rendered pod constraints; choose another queue or ask the platform owner to label a matching ResourceFlavor and GPU nodes",
				target.ClusterQueue, topology.NodeLabelGPUClass, target.GPUClass, target.GPUResourceName)
		}
		target.ResourceFlavor = flavor
		report.ResourceFlavor = flavor
	}
	if target.ResourceFlavor != "" {
		if !cq.HasResourceFlavor(target.ResourceFlavor) {
			return report, fmt.Errorf("ClusterQueue %q does not include ResourceFlavor %q required by preset %s; ask the platform owner to inspect that ClusterQueue on the workspace cluster and update the lane manifest", target.ClusterQueue, target.ResourceFlavor, presetName(target))
		}
		if !policyTopologyContract {
			rf, err := getResourceFlavor(ctx, r, target.ResourceFlavor)
			if err != nil {
				return report, fmt.Errorf("ResourceFlavor %q required by preset %s was not found (%s); ask the platform owner to inspect and repair that ResourceFlavor on the workspace cluster", target.ResourceFlavor, presetName(target), err)
			}
			if err := validateResourceFlavor(rf, target); err != nil {
				return report, err
			}
			required, err := validateResourceFlavorTopologyIntent(rf, target)
			if err != nil {
				return report, err
			}
			if required != "" {
				report.RequiredTopology = required
			}
			report.TopologyName = strings.TrimSpace(rf.Spec.TopologyName)
		}
	}
	maxGPU, maxOK := cq.MaxGPUCapacity(target.ResourceFlavor, target.GPUResourceName)
	if maxOK {
		report.GPUMax = maxGPU
	}
	if target.GPUCount > 0 && target.GPUResourceName != "" && !maxOK {
		return report, fmt.Errorf("LocalQueue %q in namespace %q points to ClusterQueue %q, but that queue has no quota for GPU resource %q; choose a queue that matches the workload GPU resource mode", target.QueueName, target.Namespace, target.ClusterQueue, target.GPUResourceName)
	}
	if target.GPUCount > 0 && maxOK && int64(target.GPUCount) > maxGPU {
		return report, fmt.Errorf("LocalQueue %q in namespace %q points to ClusterQueue %q, but that queue can admit at most %d NVIDIA GPU(s) for this flavor and the workload requests %d; choose a queue with enough GPU quota or use policy.queue: auto", target.QueueName, target.Namespace, target.ClusterQueue, maxGPU, target.GPUCount)
	}
	return report, nil
}

func findCatalogTopologyFlavor(ctx context.Context, r RawRunner, cq kueueapi.ClusterQueue, target validationTarget) (string, error) {
	var fitting []string
	fallback := ""
	var fallbackCapacity int64
	seen := map[string]bool{}
	var unreadable []string
	for _, group := range cq.Spec.ResourceGroups {
		for _, flavor := range group.Flavors {
			if seen[flavor.Name] {
				continue
			}
			seen[flavor.Name] = true
			capacity, ok := cq.MaxGPUCapacity(flavor.Name, target.GPUResourceName)
			if !ok || capacity <= 0 {
				continue
			}
			rf, err := getResourceFlavor(ctx, r, flavor.Name)
			if err != nil {
				unreadable = append(unreadable, fmt.Sprintf("%s (%s)", flavor.Name, err))
				continue
			}
			if strings.TrimSpace(rf.Spec.TopologyName) != target.TopologyName ||
				!resourceFlavorMatchesNodeSelector(rf, target.NodeSelector) ||
				!resourceFlavorMatchesPodTolerations(rf, target.PodTolerations) {
				continue
			}
			if target.GPUClass != "" && target.GPUClass != topology.GPUClassAny &&
				strings.TrimSpace(rf.Spec.NodeLabels[topology.NodeLabelGPUClass]) != target.GPUClass {
				continue
			}
			if int64(target.GPUCount) <= capacity {
				fitting = append(fitting, flavor.Name)
				continue
			}
			if fallback == "" || capacity > fallbackCapacity || capacity == fallbackCapacity && flavor.Name < fallback {
				fallback = flavor.Name
				fallbackCapacity = capacity
			}
		}
	}
	sort.Strings(fitting)
	if len(fitting) > 0 {
		return fitting[0], nil
	}
	if fallback != "" {
		return fallback, nil
	}
	if len(unreadable) > 0 {
		return "", fmt.Errorf(
			"cannot resolve topology %q in ClusterQueue %q because ResourceFlavor capabilities could not be read: %s",
			target.TopologyName, target.ClusterQueue, strings.Join(unreadable, ", "))
	}
	return "", nil
}

func (o ValidationOptions) resolve() validationTarget {
	ns := strings.TrimSpace(o.Namespace)
	gpuClass, _ := topology.NormalizeGPUClass(o.GPUClass)
	target := validationTarget{
		Namespace:               ns,
		QueueName:               strings.TrimSpace(o.QueueName),
		Preset:                  o.Preset,
		Team:                    strings.TrimSpace(o.Team),
		Lane:                    strings.TrimSpace(o.Lane),
		GPUClass:                gpuClass,
		ClusterQueue:            strings.TrimSpace(o.ClusterQueue),
		ResourceFlavor:          strings.TrimSpace(o.ResourceFlavor),
		TopologyName:            strings.TrimSpace(o.TopologyName),
		CatalogTopologyContract: o.CatalogTopologyContract,
		TopologyRequest:         o.TopologyRequest,
		NodeSelector:            o.NodeSelector,
		PodTolerations:          o.PodTolerations,
		GPUCount:                o.GPUCount,
		GPUResourceName:         strings.TrimSpace(o.GPUResourceName),
	}
	if o.Preset != nil {
		target.Namespace = topology.PresetLocalQueueNamespace(ns, *o.Preset)
		presetQueue := o.Preset.Options.QueueName
		if target.QueueName == "" {
			target.QueueName = presetQueue
		}
		if target.Team == "" {
			target.Team = o.Preset.Options.Team
		}
		if target.Lane == "" {
			target.Lane = o.Preset.Options.Lane
		}
		if target.GPUClass == "" {
			target.GPUClass, _ = topology.NormalizeGPUClass(o.Preset.Options.GPUClass)
		}
		if target.QueueName == presetQueue {
			if target.ClusterQueue == "" {
				target.ClusterQueue = o.Preset.Preset.ClusterQueue
			}
			if target.ResourceFlavor == "" {
				target.ResourceFlavor = o.Preset.Preset.ResourceFlavor
			}
			if target.TopologyName == "" {
				target.TopologyName = o.Preset.Preset.TopologyName
			}
		}
	}
	if target.Namespace == "" {
		target.Namespace = topology.DefaultLocalQueueNamespace
	}
	return target
}

func getLocalQueue(ctx context.Context, r RawRunner, namespace, queueName string) (kueueapi.LocalQueue, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", "localqueue.kueue.x-k8s.io", queueName, "-o", "json"}, nil)
	if err != nil {
		return kueueapi.LocalQueue{}, err
	}
	var item kueueapi.LocalQueue
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return kueueapi.LocalQueue{}, fmt.Errorf("parse LocalQueue %s/%s: %w", namespace, queueName, err)
	}
	if item.Metadata.Name == "" {
		return kueueapi.LocalQueue{}, fmt.Errorf("empty response")
	}
	return item, nil
}

func getClusterQueue(ctx context.Context, r RawRunner, name string) (kueueapi.ClusterQueue, error) {
	out, err := r.Raw(ctx, []string{"get", "clusterqueue.kueue.x-k8s.io", name, "-o", "json"}, nil)
	if err != nil {
		return kueueapi.ClusterQueue{}, err
	}
	var item kueueapi.ClusterQueue
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return kueueapi.ClusterQueue{}, fmt.Errorf("parse ClusterQueue %s: %w", name, err)
	}
	if item.Metadata.Name == "" {
		return kueueapi.ClusterQueue{}, fmt.Errorf("empty response")
	}
	return item, nil
}

func getResourceFlavor(ctx context.Context, r RawRunner, name string) (kueueapi.ResourceFlavor, error) {
	out, err := r.Raw(ctx, []string{"get", "resourceflavor.kueue.x-k8s.io", name, "-o", "json"}, nil)
	if err != nil {
		return kueueapi.ResourceFlavor{}, err
	}
	var item kueueapi.ResourceFlavor
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return kueueapi.ResourceFlavor{}, fmt.Errorf("parse ResourceFlavor %s: %w", name, err)
	}
	if item.Metadata.Name == "" {
		return kueueapi.ResourceFlavor{}, fmt.Errorf("empty response")
	}
	return item, nil
}

// gpuClassAllowedFlavors resolves which of a ClusterQueue's GPU-quota
// ResourceFlavors satisfy an exact researcher gpu_class request.
//
// gpu_class is a Kueue ResourceFlavor node-label contract
// (tau.azure.com/gpu-class), never a ResourceFlavor *name* heuristic: a
// flavor named "nd-h200-v5" (or one that happens to contain "h100" in its
// name, e.g. a mislabeled "legacy-h100-pool") only satisfies
// gpu_class: h200-141gb if its spec.nodeLabels carries that exact value.
// "any" skips only the class-label equality check. Every candidate is still
// read and checked against the rendered selectors, tolerations, and topology
// request. A non-nil, possibly-empty map is always returned so callers never
// bypass ResourceFlavor validation.
func gpuClassAllowedFlavors(
	ctx context.Context,
	r RawRunner,
	cq kueueapi.ClusterQueue,
	gpuClass, resourceName string,
	nodeSelector map[string]string,
	podTolerations [][]kueueapi.Toleration,
	topologyRequest bool,
) (map[string]bool, error) {
	class, _ := topology.NormalizeGPUClass(strings.TrimSpace(gpuClass))
	allowed := map[string]bool{}
	seen := map[string]bool{}
	var unreadable []string
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			if cap, ok := cq.MaxGPUCapacity(f.Name, resourceName); !ok || cap <= 0 {
				continue
			}
			rf, err := getResourceFlavor(ctx, r, f.Name)
			if err != nil {
				unreadable = append(unreadable, f.Name)
				continue
			}
			classMatches := class == "" || class == topology.GPUClassAny ||
				strings.TrimSpace(rf.Spec.NodeLabels[topology.NodeLabelGPUClass]) == class
			hasTopology := strings.TrimSpace(rf.Spec.TopologyName) != ""
			topologyMatches := hasTopology == topologyRequest
			if classMatches && topologyMatches &&
				resourceFlavorMatchesNodeSelector(rf, nodeSelector) &&
				resourceFlavorMatchesPodTolerations(rf, podTolerations) {
				allowed[f.Name] = true
			}
		}
	}
	if len(allowed) == 0 && len(unreadable) > 0 {
		return nil, fmt.Errorf("ResourceFlavor(s) %s could not be read while matching gpu_class=%q", strings.Join(unreadable, ", "), class)
	}
	return allowed, nil
}

type AutoSelectOptions struct {
	Namespace       string
	GPUCount        int
	GPUClass        string
	NodeSelector    map[string]string
	PodTolerations  [][]kueueapi.Toleration
	TopologyRequest bool
	GPUResourceName string
}

type QueueCandidate struct {
	QueueName        string
	ClusterQueue     string
	ResourceFlavor   string
	RequiredTopology string
	GPUMax           int64
	Score            int
	Reason           string
}

func SelectQueue(ctx context.Context, r RawRunner, opts AutoSelectOptions) (QueueCandidate, []QueueCandidate, error) {
	ns := strings.TrimSpace(opts.Namespace)
	if ns == "" {
		ns = topology.DefaultLocalQueueNamespace
	}
	gpuClass, _ := topology.NormalizeGPUClass(opts.GPUClass)
	lqs, err := listLocalQueues(ctx, r, ns)
	if err != nil {
		return QueueCandidate{}, nil, fmt.Errorf("list LocalQueues in namespace %q for policy.queue=auto: %w", ns, err)
	}
	var candidates []QueueCandidate
	for _, lq := range lqs.Items {
		name := strings.TrimSpace(lq.Metadata.Name)
		cqName := strings.TrimSpace(lq.Spec.ClusterQueue)
		if name == "" || cqName == "" {
			continue
		}
		cq, err := getClusterQueue(ctx, r, cqName)
		if err != nil {
			candidates = append(candidates, QueueCandidate{QueueName: name, ClusterQueue: cqName, Reason: "ClusterQueue not readable"})
			continue
		}
		topologyRequest := opts.TopologyRequest
		requiredTopology := ""
		if !topologyRequest {
			requiredTopology, err = validateQueueTopologyIntent(ctx, r, cq, validationTarget{
				ClusterQueue:    cqName,
				GPUClass:        gpuClass,
				NodeSelector:    opts.NodeSelector,
				PodTolerations:  opts.PodTolerations,
				GPUCount:        opts.GPUCount,
				GPUResourceName: opts.GPUResourceName,
			})
			if err != nil {
				candidates = append(candidates, QueueCandidate{
					QueueName:    name,
					ClusterQueue: cqName,
					Reason:       err.Error(),
				})
				continue
			}
			topologyRequest = requiredTopology != ""
		}
		allowedFlavors, err := gpuClassAllowedFlavors(
			ctx, r, cq, gpuClass, opts.GPUResourceName, opts.NodeSelector, opts.PodTolerations, topologyRequest)
		if err != nil {
			candidates = append(candidates, QueueCandidate{QueueName: name, ClusterQueue: cqName, Reason: err.Error()})
			continue
		}
		flavor, maxGPU, ok := cq.BestGPUFlavorFor(allowedFlavors, opts.GPUResourceName)
		c := QueueCandidate{
			QueueName:      name,
			ClusterQueue:   cqName,
			ResourceFlavor: flavor,
			GPUMax:         maxGPU,
		}
		if !ok {
			if gpuClass != "" && gpuClass != topology.GPUClassAny {
				c.Reason = fmt.Sprintf("no GPU quota flavor has %s=%q", topology.NodeLabelGPUClass, gpuClass)
			} else {
				c.Reason = "no matching GPU quota found"
			}
			candidates = append(candidates, c)
			continue
		}
		if opts.GPUCount > 0 && int64(opts.GPUCount) > maxGPU {
			c.Reason = fmt.Sprintf("max %d GPU(s) < requested %d", maxGPU, opts.GPUCount)
			candidates = append(candidates, c)
			continue
		}
		c.RequiredTopology = requiredTopology
		c.Score = 100
		c.Reason = "fits requested GPU count"
		candidates = append(candidates, c)
	}
	fits := make([]QueueCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Score > 0 {
			fits = append(fits, c)
		}
	}
	sort.SliceStable(fits, func(i, j int) bool {
		if fits[i].Score != fits[j].Score {
			return fits[i].Score > fits[j].Score
		}
		if fits[i].GPUMax != fits[j].GPUMax {
			return fits[i].GPUMax < fits[j].GPUMax
		}
		return fits[i].QueueName < fits[j].QueueName
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].GPUMax != candidates[j].GPUMax {
			return candidates[i].GPUMax > candidates[j].GPUMax
		}
		return candidates[i].QueueName < candidates[j].QueueName
	})
	if len(fits) == 0 {
		if gpuClass != "" && gpuClass != topology.GPUClassAny {
			return QueueCandidate{}, candidates, fmt.Errorf(
				"no visible LocalQueue in namespace %q provides gpu_class %q with enough quota for %d requested GPU(s)",
				ns, gpuClass, opts.GPUCount)
		}
		return QueueCandidate{}, candidates, fmt.Errorf("no visible LocalQueue in namespace %q can fit %d requested GPU(s)", ns, opts.GPUCount)
	}
	return fits[0], candidates, nil
}

func listLocalQueues(ctx context.Context, r RawRunner, namespace string) (kueueapi.LocalQueueList, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", "localqueues.kueue.x-k8s.io", "-o", "json"}, nil)
	if err != nil {
		return kueueapi.LocalQueueList{}, err
	}
	var items kueueapi.LocalQueueList
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return kueueapi.LocalQueueList{}, fmt.Errorf("parse LocalQueues in namespace %s: %w", namespace, err)
	}
	return items, nil
}

func validateOptionalTopologyLabels(kind, name string, labels map[string]string, target validationTarget) error {
	for _, check := range []struct {
		key  string
		want string
	}{
		{topology.LabelTeam, target.Team},
		{topology.LabelLane, target.Lane},
		{topology.LabelGPUClass, target.GPUClass},
	} {
		if check.want == "" {
			continue
		}
		got := strings.TrimSpace(labels[check.key])
		if got == "" {
			continue
		}
		if got != check.want {
			return fmt.Errorf("%s %q label %s=%q conflicts with selected value %q; queue ownership labels must match the resolved preset/team/lane", kind, name, check.key, got, check.want)
		}
	}
	return nil
}

func validateResourceFlavor(rf kueueapi.ResourceFlavor, target validationTarget) error {
	if target.GPUClass != "" && target.GPUClass != topology.GPUClassAny {
		got := strings.TrimSpace(rf.Spec.NodeLabels[topology.NodeLabelGPUClass])
		if got != target.GPUClass {
			return fmt.Errorf(
				"ResourceFlavor %q has %s=%q, but gpu_class %q requires an exact node-label match; ask the platform owner to label the ResourceFlavor and matching GPU nodes",
				rf.Metadata.Name, topology.NodeLabelGPUClass, got, target.GPUClass)
		}
	}
	if target.TopologyName != "" {
		got := strings.TrimSpace(rf.Spec.TopologyName)
		if got != target.TopologyName {
			return fmt.Errorf("ResourceFlavor %q topologyName=%q, but preset %s expects %q", target.ResourceFlavor, got, presetName(target), target.TopologyName)
		}
	}
	if target.GPUCount > 0 && !resourceFlavorMatchesNodeSelector(rf, target.NodeSelector) {
		return fmt.Errorf("ResourceFlavor %q node labels conflict with the rendered workload node selector", rf.Metadata.Name)
	}
	if target.GPUCount > 0 && !resourceFlavorMatchesPodTolerations(rf, target.PodTolerations) {
		return fmt.Errorf("ResourceFlavor %q node taints are not tolerated by the rendered GPU pods or the ResourceFlavor", rf.Metadata.Name)
	}
	return nil
}

func validateQueueTopologyIntent(ctx context.Context, r RawRunner, cq kueueapi.ClusterQueue, target validationTarget) (string, error) {
	if target.GPUCount <= 0 ||
		target.GPUResourceName == "" ||
		target.TopologyRequest ||
		target.ResourceFlavor != "" {
		return "", nil
	}

	flavors := compatibleGPUFlavorNames(cq, target)
	if len(flavors) == 0 {
		return "", nil
	}
	var tasOnly []string
	var missingMetadata []string
	requiredTopology := ""
	var unreadable []string
	for _, name := range flavors {
		rf, err := getResourceFlavor(ctx, r, name)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%s)", name, err))
			continue
		}
		if !resourceFlavorMatchesNodeSelector(rf, target.NodeSelector) {
			continue
		}
		if !resourceFlavorMatchesPodTolerations(rf, target.PodTolerations) {
			continue
		}
		if strings.TrimSpace(rf.Spec.TopologyName) == "" {
			return "", nil
		}
		required, err := requiredTopologyForFlavor(rf, target.ClusterQueue)
		if err != nil {
			missingMetadata = append(missingMetadata, name)
			tasOnly = append(tasOnly, name)
			continue
		}
		if requiredTopology != "" && requiredTopology != required {
			return "", fmt.Errorf(
				"compatible ResourceFlavors in ClusterQueue %q require conflicting %s values (%q and %q); ask the platform owner to split the queue or make its managed flavor metadata consistent",
				target.ClusterQueue, topology.RequiredTopologyAnnotation, requiredTopology, required)
		}
		requiredTopology = required
		tasOnly = append(tasOnly, name)
	}
	if len(unreadable) > 0 {
		return "", fmt.Errorf(
			"cannot determine whether GPU request without policy.topology is compatible with ClusterQueue %q because ResourceFlavor capabilities could not be read: %s; grant read access to ResourceFlavors or ask the platform owner to inspect the queue",
			target.ClusterQueue, strings.Join(unreadable, ", "))
	}
	if len(missingMetadata) > 0 {
		return "", fmt.Errorf(
			"GPU request has no policy.topology, and compatible ResourceFlavors in ClusterQueue %q support only TopologyAwareScheduling but are missing managed resource metadata annotation %s (%s); ask the platform owner to set the required Topology level (for example kubernetes.io/hostname)",
			target.ClusterQueue, topology.RequiredTopologyAnnotation, strings.Join(missingMetadata, ", "))
	}
	if len(tasOnly) == 0 {
		return "", nil
	}
	return requiredTopology, nil
}

func compatibleGPUFlavorNames(cq kueueapi.ClusterQueue, target validationTarget) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, group := range cq.Spec.ResourceGroups {
		for _, flavor := range group.Flavors {
			if _, ok := seen[flavor.Name]; ok {
				continue
			}
			capacity, ok := cq.MaxGPUCapacity(flavor.Name, target.GPUResourceName)
			if !ok || capacity < int64(target.GPUCount) {
				continue
			}
			seen[flavor.Name] = struct{}{}
			names = append(names, flavor.Name)
		}
	}
	sort.Strings(names)
	return names
}

func resourceFlavorMatchesNodeSelector(rf kueueapi.ResourceFlavor, selector map[string]string) bool {
	if selected := strings.TrimSpace(selector[topology.NodeLabelGPUClass]); selected != "" {
		if strings.TrimSpace(rf.Spec.NodeLabels[topology.NodeLabelGPUClass]) != selected {
			return false
		}
	}
	for key, flavorValue := range rf.Spec.NodeLabels {
		if selectedValue, ok := selector[key]; ok && selectedValue != flavorValue {
			return false
		}
	}
	return true
}

func resourceFlavorMatchesPodTolerations(rf kueueapi.ResourceFlavor, podTolerations [][]kueueapi.Toleration) bool {
	if len(rf.Spec.NodeTaints) == 0 {
		return true
	}
	if len(podTolerations) == 0 {
		podTolerations = [][]kueueapi.Toleration{nil}
	}
	for _, tolerations := range podTolerations {
		for _, taint := range rf.Spec.NodeTaints {
			if taint.Effect != "NoSchedule" && taint.Effect != "NoExecute" {
				continue
			}
			if !podToleratesTaint(tolerations, taint) && !podToleratesTaint(rf.Spec.Tolerations, taint) {
				return false
			}
		}
	}
	return true
}

func podToleratesTaint(tolerations []kueueapi.Toleration, taint kueueapi.Taint) bool {
	for _, toleration := range tolerations {
		if toleration.Effect != "" && toleration.Effect != taint.Effect {
			continue
		}
		switch toleration.Operator {
		case "Exists":
			if toleration.Key == "" || toleration.Key == taint.Key {
				return true
			}
		case "", "Equal":
			if toleration.Key == taint.Key && toleration.Value == taint.Value {
				return true
			}
		case "Lt", "Gt":
			if toleration.Key != taint.Key {
				continue
			}
			tolerationValue, tolerationErr := strconv.ParseInt(toleration.Value, 10, 64)
			taintValue, taintErr := strconv.ParseInt(taint.Value, 10, 64)
			if tolerationErr != nil || taintErr != nil {
				continue
			}
			if toleration.Operator == "Lt" && taintValue < tolerationValue ||
				toleration.Operator == "Gt" && taintValue > tolerationValue {
				return true
			}
		}
	}
	return false
}

func validateResourceFlavorTopologyIntent(rf kueueapi.ResourceFlavor, target validationTarget) (string, error) {
	if target.GPUCount <= 0 || target.GPUResourceName == "" {
		return "", nil
	}
	hasTopology := strings.TrimSpace(rf.Spec.TopologyName) != ""
	if target.TopologyRequest {
		if hasTopology {
			return "", nil
		}
		return "", fmt.Errorf(
			"GPU request sets policy.topology, but ResourceFlavor %q in ClusterQueue %q does not support TopologyAwareScheduling; choose a topology-capable queue or ask the platform owner to set spec.topologyName",
			rf.Metadata.Name, target.ClusterQueue)
	}
	if !hasTopology {
		return "", nil
	}
	required, err := requiredTopologyForFlavor(rf, target.ClusterQueue)
	if err != nil {
		return "", err
	}
	return required, nil
}

func requiredTopologyForFlavor(rf kueueapi.ResourceFlavor, clusterQueue string) (string, error) {
	required := strings.TrimSpace(rf.Metadata.Annotations[topology.RequiredTopologyAnnotation])
	if required != "" {
		return required, nil
	}
	return "", fmt.Errorf(
		"ResourceFlavor %q in ClusterQueue %q supports only TopologyAwareScheduling but is missing managed resource metadata annotation %s; ask the platform owner to set that annotation to the required Topology level (for example kubernetes.io/hostname), or set policy.topology explicitly",
		rf.Metadata.Name, clusterQueue, topology.RequiredTopologyAnnotation)
}

func missingLocalQueueError(target validationTarget, detail string) error {
	if target.Preset != nil {
		return topology.MissingPresetLocalQueueError(*target.Preset, target.Namespace, target.QueueName, detail)
	}
	return fmt.Errorf("LocalQueue %q in namespace %q was not found (%s); choose a different --queue or ask the platform owner to inspect and repair that LocalQueue on the workspace cluster", target.QueueName, target.Namespace, detail)
}

func presetName(target validationTarget) string {
	if target.Preset == nil || target.Preset.Preset.Name == "" {
		return "<none>"
	}
	return target.Preset.Preset.Name
}

// firstNonEmpty returns the first non-empty string, used to fill a validation
// target from the live cluster when the caller left a field blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
