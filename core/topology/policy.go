package topology

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
	"gopkg.in/yaml.v3"
)

const (
	defaultPolicyEnv = "TAU_TOPOLOGY_POLICY"
	LabelPreset      = workloadmeta.LabelPreset
)

// Policy is the platform-owned mapping from researcher-friendly presets to
// Tau/Kueue topology intent.
type Policy struct {
	Name        string
	Description string
	SourceFile  string
	Presets     map[string]Preset
}

// Preset is one researcher-facing Azure compute choice.
type Preset struct {
	Name                      string
	Description               string
	Profile                   string
	Team                      string
	Lane                      string
	Mode                      string
	Placement                 string
	Shape                     string
	GPUClass                  string
	QueueName                 string
	ClusterQueue              string
	Namespace                 string
	ResourceFlavor            string
	TopologyName              string
	CheckpointEvery           string
	WorkloadPriorityClassName string
	PodPriorityClassName      string
	Reclaimable               bool
	Disabled                  bool
	DisabledReason            string
	Explain                   string
	// Workers is the number of pod replicas (each holding `Shape` GPUs)
	// the preset implies. 1 → single-node (default; byte-identical render
	// to today). >1 → multi-node, requires placement=multi-node-nccl.
	Workers int
}

// ResolvedPreset is what CLI callers need after loading a policy.
type ResolvedPreset struct {
	PolicyName  string
	SourceFile  string
	Preset      Preset
	Options     Options
	Annotations map[string]string
	Labels      map[string]string
}

// WithDRAQueue maps a resolved device-plugin preset onto the parallel DRA
// admission contract. Worker ResourceFlavors use the same series name with a
// -dra suffix, while the DRA ClusterQueue intentionally omits TAS topology.
func WithDRAQueue(p ResolvedPreset) ResolvedPreset {
	if !usesManagedSharedGPUQueue(p.Preset) {
		return p
	}
	p.Preset.QueueName = SharedDRAQueueName
	p.Preset.ClusterQueue = sharedDRAClusterQueueName
	if p.Preset.ResourceFlavor != "" && !strings.HasSuffix(p.Preset.ResourceFlavor, "-dra") {
		p.Preset.ResourceFlavor += "-dra"
	}
	p.Preset.TopologyName = ""
	p.Options.QueueName = SharedDRAQueueName
	p.Options.DisableKueueTopologyAnnotations = true
	p.Annotations = cloneStringMap(p.Annotations)
	p.Annotations[AnnotationTopologyQueue] = SharedDRAQueueName
	p.Annotations[workloadmeta.AnnotationClusterQueue] = sharedDRAClusterQueueName
	if p.Preset.ResourceFlavor != "" {
		p.Annotations[workloadmeta.AnnotationResourceFlavor] = p.Preset.ResourceFlavor
	}
	delete(p.Annotations, workloadmeta.AnnotationKueueTopology)
	return p
}

func usesManagedSharedGPUQueue(p Preset) bool {
	queueName := strings.TrimSpace(p.QueueName)
	clusterQueue := strings.TrimSpace(p.ClusterQueue)
	switch queueName {
	case "", SharedGPUQueueName:
		if clusterQueue != "" && clusterQueue != SharedGPUClusterQueueName {
			return false
		}
	case SharedDRAQueueName:
		if clusterQueue != "" && clusterQueue != sharedDRAClusterQueueName {
			return false
		}
	default:
		return false
	}
	if p.ResourceFlavor == "" {
		return true
	}
	_, managed := ManagedGPUSeriesForFlavor(p.ResourceFlavor)
	return managed
}

// ManagedGPUSeriesForFlavor returns the node label value enforced by managed
// Azure GPU ResourceFlavors. DRA flavors share the underlying series selector.
func ManagedGPUSeriesForFlavor(flavor string) (string, bool) {
	series := strings.TrimSuffix(strings.TrimSpace(flavor), "-dra")
	switch series {
	case "ndm-a100-v4", "nd-h200-v5":
		return series, true
	default:
		return "", false
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type policyDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Description string               `yaml:"description"`
		Presets     map[string]presetDoc `yaml:"presets"`
	} `yaml:"spec"`
}

type presetDoc struct {
	Description               string `yaml:"description"`
	Profile                   string `yaml:"profile"`
	Team                      string `yaml:"team"`
	Lane                      string `yaml:"lane"`
	Mode                      string `yaml:"mode"`
	Placement                 string `yaml:"placement"`
	Shape                     string `yaml:"shape"`
	GPUClass                  string `yaml:"gpuClass"`
	QueueName                 string `yaml:"queue"`
	ClusterQueue              string `yaml:"clusterQueue"`
	Namespace                 string `yaml:"namespace"`
	ResourceFlavor            string `yaml:"resourceFlavor"`
	TopologyName              string `yaml:"topologyName"`
	CheckpointEvery           string `yaml:"checkpointEvery"`
	WorkloadPriorityClassName string `yaml:"workloadPriorityClassName"`
	PodPriorityClassName      string `yaml:"podPriorityClassName"`
	Reclaimable               bool   `yaml:"reclaimable"`
	Disabled                  bool   `yaml:"disabled"`
	DisabledReason            string `yaml:"disabledReason"`
	Explain                   string `yaml:"explain"`
	Workers                   int    `yaml:"workers"`
}

// ResolvePreset loads the platform policy catalog and returns one preset by
// name. It fails on disabled presets so researcher submits don't silently land
// on unavailable Azure capacity.
func ResolvePreset(policyPath, name string) (ResolvedPreset, error) {
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		return ResolvedPreset{}, err
	}
	preset, ok := pol.Presets[name]
	if !ok {
		return ResolvedPreset{}, fmt.Errorf("topology preset %q not found in %s (available: %s)", name, pol.SourceFile, strings.Join(pol.Names(), ", "))
	}
	if preset.Disabled {
		reason := preset.DisabledReason
		if reason == "" {
			reason = "preset is disabled by platform policy"
		}
		return ResolvedPreset{}, fmt.Errorf("topology preset %q is disabled: %s", name, reason)
	}
	return pol.resolve(preset), nil
}

// SuggestPreset infers the preset that fits a researcher's intent expressed
// as (team, lane, gpus, workers). It exists so the common case — "I'm on the
// research team, I want to train with 2 GPUs" — is one CLI flag (or zero,
// with a default team) instead of a memorized preset name like
// azure.research.training.2x.
//
// Resolution rules:
//
//  1. Filter to enabled presets where Team and Lane match exactly, the
//     shape's GPU-count prefix equals gpus, and Workers equals workers.
//  2. Prefer non-reclaimable matches over reclaimable ones (training default
//     should not silently land on preemptible capacity).
//  3. If still ambiguous, return an error listing candidates so the caller
//     can pass --preset explicitly. Better to fail clearly than to pick.
//
// `workers` defaults to 1 (caller passes 1 for single-node intent). Multi-node
// callers pass workers > 1 and the function only matches presets with the
// matching Workers field set in the policy YAML.
//
// Returns errNoPresetMatch when nothing matches so callers can decide whether
// to fall through to the no-preset path or surface the error.
func SuggestPreset(policyPath, team, lane string, gpus, workers int) (ResolvedPreset, error) {
	if team == "" || lane == "" || gpus < 1 {
		return ResolvedPreset{}, fmt.Errorf("preset suggestion requires non-empty team, lane, and gpus>=1 (got team=%q, lane=%q, gpus=%d)", team, lane, gpus)
	}
	if workers < 1 {
		return ResolvedPreset{}, fmt.Errorf("preset suggestion requires workers>=1 (got %d)", workers)
	}
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		return ResolvedPreset{}, err
	}
	var matches []Preset
	for _, name := range pol.Names() {
		p := pol.Presets[name]
		if p.Disabled || p.Team != team || p.Lane != lane {
			continue
		}
		n, ok, _ := GPUCountFromShape(p.Shape)
		if !ok || n != gpus {
			continue
		}
		if p.Workers != workers {
			continue
		}
		matches = append(matches, p)
	}
	if len(matches) == 0 {
		return ResolvedPreset{}, fmt.Errorf("%w: team=%s lane=%s gpus=%d workers=%d in %s; pass --preset explicitly or inspect the platform topology policy for available presets", errNoPresetMatch, team, lane, gpus, workers, pol.SourceFile)
	}
	if len(matches) > 1 {
		var nonReclaim []Preset
		for _, p := range matches {
			if !p.Reclaimable {
				nonReclaim = append(nonReclaim, p)
			}
		}
		if len(nonReclaim) >= 1 {
			matches = nonReclaim
		}
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, p := range matches {
			names[i] = p.Name
		}
		sort.Strings(names)
		return ResolvedPreset{}, fmt.Errorf("multiple presets match team=%s lane=%s gpus=%d workers=%d in %s: %s; pass --preset explicitly", team, lane, gpus, workers, pol.SourceFile, strings.Join(names, ", "))
	}
	return pol.resolve(matches[0]), nil
}

// errNoPresetMatch is returned by SuggestPreset when no enabled preset matches
// the given (team, lane, gpus) intent. Callers can use errors.Is to detect
// this and fall back to the no-preset path instead of failing the jobrender.
var errNoPresetMatch = errors.New("no enabled preset matches intent")

// GPUCountFromShape parses the GPU-count prefix from a shape string such as
// "2xa100-80gb" or "8xh200-141gb". Returns (count, true, nil) for shapes that
// match the Nx<sku> grammar. Returns (0, false, nil) for shapes that don't
// have an Nx prefix (treated as no-op). Returns an error only for malformed
// prefixes that look like Nx but aren't positive integers.
func GPUCountFromShape(shape string) (int, bool, error) {
	count, _, ok := strings.Cut(shape, "x")
	if !ok || count == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("shape %q: GPU count prefix must be a positive integer", shape)
	}
	return n, true, nil
}

// LoadPolicy loads and validates a topology policy. Empty path uses
// TAU_TOPOLOGY_POLICY, then the in-tree Azure policy fallback, then the
// embedded default policy shipped in the Tau binary.
func LoadPolicy(path string) (Policy, error) {
	resolved, err := resolvePolicyPath(path)
	if err != nil {
		return Policy{}, err
	}
	data := embeddedDefaultPolicy
	if resolved != embeddedPolicySource {
		var err error
		data, err = os.ReadFile(resolved)
		if err != nil {
			return Policy{}, fmt.Errorf("read topology policy %s: %w", resolved, err)
		}
	}
	var doc policyDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Policy{}, fmt.Errorf("parse topology policy %s: %w", resolved, err)
	}
	if doc.Kind != "TopologyPolicy" {
		return Policy{}, fmt.Errorf("topology policy %s: kind must be TopologyPolicy, got %q", resolved, doc.Kind)
	}
	pol := Policy{
		Name:        doc.Metadata.Name,
		Description: doc.Spec.Description,
		SourceFile:  resolved,
		Presets:     map[string]Preset{},
	}
	if pol.Name == "" {
		return Policy{}, fmt.Errorf("topology policy %s: metadata.name is required", resolved)
	}
	if len(doc.Spec.Presets) == 0 {
		return Policy{}, fmt.Errorf("topology policy %s: spec.presets is required", resolved)
	}
	for name, raw := range doc.Spec.Presets {
		preset := Preset{
			Name:                      normalizePresetName(name),
			Description:               strings.TrimSpace(raw.Description),
			Profile:                   normalizeLabelValue(raw.Profile),
			Team:                      normalizeLabelValue(raw.Team),
			Lane:                      normalizeLabelValue(raw.Lane),
			Mode:                      normalizeLabelValue(raw.Mode),
			Placement:                 normalizeLabelValue(raw.Placement),
			Shape:                     normalizeLabelValue(raw.Shape),
			GPUClass:                  normalizeLabelValue(raw.GPUClass),
			QueueName:                 normalizeLabelValue(raw.QueueName),
			ClusterQueue:              normalizeLabelValue(raw.ClusterQueue),
			Namespace:                 normalizeLabelValue(raw.Namespace),
			ResourceFlavor:            normalizeLabelValue(raw.ResourceFlavor),
			TopologyName:              normalizeLabelValue(raw.TopologyName),
			CheckpointEvery:           strings.TrimSpace(raw.CheckpointEvery),
			WorkloadPriorityClassName: normalizeLabelValue(raw.WorkloadPriorityClassName),
			PodPriorityClassName:      normalizeLabelValue(raw.PodPriorityClassName),
			Reclaimable:               raw.Reclaimable,
			Disabled:                  raw.Disabled,
			DisabledReason:            strings.TrimSpace(raw.DisabledReason),
			Explain:                   strings.TrimSpace(raw.Explain),
			Workers:                   raw.Workers,
		}
		if preset.Workers == 0 {
			preset.Workers = 1
		}
		preset.defaultPriorities()
		if preset.Name == "" {
			return Policy{}, fmt.Errorf("topology policy %s: preset name %q normalizes to empty", resolved, name)
		}
		if err := preset.validate(); err != nil {
			return Policy{}, fmt.Errorf("topology policy %s preset %q: %w", resolved, name, err)
		}
		pol.Presets[preset.Name] = preset
	}
	return pol, nil
}

// resolvePolicyPath finds the platform policy catalog.
func resolvePolicyPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	if env := os.Getenv(defaultPolicyEnv); env != "" {
		return env, nil
	}
	return embeddedPolicySource, nil
}

func (p Policy) resolve(preset Preset) ResolvedPreset {
	return ResolvedPreset{
		PolicyName: p.Name,
		SourceFile: p.SourceFile,
		Preset:     preset,
		Options: Options{
			Team:                            preset.Team,
			Lane:                            preset.Lane,
			Mode:                            preset.Mode,
			Placement:                       preset.Placement,
			Shape:                           preset.Shape,
			GPUClass:                        preset.GPUClass,
			CheckpointEvery:                 preset.CheckpointEvery,
			QueueName:                       preset.QueueName,
			WorkloadPriorityClassName:       preset.WorkloadPriorityClassName,
			PodPriorityClassName:            preset.PodPriorityClassName,
			DisableKueueTopologyAnnotations: preset.TopologyName == "",
		},
		Labels:      preset.labels(),
		Annotations: preset.annotations(p.SourceFile),
	}
}

func (p Preset) labels() map[string]string {
	out := map[string]string{
		LabelPreset: p.Name,
	}
	if p.Reclaimable {
		out[workloadmeta.LabelPreemptible] = "true"
		out[workloadmeta.LabelReclaimable] = "true"
	}
	return out
}

func (p Policy) Names() []string {
	names := make([]string, 0, len(p.Presets))
	for name := range p.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p Preset) annotations(source string) map[string]string {
	out := map[string]string{
		workloadmeta.AnnotationPolicySource: filepath.Base(source),
	}
	if p.Description != "" {
		out[workloadmeta.AnnotationPresetDesc] = p.Description
	}
	if p.Explain != "" {
		out[workloadmeta.AnnotationPresetExplain] = p.Explain
	}
	if p.ResourceFlavor != "" {
		out[workloadmeta.AnnotationResourceFlavor] = p.ResourceFlavor
	}
	if p.ClusterQueue != "" {
		out[workloadmeta.AnnotationClusterQueue] = p.ClusterQueue
	}
	if p.TopologyName != "" {
		out[workloadmeta.AnnotationKueueTopology] = p.TopologyName
	}
	if p.WorkloadPriorityClassName != "" {
		out[workloadmeta.AnnotationWorkloadPriorityClass] = p.WorkloadPriorityClassName
	}
	if p.PodPriorityClassName != "" {
		out[workloadmeta.AnnotationPodPriorityClass] = p.PodPriorityClassName
	}
	if p.Reclaimable {
		out[workloadmeta.LabelReclaimable] = "true"
	}
	return out
}

func (p *Preset) defaultPriorities() {
	if p.Reclaimable {
		p.WorkloadPriorityClassName = DefaultElasticWorkloadPrio
		p.PodPriorityClassName = defaultElasticPodPriority
		return
	}
	switch p.Lane {
	case "eval":
		if p.WorkloadPriorityClassName == "" {
			p.WorkloadPriorityClassName = defaultEvalWorkloadPrio
		}
		if p.PodPriorityClassName == "" {
			p.PodPriorityClassName = defaultEvalPodPriority
		}
	case "training", "large-memory":
		if p.WorkloadPriorityClassName == "" {
			p.WorkloadPriorityClassName = defaultTrainWorkloadPrio
		}
		if p.PodPriorityClassName == "" {
			p.PodPriorityClassName = DefaultTrainPodPriority
		}
	case "elastic":
		if p.WorkloadPriorityClassName == "" {
			p.WorkloadPriorityClassName = DefaultElasticWorkloadPrio
		}
		if p.PodPriorityClassName == "" {
			p.PodPriorityClassName = defaultElasticPodPriority
		}
	}
}

func (p Preset) validate() error {
	if p.Disabled {
		if p.DisabledReason == "" {
			return errors.New("disabled presets require disabledReason")
		}
	}
	// Profile field is optional (legacy support; now handled via packs)
	if p.Team == "" {
		return errors.New("team is required")
	}
	if p.Lane == "" {
		return errors.New("lane is required")
	}
	if p.QueueName == "" {
		return errors.New("queue is required")
	}
	if p.ClusterQueue == "" {
		return errors.New("clusterQueue is required")
	}
	if p.GPUClass == "" {
		return errors.New("gpuClass is required")
	}
	if p.Workers < 1 {
		return fmt.Errorf("workers: want ≥ 1 (got %d)", p.Workers)
	}
	if p.Workers > 1 && p.Placement != "multi-node-nccl" {
		return fmt.Errorf("workers > 1 (got %d) requires placement=multi-node-nccl (got %q)", p.Workers, p.Placement)
	}
	c := contract{
		team:            p.Team,
		lane:            p.Lane,
		mode:            p.Mode,
		placement:       p.Placement,
		shape:           p.Shape,
		gpuClass:        p.GPUClass,
		checkpointEvery: p.CheckpointEvery,
		queue:           p.QueueName,
		preemptible:     true,
		hasCheckpoint:   p.CheckpointEvery != "" || p.Mode != "elastic",
	}
	if err := c.validate(p.Name); err != nil {
		return err
	}
	if p.Namespace != "" && !validLabelValue(p.Namespace) {
		return fmt.Errorf("namespace %q is not a valid k8s label value", p.Namespace)
	}
	if p.WorkloadPriorityClassName != "" && !validLabelValue(p.WorkloadPriorityClassName) {
		return fmt.Errorf("workloadPriorityClassName %q is not a valid k8s label value", p.WorkloadPriorityClassName)
	}
	if p.PodPriorityClassName != "" && !validLabelValue(p.PodPriorityClassName) {
		return fmt.Errorf("podPriorityClassName %q is not a valid k8s label value", p.PodPriorityClassName)
	}
	return nil
}

func normalizePresetName(v string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(v)), ".")
}
