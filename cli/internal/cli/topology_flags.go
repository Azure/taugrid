package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/queueresolve"
	"github.com/Azure/taugrid/core/kueueapi"
	runqueue "github.com/Azure/taugrid/core/queue"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// topologyFlags carries placement intent resolved from a run config's policy
// block (see configToDispatch). These knobs are config-first: there is no cobra
// flag surface for them on `tau run`, and adding one would create a second
// source of truth alongside `policy.*`.
type topologyFlags struct {
	preset                   string
	policyPath               string
	team                     string
	lane                     string
	mode                     string
	topology                 string
	shape                    string
	gpuClass                 string
	queue                    string
	priorityTier             string
	workloadPriorityClass    string
	podPriorityClass         string
	disableDefaultPriorities bool
}

func formatPresetHandoff(p runtopology.ResolvedPreset) string {
	var b strings.Builder
	fmt.Fprintf(&b, "preset:  %s", p.Preset.Name)
	if p.Preset.Description != "" {
		fmt.Fprintf(&b, " — %s", p.Preset.Description)
	}
	b.WriteString("\n")
	if p.Preset.Explain != "" {
		fmt.Fprintf(&b, "why:     %s\n", p.Preset.Explain)
	}
	return b.String()
}

func (f topologyFlags) resolvePreset(explicitProfile string) (string, *runtopology.ResolvedPreset, []string, error) {
	if f.preset == "" {
		return explicitProfile, nil, nil, nil
	}
	resolved, err := runtopology.ResolvePreset(f.policyPath, f.preset)
	if err != nil {
		return "", nil, nil, err
	}
	profileName := explicitProfile
	warnings := []string{}
	if profileName == "" {
		profileName = resolved.Preset.Profile
	} else if resolved.Preset.Profile != "" && profileName != resolved.Preset.Profile {
		warnings = append(warnings, fmt.Sprintf("warning: --profile %q overrides preset %s profile %q", profileName, resolved.Preset.Name, resolved.Preset.Profile))
	}
	return profileName, &resolved, warnings, nil
}

func (f topologyFlags) applyWithChanged(o *jobrender.Options, preset *runtopology.ResolvedPreset, changed func(string) bool) ([]string, error) {
	return f.applyWithChangedAndWorkspaceQueue(o, preset, changed, false)
}

func (f topologyFlags) applyWithChangedAndWorkspaceQueue(o *jobrender.Options, preset *runtopology.ResolvedPreset, changed func(string) bool, workspaceQueueResolved bool) ([]string, error) {
	warnings := []string{}
	if preset != nil {
		presetOptions := preset.Options
		jobrender.ApplyTopologyOptions(o, presetOptions)
		if series, ok := runtopology.ManagedGPUSeriesForFlavor(preset.Preset.ResourceFlavor); ok && !workspaceQueueResolved {
			if o.NodeSelector == nil {
				o.NodeSelector = map[string]string{}
			}
			if current := o.NodeSelector[runtopology.ManagedGPUSeriesLabel]; current != "" && current != series {
				return nil, fmt.Errorf("preset %s requires %s=%s, but the workload selects %s", preset.Preset.Name, runtopology.ManagedGPUSeriesLabel, series, current)
			}
			o.NodeSelector[runtopology.ManagedGPUSeriesLabel] = series
		}
		if o.Labels == nil {
			o.Labels = map[string]string{}
		}
		for k, v := range preset.Labels {
			o.Labels[k] = v
		}
		if o.Annotations == nil {
			o.Annotations = map[string]string{}
		}
		for k, v := range preset.Annotations {
			o.Annotations[k] = v
		}
	}

	override := func(flag, raw, presetValue string, set func(string)) {
		if !changed(flag) {
			return
		}
		if flag != "queue" && preset != nil && presetValue != "" && raw != "" && raw != presetValue {
			warnings = append(warnings, fmt.Sprintf("warning: --%s=%q overrides preset %s value %q", flag, raw, preset.Preset.Name, presetValue))
		}
		set(raw)
	}
	override("team", f.team, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.Team }), func(v string) { o.Team = v })
	override("lane", f.lane, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.Lane }), func(v string) { o.Lane = v })
	override("mode", f.mode, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.Mode }), func(v string) { o.Mode = v })
	override("topology", f.topology, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.Placement }), func(v string) { o.Topology = v })
	override("shape", f.shape, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.Shape }), func(v string) { o.Shape = v })
	override("gpu-class", f.gpuClass, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.GPUClass }), func(v string) { o.GPUClass = v })
	override("queue", f.queue, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.QueueName }), func(v string) { o.QueueName = v })
	if preset != nil && !workspaceQueueResolved && changed("queue") &&
		!strings.EqualFold(strings.TrimSpace(o.QueueName), "auto") &&
		strings.TrimSpace(o.QueueName) != strings.TrimSpace(preset.Options.QueueName) {
		o.DisableKueueTopologyAnnotations = true
		delete(o.Annotations, workloadmeta.AnnotationKueueTopology)
	}
	override("priority-tier", f.priorityTier, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.PriorityTier }), func(v string) {
		o.PriorityTier = v
		o.WorkloadPriorityClassName = ""
		o.PodPriorityClassName = ""
	})
	override("workload-priority-class", f.workloadPriorityClass, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.WorkloadPriorityClassName }), func(v string) { o.WorkloadPriorityClassName = v })
	override("pod-priority-class", f.podPriorityClass, valueOrEmpty(preset, func(p runtopology.ResolvedPreset) string { return p.Options.PodPriorityClassName }), func(v string) { o.PodPriorityClassName = v })

	if preset == nil {
		jobrender.ApplyTopologyOptions(o, runtopology.Options{
			Team:                      f.team,
			Lane:                      f.lane,
			Mode:                      f.mode,
			Placement:                 f.topology,
			Shape:                     f.shape,
			GPUClass:                  f.gpuClass,
			QueueName:                 f.queue,
			PriorityTier:              f.priorityTier,
			WorkloadPriorityClassName: f.workloadPriorityClass,
			PodPriorityClassName:      f.podPriorityClass,
			DisableDefaultPriorities:  f.disableDefaultPriorities,
		})
	}
	if changed("disable-default-priorities") {
		o.DisableDefaultPriorities = f.disableDefaultPriorities
	}
	if canonical, deprecated := runtopology.NormalizeGPUClass(o.GPUClass); deprecated {
		warnings = append(warnings, fmt.Sprintf(
			"warning: gpu_class %q is deprecated; use %q instead (placement and interconnect belong in policy.topology)",
			o.GPUClass, canonical))
		o.GPUClass = canonical
	}
	if !workspaceQueueResolved && !strings.EqualFold(strings.TrimSpace(o.QueueName), "auto") {
		if warning, err := f.reconcilePresetQueueTeamOverride(o, preset, changed("queue"), changed("team")); err != nil {
			return nil, err
		} else if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings, nil
}

func configureGPUQueueModeWithChanged(gpuResourceMode string, o *jobrender.Options, changed func(string) bool) {
	switch gpuResourceMode {
	case "dra":
		o.GPUResourceName = runqueue.GPUResource
		o.DisableKueueTopologyAnnotations = true
		if !changed("queue") {
			queueName := strings.TrimSpace(o.QueueName)
			if queueName == "" || queueName == runtopology.SharedGPUQueueName {
				o.QueueName = runtopology.SharedDRAQueueName
			}
		}
	case "device-plugin":
		o.GPUResourceName = runqueue.GPUResourceDevicePlugin
	}
}

func mergeNodeSelectors(base, required map[string]string) (map[string]string, error) {
	if len(required) == 0 {
		return base, nil
	}
	if base == nil {
		base = map[string]string{}
	}
	for key, value := range required {
		if current := base[key]; current != "" && current != value {
			return nil, fmt.Errorf("node selector %s=%s conflicts with required value %s", key, current, value)
		}
		base[key] = value
	}
	return base, nil
}

func prepareAutoQueueRender(o *jobrender.Options, preset *runtopology.ResolvedPreset, allowImplicit bool, dryRun string) (bool, bool) {
	if o == nil {
		return false, false
	}
	explicit := strings.EqualFold(strings.TrimSpace(o.QueueName), "auto")
	implicit := dryRun != "client" && allowImplicit && strings.TrimSpace(o.QueueName) == "" && preset == nil
	if implicit {
		// Render a valid provisional manifest, then select from its effective
		// scheduling contract and render once more with the selected queue.
		o.QueueName = "auto"
	}
	return explicit, implicit
}

func (f topologyFlags) resolveAutoQueueFromManifest(
	ctx context.Context,
	r kubeRawRunner,
	namespace string,
	o *jobrender.Options,
	rendered []byte,
	dryRun string,
	explicitAuto bool,
	implicitAuto bool,
) ([]string, error) {
	if o == nil || (!explicitAuto && !implicitAuto) {
		return nil, nil
	}
	if dryRun == "client" {
		if explicitAuto {
			return nil, fmt.Errorf("--queue=auto requires live Kueue queue discovery; use --dry-run=server or omit --dry-run")
		}
		return nil, nil
	}
	if r == nil {
		return nil, fmt.Errorf("automatic queue selection requires cluster access")
	}

	contract, err := renderedQueueContractFromManifest(rendered)
	if err != nil {
		return nil, fmt.Errorf("read rendered scheduling contract for automatic queue selection: %w", err)
	}
	if contract.GPUCount <= 0 {
		if explicitAuto {
			return nil, fmt.Errorf("--queue=auto requires a positive GPU request")
		}
		return nil, nil
	}
	gpuResourceName := strings.TrimSpace(contract.GPUResourceName)
	if gpuResourceName == "" {
		gpuResourceName = "nvidia.com/gpu"
	}
	gpuClass := strings.TrimSpace(contract.GPUClass)
	if gpuClass == "" {
		gpuClass = runtopology.GPUClassAny
	}

	selected, candidates, err := queueresolve.SelectQueue(ctx, r, queueresolve.AutoSelectOptions{
		Namespace:       namespace,
		GPUCount:        contract.GPUCount,
		GPUClass:        gpuClass,
		NodeSelector:    contract.NodeSelector,
		PodTolerations:  contract.PodTolerations,
		TopologyRequest: contract.TopologyRequest,
		GPUResourceName: gpuResourceName,
	})
	if err != nil {
		prefix := "policy.queue: auto"
		if explicitAuto {
			prefix = "--queue=auto"
		}
		return nil, fmt.Errorf("%s: %w%s", prefix, err, formatQueueCandidates(candidates))
	}
	o.QueueName = selected.QueueName
	o.RequiredTopology = selected.RequiredTopology
	return []string{fmt.Sprintf("selected queue %s -> %s/%s for %d GPU(s)", selected.QueueName, selected.ClusterQueue, selected.ResourceFlavor, contract.GPUCount)}, nil
}

func resolveAccessibleQueueNamespace(ctx context.Context, r kubeRawRunner, namespaceExplicit bool, namespace *string, o *jobrender.Options, dryRun, workloadResource string, preserveImplicitAuto bool) ([]string, error) {
	if namespaceExplicit || dryRun == "client" || r == nil {
		return nil, nil
	}
	queueName := strings.TrimSpace(o.QueueName)
	explicitAuto := strings.EqualFold(queueName, "auto")
	preserveQueue := explicitAuto || preserveImplicitAuto && queueName == ""
	if explicitAuto {
		queueName = ""
	}
	selected, candidates, err := queueresolve.ResolveAccessibleQueue(ctx, r, queueresolve.ResolveAccessibleQueueOptions{
		QueueName:        queueName,
		Team:             o.Team,
		WorkloadResource: workloadResource,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve queue namespace: %w%s", err, formatAccessibleQueueCandidates(candidates))
	}
	*namespace = selected.Namespace
	if strings.TrimSpace(o.QueueName) == "" && !preserveQueue {
		o.QueueName = selected.QueueName
	}
	return []string{fmt.Sprintf("resolved queue %s/%s via Kubernetes RBAC", selected.Namespace, selected.QueueName)}, nil
}

func formatAccessibleQueueCandidates(candidates []queueresolve.AccessibleQueue) string {
	if len(candidates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nauthorized queue namespaces:")
	for _, c := range candidates {
		fmt.Fprintf(&b, "\n- %s/%s", c.Namespace, c.QueueName)
		if c.Team != "" {
			fmt.Fprintf(&b, " team=%s", c.Team)
		}
	}
	return b.String()
}

func formatQueueCandidates(candidates []queueresolve.QueueCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nvisible queues:")
	for _, c := range candidates {
		fmt.Fprintf(&b, "\n- %s -> %s/%s max_gpu=%d", c.QueueName, c.ClusterQueue, c.ResourceFlavor, c.GPUMax)
		if c.Reason != "" {
			fmt.Fprintf(&b, " (%s)", c.Reason)
		}
	}
	return b.String()
}

func valueOrEmpty(preset *runtopology.ResolvedPreset, get func(runtopology.ResolvedPreset) string) string {
	if preset == nil {
		return ""
	}
	return get(*preset)
}

func (f topologyFlags) reconcilePresetQueueTeamOverride(o *jobrender.Options, preset *runtopology.ResolvedPreset, queueChanged, teamChanged bool) (string, error) {
	if preset == nil {
		return "", nil
	}
	result, err := runtopology.ReconcilePresetQueueOverride(*preset, o.QueueName, o.Team, o.Lane, queueChanged, teamChanged)
	if err != nil {
		return "", err
	}
	o.Team = result.Team
	return result.Warning, nil
}

type kubeRawRunner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

type renderedQueueContract struct {
	QueueName       string
	GPUClass        string
	GPUCount        int
	GPUResourceName string
	NodeSelector    map[string]string
	PodTolerations  [][]kueueapi.Toleration
	TopologyRequest bool
}

type queueValidationPolicy struct {
	Preset                  *runtopology.ResolvedPreset
	TopologyName            string
	CatalogTopologyContract bool
}

func inspectRenderedQueue(ctx context.Context, r kubeRawRunner, namespace string, manifest []byte, opts jobrender.Options, policy queueValidationPolicy) (queueresolve.ValidationReport, error) {
	contract, err := renderedQueueContractFromManifest(manifest)
	if err != nil {
		return queueresolve.ValidationReport{}, err
	}
	queueName := contract.QueueName
	if queueName == "" {
		queueName = opts.QueueName
	}
	if queueName == "" {
		return queueresolve.ValidationReport{}, nil
	}
	nodeSelector := opts.NodeSelector
	if len(contract.NodeSelector) > 0 {
		nodeSelector = contract.NodeSelector
	}
	gpuResourceName := opts.GPUResourceName
	if contract.GPUResourceName != "" {
		gpuResourceName = contract.GPUResourceName
	}
	gpuClass := opts.GPUClass
	if contract.GPUClass != "" {
		gpuClass = contract.GPUClass
	}
	validationOpts := queueresolve.ValidationOptions{
		Namespace:               namespace,
		QueueName:               queueName,
		Preset:                  policy.Preset,
		Team:                    opts.Team,
		Lane:                    opts.Lane,
		GPUClass:                gpuClass,
		NodeSelector:            nodeSelector,
		PodTolerations:          contract.PodTolerations,
		GPUCount:                contract.GPUCount,
		GPUResourceName:         gpuResourceName,
		TopologyRequest:         contract.TopologyRequest,
		TopologyName:            policy.TopologyName,
		CatalogTopologyContract: policy.CatalogTopologyContract,
	}
	report, err := queueresolve.ValidateSelection(ctx, r, validationOpts)
	if err != nil && contract.GPUCount > 0 {
		_, candidates, selectErr := queueresolve.SelectQueue(ctx, r, queueresolve.AutoSelectOptions{
			Namespace:       namespace,
			GPUCount:        contract.GPUCount,
			GPUClass:        gpuClass,
			NodeSelector:    nodeSelector,
			PodTolerations:  contract.PodTolerations,
			GPUResourceName: gpuResourceName,
		})
		if selectErr == nil || len(candidates) > 0 {
			return report, fmt.Errorf("%w%s", err, formatQueueCandidates(candidates))
		}
	}
	return report, err
}

func validateRenderedQueue(ctx context.Context, r kubeRawRunner, namespace string, manifest []byte, opts jobrender.Options, policy queueValidationPolicy) error {
	report, err := inspectRenderedQueue(ctx, r, namespace, manifest, opts, policy)
	if err != nil {
		return err
	}
	if report.RequiredTopology != "" {
		return fmt.Errorf(
			"generated GPU workload is missing ResourceFlavor-required annotation %s=%q; Tau-managed submission paths must inject it before apply",
			runtopology.RequiredTopologyAnnotation, report.RequiredTopology)
	}
	return nil
}

func prepareGeneratedQueueTopology(
	ctx context.Context,
	r kubeRawRunner,
	namespace string,
	rendered []byte,
	opts *jobrender.Options,
	policy queueValidationPolicy,
	rerender func() ([]byte, error),
) ([]byte, error) {
	report, err := inspectRenderedQueue(ctx, r, namespace, rendered, *opts, policy)
	if err != nil {
		return nil, err
	}
	if report.RequiredTopology == "" {
		return rendered, nil
	}
	opts.RequiredTopology = report.RequiredTopology
	rendered, err = rerender()
	if err != nil {
		return nil, fmt.Errorf("render ResourceFlavor-required topology: %w", err)
	}
	if err := validateRenderedQueue(ctx, r, namespace, rendered, *opts, policy); err != nil {
		return nil, err
	}
	return rendered, nil
}

func renderedQueueContractFromManifest(manifest []byte) (renderedQueueContract, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var out renderedQueueContract
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return renderedQueueContract{}, fmt.Errorf("parse rendered manifest for queue validation: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		if renderedGPUObjectsRequestTopology(obj) {
			out.TopologyRequest = true
		}
		resourceNames := map[string]struct{}{}
		collectRenderedGPUResourceNames(obj, resourceNames)
		for resourceName := range resourceNames {
			if out.GPUResourceName != "" && out.GPUResourceName != resourceName {
				return renderedQueueContract{}, fmt.Errorf("rendered workload requests multiple GPU resource types %q and %q", out.GPUResourceName, resourceName)
			}
			out.GPUResourceName = resourceName
		}
		var selectors []map[string]string
		collectRenderedGPUNodeSelectors(obj, &selectors)
		for _, selector := range selectors {
			out.NodeSelector, err = mergeNodeSelectors(out.NodeSelector, selector)
			if err != nil {
				return renderedQueueContract{}, fmt.Errorf("rendered GPU pod selectors: %w", err)
			}
		}
		collectRenderedGPUTolerations(obj, &out.PodTolerations)

		meta, _ := obj["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if out.QueueName == "" {
			if queueName, _ := labels[runtopology.QueueLabel].(string); strings.TrimSpace(queueName) != "" {
				out.QueueName = strings.TrimSpace(queueName)
			}
		}
		if out.GPUClass == "" {
			if gpuClass, _ := labels[workloadmeta.LabelGPUClass].(string); strings.TrimSpace(gpuClass) != "" {
				out.GPUClass, _ = runtopology.NormalizeGPUClass(gpuClass)
			}
		}
		if kind, _ := obj["kind"].(string); kind == "RayJob" {
			out.GPUCount = maxInt(out.GPUCount, collectRenderedRayJobGPUCount(obj))
		} else if kind == "Job" {
			out.GPUCount = maxInt(out.GPUCount, collectRenderedJobGPUCount(obj))
		} else {
			out.GPUCount = maxInt(out.GPUCount, collectRenderedGPUCount(obj))
		}
	}
}

func renderedGPUObjectsRequestTopology(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if renderedPodTemplateRequestsTopology(typed) {
			return true
		}
		for _, child := range typed {
			if renderedGPUObjectsRequestTopology(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if renderedGPUObjectsRequestTopology(child) {
				return true
			}
		}
	}
	return false
}

func renderedPodTemplateRequestsTopology(value map[string]any) bool {
	spec, ok := value["spec"].(map[string]any)
	if !ok || collectRenderedDirectPodGPUCount(spec) == 0 {
		return false
	}
	metadata, _ := value["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	for _, key := range []string{
		"kueue.x-k8s.io/podset-required-topology",
		"kueue.x-k8s.io/podset-preferred-topology",
		"kueue.x-k8s.io/podset-unconstrained-topology",
	} {
		if renderedTopologyAnnotationSet(annotations[key]) {
			return true
		}
	}
	return false
}

func collectRenderedDirectPodGPUCount(spec map[string]any) int {
	count := collectRenderedGPUCount(spec["containers"])
	count = maxInt(count, collectRenderedGPUCount(spec["initContainers"]))
	return maxInt(count, collectRenderedGPUCount(spec["resourceClaims"]))
}

func renderedTopologyAnnotationSet(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case bool:
		return typed
	default:
		return value != nil
	}
}

func collectRenderedJobGPUCount(obj map[string]any) int {
	spec, _ := obj["spec"].(map[string]any)
	parallelism := intFromYAMLScalar(spec["parallelism"])
	if parallelism <= 0 {
		parallelism = 1
	}
	return parallelism * collectRenderedGPUCount(spec["template"])
}

func collectRenderedGPUCount(v any) int {
	switch x := v.(type) {
	case map[string]any:
		var maxGPU int
		for k, child := range x {
			if isRenderedGPUResource(k) {
				maxGPU = maxInt(maxGPU, intFromYAMLScalar(child))
				continue
			}
			if k == "resourceClaimTemplateName" {
				maxGPU = maxInt(maxGPU, gpuCountFromClaimName(fmt.Sprint(child)))
				continue
			}
			maxGPU = maxInt(maxGPU, collectRenderedGPUCount(child))
		}
		return maxGPU
	case []any:
		var maxGPU int
		for _, child := range x {
			maxGPU = maxInt(maxGPU, collectRenderedGPUCount(child))
		}
		return maxGPU
	default:
		return 0
	}
}

func collectRenderedGPUResourceNames(v any, names map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			if isRenderedGPUResource(key) {
				names[key] = struct{}{}
				continue
			}
			if key == "resourceClaimTemplateName" && gpuCountFromClaimName(fmt.Sprint(child)) > 0 {
				names[runqueue.GPUResource] = struct{}{}
				continue
			}
			collectRenderedGPUResourceNames(child, names)
		}
	case []any:
		for _, child := range x {
			collectRenderedGPUResourceNames(child, names)
		}
	}
}

func collectRenderedGPUNodeSelectors(v any, selectors *[]map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if rawSelector, ok := x["nodeSelector"].(map[string]any); ok && collectRenderedGPUCount(x) > 0 {
			selector := make(map[string]string, len(rawSelector))
			for key, value := range rawSelector {
				selector[key] = fmt.Sprint(value)
			}
			*selectors = append(*selectors, selector)
		}
		for _, child := range x {
			collectRenderedGPUNodeSelectors(child, selectors)
		}
	case []any:
		for _, child := range x {
			collectRenderedGPUNodeSelectors(child, selectors)
		}
	}
}

func collectRenderedGPUTolerations(value any, podTolerations *[][]kueueapi.Toleration) {
	switch typed := value.(type) {
	case map[string]any:
		if spec, ok := typed["spec"].(map[string]any); ok && collectRenderedDirectPodGPUCount(spec) > 0 {
			*podTolerations = append(*podTolerations, renderedTolerations(spec["tolerations"]))
		}
		for _, child := range typed {
			collectRenderedGPUTolerations(child, podTolerations)
		}
	case []any:
		for _, child := range typed {
			collectRenderedGPUTolerations(child, podTolerations)
		}
	}
}

func renderedTolerations(value any) []kueueapi.Toleration {
	items, _ := value.([]any)
	tolerations := make([]kueueapi.Toleration, 0, len(items))
	for _, item := range items {
		raw, _ := item.(map[string]any)
		tolerations = append(tolerations, kueueapi.Toleration{
			Key:      renderedString(raw, "key"),
			Operator: renderedString(raw, "operator"),
			Value:    renderedString(raw, "value"),
			Effect:   renderedString(raw, "effect"),
		})
	}
	return tolerations
}

func renderedString(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func isRenderedGPUResource(name string) bool {
	return name == runqueue.GPUResource ||
		name == runqueue.GPUResourceDevicePlugin ||
		strings.HasPrefix(name, "nvidia.com/mig-")
}

func gpuCountFromClaimName(name string) int {
	if strings.TrimSpace(name) == "full-gpu" {
		return 1
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimSpace(name), "ds-%dgpus", &count); err == nil && count > 0 {
		return count
	}
	return 0
}

func collectRenderedRayJobGPUCount(obj map[string]any) int {
	spec, _ := obj["spec"].(map[string]any)
	cluster, _ := spec["rayClusterSpec"].(map[string]any)
	total := 0
	if head, ok := cluster["headGroupSpec"].(map[string]any); ok {
		total += collectRenderedGPUCount(head)
	}
	if groups, ok := cluster["workerGroupSpecs"].([]any); ok {
		for _, raw := range groups {
			group, _ := raw.(map[string]any)
			replicas := intFromYAMLScalar(group["replicas"])
			if replicas <= 0 {
				replicas = 1
			}
			total += replicas * collectRenderedGPUCount(group)
		}
	}
	return total
}

func intFromYAMLScalar(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var out int
		if _, err := fmt.Sscanf(x, "%d", &out); err == nil {
			return out
		}
	}
	return 0
}

func maxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}
