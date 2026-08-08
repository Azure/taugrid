// Package topology turns Tau's researcher-facing workload contract into
// Kueue-facing queue, priority, and topology metadata.
package topology

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	QueueLabel                  = "kueue.x-k8s.io/queue-name"
	workloadPriorityLabel       = "kueue.x-k8s.io/priority-class"
	RequiredTopologyAnnotation  = "kueue.x-k8s.io/podset-required-topology"
	requiredTopologyAnnotation  = RequiredTopologyAnnotation
	preferredTopologyAnnotation = "kueue.x-k8s.io/podset-preferred-topology"
	unconstrainedTopologyAnnot  = "kueue.x-k8s.io/podset-unconstrained-topology"
	hostnameTopology            = "kubernetes.io/hostname"
	defaultElasticPodPriority   = "taugrid-default"
	DefaultElasticWorkloadPrio  = "taugrid-default"
	DefaultTrainPodPriority     = "taugrid-default"
	defaultTrainWorkloadPrio    = "taugrid-default"
	priorityTrainPodPriority    = "taugrid-priority"
	priorityTrainWorkloadPrio   = "taugrid-priority"
	defaultEvalPodPriority      = "taugrid-default"
	defaultEvalWorkloadPrio     = "taugrid-default"
	priorityEvalPodPriority     = "taugrid-priority"
	priorityEvalWorkloadPrio    = "taugrid-priority"
	SharedGPUQueueName          = "jobqueue"
	SharedGPUClusterQueueName   = "tau-cq"
	SharedDRAQueueName          = "jobqueue-dra"
	sharedDRAClusterQueueName   = "tau-dra-cq"
	ManagedGPUSeriesLabel       = "kueue.azure.com/gpu-series"
	AKSNodePoolModeLabel        = "kubernetes.azure.com/mode"
	AKSSystemNodePoolMode       = "system"
	// GPUClassAny is explicit unconstrained hardware selection: it renders no
	// class selector and lets Kueue admit against any available GPU flavor.
	GPUClassAny = "any"
	// GPUClassA10080GB, GPUClassH10095GB, and GPUClassH200141GB are the
	// canonical, hardware-only gpu_class values. They name capacity/memory
	// only ("80gb", "95gb", "141gb"); interconnect/placement concerns
	// (NVLink, same-host, multi-node) belong solely to spec.topology.placement
	// and must never be re-encoded into a gpu_class name. Each canonical value
	// is also the exact tau.azure.com/gpu-class node-label/ResourceFlavor
	// contract Tau uses to select a Kueue ResourceFlavor -- resolution must
	// match this value exactly and never by matching a substring of a
	// ResourceFlavor's own name.
	GPUClassA10080GB        = "a100-80gb"
	GPUClassH10095GB        = "h100-95gb"
	GPUClassH200141GB       = "h200-141gb"
	LabelTeam               = workloadmeta.LabelTeam
	LabelLane               = workloadmeta.LabelLane
	LabelGPUClass           = workloadmeta.LabelGPUClass
	LabelShape              = workloadmeta.LabelShape
	AnnotationTopologyQueue = workloadmeta.AnnotationTopologyQueue
)

// SystemNodeAffinity requires AKS control pods to use the system pool while
// preserving portability to clusters whose nodes do not carry the AKS label.
func SystemNodeAffinity() map[string]any {
	return map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{
					map[string]any{
						"matchExpressions": []any{
							map[string]any{
								"key":      AKSNodePoolModeLabel,
								"operator": "In",
								"values":   []any{AKSSystemNodePoolMode},
							},
						},
					},
					map[string]any{
						"matchExpressions": []any{
							map[string]any{
								"key":      AKSNodePoolModeLabel,
								"operator": "DoesNotExist",
							},
						},
					},
				},
			},
		},
	}
}

// WithoutKueueTopologyAnnotations copies annotations while removing pod-set
// topology requests that apply to execution workers but not control-plane pods.
func WithoutKueueTopologyAnnotations(annotations map[string]string) map[string]string {
	out := make(map[string]string, len(annotations))
	for key, value := range annotations {
		switch key {
		case requiredTopologyAnnotation, preferredTopologyAnnotation, unconstrainedTopologyAnnot:
			continue
		default:
			out[key] = value
		}
	}
	return out
}

var labelValueRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// legacyGPUClassAliases maps pre-canonical gpu_class spellings -- which
// folded interconnect/placement terms (NVLink, standalone) into the
// hardware name -- to their canonical, hardware-only replacement. These
// aliases exist only for one compatibility window: NormalizeGPUClass accepts
// them as input so existing profiles/configs/presets keep working, but every
// validation, rendering, status, and explain surface operates on the
// canonical value, and no new aliases should be added here.
var legacyGPUClassAliases = map[string]string{
	"a100-nvlink-80gb":     GPUClassA10080GB,
	"h100-standalone-95gb": GPUClassH10095GB,
	"h200-nvlink-141gb":    GPUClassH200141GB,
}

// NormalizeGPUClass maps a researcher-supplied gpu_class value (from a CLI
// flag, run config, preset, or profile spec.topology.gpuClass) to its
// canonical hardware-only spelling. It returns the canonical value and
// whether the input was a deprecated legacy alias that should be migrated.
// Unrecognized values (including "any" and already-canonical values) are
// returned unchanged with deprecatedAlias=false; callers still run the
// result through validEnum to reject truly invalid classes.
func NormalizeGPUClass(v string) (canonical string, deprecatedAlias bool) {
	normalized := normalizeLabelValue(v)
	if mapped, ok := legacyGPUClassAliases[normalized]; ok {
		return mapped, true
	}
	return normalized, false
}

func IsSupportedGPUClass(v string) bool {
	canonical, _ := NormalizeGPUClass(v)
	switch canonical {
	case GPUClassAny, GPUClassA10080GB, GPUClassH10095GB, GPUClassH200141GB:
		return true
	default:
		return false
	}
}

func ValidateGPUClassNodeSelector(gpuClass string, selector map[string]string) error {
	canonical, _ := NormalizeGPUClass(gpuClass)
	selected := strings.TrimSpace(selector[workloadmeta.NodeLabelGPUClass])
	if selected == "" || canonical == "" {
		return nil
	}
	if canonical == GPUClassAny {
		return fmt.Errorf("gpu_class %q is unconstrained and cannot be combined with node selector %s=%q", canonical, workloadmeta.NodeLabelGPUClass, selected)
	}
	if selected != canonical {
		return fmt.Errorf("gpu_class %q requires %s=%q, but the workload node selector uses %q", canonical, workloadmeta.NodeLabelGPUClass, canonical, selected)
	}
	return nil
}

// ResolveGPUClass returns the effective canonical gpu_class after applying an
// explicit override to a profile's topology contract.
func ResolveGPUClass(p profile.Profile, override string) (string, bool) {
	raw, _ := asMap(p.Spec["topology"])
	gpuClass := stringFrom(raw, "gpuClass", "gpuFlavor", "flavor")
	if override != "" {
		gpuClass = override
	}
	return NormalizeGPUClass(gpuClass)
}

// Options are caller-provided overrides. They are intentionally strings so the
// CLI can pass flags through directly; Build normalizes and validates them.
type Options struct {
	Team            string
	Lane            string
	Mode            string
	Placement       string
	Shape           string
	GPUClass        string
	CheckpointEvery string
	QueueName       string
	// WorkloadPriorityClassName controls Kueue admission ordering.
	WorkloadPriorityClassName string
	// PodPriorityClassName controls Kubernetes scheduler priority/preemption.
	PodPriorityClassName string
	// PriorityTier is a user-facing shorthand for Tau-managed priority classes.
	// Supported values are "default" and "priority".
	PriorityTier string
	// RequiredTopology is a platform-owned ResourceFlavor requirement copied to
	// generated GPU pod templates after live queue resolution.
	RequiredTopology string
	// DisableKueueTopologyAnnotations keeps Tau's topology labels/explainability
	// while omitting Kueue TAS podset annotations. Use this when the selected
	// ResourceFlavor does not define spec.topologyName; Kueue rejects podset
	// topology requests for flavors without a topology.
	DisableKueueTopologyAnnotations bool
	// DisableDefaultPriorities omits TauGrid default priority classes unless the
	// caller/preset supplied explicit priority names.
	DisableDefaultPriorities bool
}

// Plan is the topology-aware scheduling decoration Render should apply.
type Plan struct {
	Labels               map[string]string
	Annotations          map[string]string
	NodeSelector         map[string]string
	QueueName            string
	PodPriorityClassName string
}

// Build returns the Tau/Kueue metadata implied by a profile's spec.topology
// and any caller overrides. Profiles without topology intent and calls without
// topology flags produce an empty Plan.
func Build(p profile.Profile, o Options) (Plan, error) {
	raw, hasSpec := asMap(p.Spec["topology"])
	if !hasSpec && !o.hasValues() {
		return Plan{}, nil
	}

	if enabled, ok := boolFrom(raw, "enabled"); ok && !enabled {
		reason := stringFrom(raw, "disabledReason", "reason")
		if reason == "" {
			reason = "profile topology contract is disabled"
		}
		return Plan{}, fmt.Errorf("profile %q topology disabled: %s", p.Name, reason)
	}

	spec := contractFromProfile(p, raw)
	spec.apply(o)
	spec.normalize()
	spec.defaultMode()
	if spec.priorityTier != "" {
		spec.applyPriorityTierDefaults()
	} else if !spec.disableDefaultPriorities {
		spec.defaultPriorities()
	}

	if err := spec.validate(p.Name); err != nil {
		return Plan{}, err
	}

	plan := Plan{
		Labels:       map[string]string{},
		Annotations:  map[string]string{},
		NodeSelector: map[string]string{},
		QueueName:    spec.queueName(),
	}
	if spec.gpuClass != "" {
		plan.Labels[LabelGPUClass] = spec.gpuClass
		if spec.gpuClass != GPUClassAny {
			plan.NodeSelector[workloadmeta.NodeLabelGPUClass] = spec.gpuClass
		}
	}
	if spec.workloadPriorityClassName != "" {
		plan.Labels[workloadPriorityLabel] = spec.workloadPriorityClassName
	}
	if spec.mode == "elastic" && plan.Labels[workloadPriorityLabel] == "" {
		plan.Labels[workloadPriorityLabel] = DefaultElasticWorkloadPrio
	}

	plan.PodPriorityClassName = spec.podPriorityClassName
	if spec.mode == "elastic" && plan.PodPriorityClassName == "" {
		plan.PodPriorityClassName = defaultElasticPodPriority
	}

	if !spec.disableKueueTopology {
		if spec.requiredTopology != "" {
			plan.Annotations[requiredTopologyAnnotation] = spec.requiredTopology
		}
		switch spec.placement {
		case "single-node-nvlink":
			if spec.requiredTopology != "" && spec.requiredTopology != hostnameTopology {
				return Plan{}, fmt.Errorf(
					"profile %q topology: ResourceFlavor requires %s=%q, but placement=single-node-nvlink requires %q",
					p.Name, requiredTopologyAnnotation, spec.requiredTopology, hostnameTopology)
			}
			plan.Annotations[requiredTopologyAnnotation] = hostnameTopology
		case "multi-node-nccl":
			if spec.requiredTopology != "" {
				return Plan{}, fmt.Errorf(
					"profile %q topology: ResourceFlavor requires %s=%q, which conflicts with placement=multi-node-nccl",
					p.Name, requiredTopologyAnnotation, spec.requiredTopology)
			}
			// Multi-node gangs must span hosts. The managed worker Topologies
			// expose hostname only, so an explicit hostname preference would
			// first try to co-locate the gang and broader levels are invalid.
			plan.Annotations[unconstrainedTopologyAnnot] = "true"
		case "independent", "elastic-workers":
			if spec.requiredTopology != "" {
				return Plan{}, fmt.Errorf(
					"profile %q topology: ResourceFlavor requires %s=%q, which conflicts with placement=%s",
					p.Name, requiredTopologyAnnotation, spec.requiredTopology, spec.placement)
			}
			plan.Annotations[unconstrainedTopologyAnnot] = "true"
		}
	}

	return plan, nil
}

type contract struct {
	team                      string
	lane                      string
	mode                      string
	placement                 string
	shape                     string
	gpuClass                  string
	checkpointEvery           string
	queue                     string
	podPriorityClassName      string
	workloadPriorityClassName string
	priorityTier              string
	requiredTopology          string
	disabledReason            string
	preemptible               bool
	hasCheckpoint             bool
	disableKueueTopology      bool
	disableDefaultPriorities  bool
}

func contractFromProfile(p profile.Profile, raw map[string]any) contract {
	c := contract{
		team:                      stringFrom(raw, "team"),
		lane:                      firstNonEmpty(stringFrom(raw, "lane"), laneFromProfile(p)),
		mode:                      stringFrom(raw, "mode"),
		placement:                 stringFrom(raw, "placement", "class", "type", "topologyClass"),
		shape:                     stringFrom(raw, "shape"),
		gpuClass:                  stringFrom(raw, "gpuClass", "gpuFlavor", "flavor"),
		checkpointEvery:           stringFrom(raw, "checkpointEvery", "checkpointInterval"),
		queue:                     stringFrom(raw, "queueName", "localQueue"),
		podPriorityClassName:      stringFrom(raw, "podPriorityClassName"),
		workloadPriorityClassName: stringFrom(raw, "workloadPriorityClassName"),
		priorityTier:              stringFrom(raw, "priorityTier", "priority"),
		disabledReason:            stringFrom(raw, "disabledReason", "reason"),
	}
	if pol, ok := asMap(p.Spec["policy"]); ok {
		if preemptible, ok := boolFrom(pol, "preemptable", "preemptible"); ok {
			c.preemptible = preemptible
		}
		if checkpoint, ok := boolFrom(pol, "checkpointOnPreempt", "resumeFromCheckpoint"); ok && checkpoint {
			c.hasCheckpoint = true
		}
		if retry, ok := asMap(pol["retry"]); ok {
			if checkpoint, ok := boolFrom(retry, "resumeFromCheckpoint", "checkpointOnRetry"); ok && checkpoint {
				c.hasCheckpoint = true
			}
		}
	}
	if checkpoint, ok := boolFrom(raw, "checkpointOnPreempt", "resumeFromCheckpoint"); ok && checkpoint {
		c.hasCheckpoint = true
	}
	if c.checkpointEvery != "" {
		c.hasCheckpoint = true
	}
	return c
}

func (c *contract) apply(o Options) {
	if o.Team != "" {
		c.team = o.Team
	}
	if o.Lane != "" {
		c.lane = o.Lane
	}
	if o.Mode != "" {
		c.mode = o.Mode
	}
	if o.Placement != "" {
		c.placement = o.Placement
	}
	if o.Shape != "" {
		c.shape = o.Shape
	}
	if o.GPUClass != "" {
		c.gpuClass = o.GPUClass
	}
	if o.CheckpointEvery != "" {
		c.checkpointEvery = o.CheckpointEvery
		c.hasCheckpoint = true
	}
	if o.QueueName != "" {
		c.queue = o.QueueName
	}
	if o.PriorityTier != "" {
		c.priorityTier = o.PriorityTier
		c.workloadPriorityClassName = ""
		c.podPriorityClassName = ""
	}
	if o.RequiredTopology != "" {
		c.requiredTopology = strings.TrimSpace(o.RequiredTopology)
	}
	if o.WorkloadPriorityClassName != "" {
		c.workloadPriorityClassName = o.WorkloadPriorityClassName
	}
	if o.PodPriorityClassName != "" {
		c.podPriorityClassName = o.PodPriorityClassName
	}
	if o.DisableKueueTopologyAnnotations {
		c.disableKueueTopology = true
	}
	if o.DisableDefaultPriorities {
		c.disableDefaultPriorities = true
	}
}

func (c *contract) normalize() {
	c.team = normalizeLabelValue(c.team)
	c.lane = normalizeLabelValue(c.lane)
	c.mode = normalizeLabelValue(c.mode)
	c.placement = normalizeLabelValue(c.placement)
	c.shape = normalizeLabelValue(c.shape)
	c.gpuClass, _ = NormalizeGPUClass(c.gpuClass)
	c.queue = normalizeLabelValue(c.queue)
	c.podPriorityClassName = normalizeLabelValue(c.podPriorityClassName)
	c.workloadPriorityClassName = normalizeLabelValue(c.workloadPriorityClassName)
	c.priorityTier = normalizeLabelValue(c.priorityTier)
}

func (c *contract) defaultMode() {
	if c.mode == "" {
		if c.lane == "elastic" {
			c.mode = "elastic"
		} else {
			c.mode = "fixed"
		}
	}
	if c.lane == "" && c.mode == "elastic" {
		c.lane = "elastic"
	}
}

func (c *contract) defaultPriorities() {
	switch c.lane {
	case "eval":
		if c.workloadPriorityClassName == "" {
			c.workloadPriorityClassName = defaultEvalWorkloadPrio
		}
		if c.podPriorityClassName == "" {
			c.podPriorityClassName = defaultEvalPodPriority
		}
	case "training", "large-memory":
		if c.workloadPriorityClassName == "" {
			c.workloadPriorityClassName = defaultTrainWorkloadPrio
		}
		if c.podPriorityClassName == "" {
			c.podPriorityClassName = DefaultTrainPodPriority
		}
	case "elastic":
		if c.workloadPriorityClassName == "" {
			c.workloadPriorityClassName = DefaultElasticWorkloadPrio
		}
		if c.podPriorityClassName == "" {
			c.podPriorityClassName = defaultElasticPodPriority
		}
	}
}

func (c *contract) applyPriorityTierDefaults() {
	workloadPriority, podPriority := "", ""
	switch c.priorityTier {
	case "default":
		switch c.lane {
		case "eval":
			workloadPriority, podPriority = defaultEvalWorkloadPrio, defaultEvalPodPriority
		case "elastic":
			workloadPriority, podPriority = DefaultElasticWorkloadPrio, defaultElasticPodPriority
		default:
			workloadPriority, podPriority = defaultTrainWorkloadPrio, DefaultTrainPodPriority
		}
	case "priority":
		switch c.lane {
		case "eval":
			workloadPriority, podPriority = priorityEvalWorkloadPrio, priorityEvalPodPriority
		default:
			workloadPriority, podPriority = priorityTrainWorkloadPrio, priorityTrainPodPriority
		}
	}
	if c.workloadPriorityClassName == "" {
		c.workloadPriorityClassName = workloadPriority
	}
	if c.podPriorityClassName == "" {
		c.podPriorityClassName = podPriority
	}
}

func (c contract) validate(profileName string) error {
	if err := validEnum("mode", c.mode, "fixed", "elastic"); err != nil {
		return fmt.Errorf("profile %q topology: %w", profileName, err)
	}
	if c.team != "" && !validLabelValue(c.team) {
		return fmt.Errorf("profile %q topology: team %q is not a valid k8s label value", profileName, c.team)
	}
	if c.lane != "" {
		if err := validEnum("lane", c.lane, "eval", "training", "large-memory", "elastic"); err != nil {
			return fmt.Errorf("profile %q topology: %w", profileName, err)
		}
	}
	if c.placement != "" {
		if err := validEnum("placement", c.placement, "independent", "single-node-nvlink", "multi-node-nccl", "elastic-workers"); err != nil {
			return fmt.Errorf("profile %q topology: %w", profileName, err)
		}
	}
	if c.gpuClass != "" {
		if err := validEnum("gpuClass", c.gpuClass, GPUClassAny, GPUClassA10080GB, GPUClassH10095GB, GPUClassH200141GB); err != nil {
			return fmt.Errorf("profile %q topology: %w", profileName, err)
		}
	}
	if c.workloadPriorityClassName != "" && !validLabelValue(c.workloadPriorityClassName) {
		return fmt.Errorf("profile %q topology: workloadPriorityClassName %q is not a valid k8s label value", profileName, c.workloadPriorityClassName)
	}
	if c.podPriorityClassName != "" && !validLabelValue(c.podPriorityClassName) {
		return fmt.Errorf("profile %q topology: podPriorityClassName %q is not a valid k8s label value", profileName, c.podPriorityClassName)
	}
	if c.priorityTier != "" {
		if err := validEnum("priority", c.priorityTier, "default", "priority"); err != nil {
			return fmt.Errorf("profile %q topology: %w", profileName, err)
		}
	}
	if c.priorityTier == "priority" && c.lane == "elastic" {
		return fmt.Errorf("profile %q topology: priority=priority is not supported for lane=elastic; use explicit priority class names if needed", profileName)
	}
	if c.mode == "elastic" {
		if c.lane != "elastic" {
			return fmt.Errorf("profile %q topology: mode=elastic must route through lane=elastic, got lane=%q", profileName, c.lane)
		}
		if !c.preemptible {
			return fmt.Errorf("profile %q topology: mode=elastic requires policy.preemptable=true", profileName)
		}
		if !c.hasCheckpoint {
			return fmt.Errorf("profile %q topology: mode=elastic requires checkpoint/restart semantics (policy.checkpointOnPreempt=true or topology.checkpointEvery)", profileName)
		}
		if c.gpuClass == GPUClassH200141GB {
			return fmt.Errorf("profile %q topology: elastic jobs cannot request scarce %s", profileName, GPUClassH200141GB)
		}
		if c.placement != "" && c.placement != "independent" && c.placement != "elastic-workers" {
			return fmt.Errorf("profile %q topology: mode=elastic must use independent or elastic-workers placement, got %q", profileName, c.placement)
		}
	}
	if c.lane == "elastic" && c.mode != "elastic" {
		return fmt.Errorf("profile %q topology: lane=elastic must use mode=elastic", profileName)
	}
	if c.lane == "eval" && c.gpuClass == GPUClassH200141GB {
		return fmt.Errorf("profile %q topology: eval lane cannot request scarce %s", profileName, GPUClassH200141GB)
	}
	if c.gpuClass == GPUClassH200141GB && c.lane != "" && c.lane != "large-memory" {
		return fmt.Errorf("profile %q topology: %s is reserved for lane=large-memory, got lane=%q", profileName, GPUClassH200141GB, c.lane)
	}
	return nil
}

func (c contract) queueName() string {
	if c.queue != "" {
		return c.queue
	}
	if c.team == "" && c.lane == "" {
		return ""
	}
	return SharedGPUQueueName
}

func (o Options) hasValues() bool {
	return o.Team != "" || o.Lane != "" || o.Mode != "" || o.Placement != "" ||
		o.Shape != "" || o.GPUClass != "" || o.CheckpointEvery != "" || o.QueueName != "" ||
		o.WorkloadPriorityClassName != "" || o.PodPriorityClassName != "" || o.PriorityTier != "" ||
		o.RequiredTopology != "" || o.DisableDefaultPriorities
}

func laneFromProfile(p profile.Profile) string {
	if v, ok := p.Spec["lane"].(string); ok && v != "" {
		return v
	}
	return p.Lane
}

func normalizeLabelValue(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.ReplaceAll(v, "_", "-")
	v = strings.ReplaceAll(v, " ", "-")
	return v
}

func validLabelValue(v string) bool {
	return len(v) <= 63 && labelValueRE.MatchString(v)
}

func validEnum(name, got string, allowed ...string) error {
	if got == "" {
		return nil
	}
	for _, v := range allowed {
		if got == v {
			return nil
		}
	}
	return fmt.Errorf("%s=%q is invalid (allowed: %s)", name, got, strings.Join(allowed, "|"))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func stringFrom(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func boolFrom(m map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case bool:
			return v, true
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true", "yes", "1":
				return true, true
			case "false", "no", "0":
				return false, true
			}
		}
	}
	return false, false
}

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := map[string]any{}
		for k, v := range m {
			if s, ok := k.(string); ok {
				out[s] = v
			}
		}
		return out, len(out) > 0
	default:
		return nil, false
	}
}
