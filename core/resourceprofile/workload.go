// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	ModeFixed   = "fixed"
	ModeElastic = "elastic"

	PlacementIndependent      = "independent"
	PlacementSingleNodeNVLink = "single-node-nvlink"
	PlacementMultiNodeNCCL    = "multi-node-nccl"

	ExecutionTargetSingleCluster  ExecutionTarget = "singleCluster"
	ExecutionTargetMultiKueueBeta ExecutionTarget = "multiKueueBeta"

	ConditionReady                = "Ready"
	ConditionExecutionReady       = "ExecutionReady"
	ConditionLocalQueuesResolved  = "LocalQueuesResolved"
	ConditionClusterQueuesReady   = "ClusterQueuesReady"
	ConditionResourceFlavorsReady = "ResourceFlavorsReady"
	ConditionTopologiesReady      = "TopologiesReady"
	ConditionPriorityClassesReady = "PriorityClassesReady"

	ProfileSnapshotAPIVersion = "tau.azure.com/v1alpha1"
	ProfileSnapshotKind       = "TauWorkloadProfileSnapshot"
)

// ExecutionTarget selects whether a profile executes on the submitting cluster
// or through the explicitly gated MultiKueue Beta path.
type ExecutionTarget string

// WorkloadProfile is stable workload intent. It deliberately excludes observed
// capacity, quota, flavor selectors, and topology selectors.
// +kubebuilder:validation:XValidation:rule="self.workerCount == 1 || self.placement == 'multi-node-nccl'",message="workerCount greater than one requires placement=multi-node-nccl"
// +kubebuilder:validation:XValidation:rule="self.executionTarget != 'multiKueueBeta' || (has(self.applicability) && has(self.applicability.teams) && size(self.applicability.teams) > 0 && has(self.applicability.namespaces) && size(self.applicability.namespaces) > 0)",message="multiKueueBeta requires non-empty team and namespace applicability allowlists"
type WorkloadProfile struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name          string               `json:"name" yaml:"name"`
	Description   string               `json:"description,omitempty" yaml:"description,omitempty"`
	Applicability ProfileApplicability `json:"applicability,omitempty" yaml:"applicability,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUsPerWorker int32 `json:"gpusPerWorker" yaml:"gpusPerWorker"`
	// +kubebuilder:validation:Minimum=1
	WorkerCount int32 `json:"workerCount" yaml:"workerCount"`
	// +kubebuilder:validation:Enum=fixed;elastic
	Mode string `json:"mode" yaml:"mode"`
	// +kubebuilder:validation:Enum=independent;single-node-nvlink;multi-node-nccl
	Placement string `json:"placement" yaml:"placement"`
	// +kubebuilder:validation:MinLength=1
	DefaultLocalQueue string `json:"defaultLocalQueue" yaml:"defaultLocalQueue"`
	// +kubebuilder:default=singleCluster
	// +kubebuilder:validation:Enum=singleCluster;multiKueueBeta
	ExecutionTarget ExecutionTarget   `json:"executionTarget,omitempty" yaml:"executionTarget,omitempty"`
	Priorities      ProfilePriorities `json:"priorities" yaml:"priorities"`
}

type ProfileApplicability struct {
	// +listType=set
	Teams []string `json:"teams,omitempty" yaml:"teams,omitempty"`
	// +listType=set
	Lanes []string `json:"lanes,omitempty" yaml:"lanes,omitempty"`
	// +listType=set
	Namespaces []string `json:"namespaces,omitempty" yaml:"namespaces,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.disableDefaultPriorities) && self.disableDefaultPriorities) ? (!has(self.workloadPriorityClassName) && !has(self.podPriorityClassName)) : (has(self.workloadPriorityClassName) && has(self.podPriorityClassName))",message="priority class names are required unless default priorities are explicitly disabled"
type ProfilePriorities struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	WorkloadPriorityClassName string `json:"workloadPriorityClassName,omitempty" yaml:"workloadPriorityClassName,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PodPriorityClassName string `json:"podPriorityClassName,omitempty" yaml:"podPriorityClassName,omitempty"`
	// DisableDefaultPriorities explicitly permits both priority class references
	// to be absent.
	DisableDefaultPriorities bool `json:"disableDefaultPriorities,omitempty" yaml:"disableDefaultPriorities,omitempty"`
}

// ResolvedLocalQueue identifies an observed LocalQueue and its ClusterQueue.
type ResolvedLocalQueue struct {
	Namespace    string `json:"namespace" yaml:"namespace"`
	Name         string `json:"name" yaml:"name"`
	ClusterQueue string `json:"clusterQueue" yaml:"clusterQueue"`
}

// ResolvedWorkloadProfile combines normalized stable intent with only the
// observed resource identities needed by renderers and diagnostics.
type ResolvedWorkloadProfile struct {
	WorkloadProfile `json:",inline" yaml:",inline"`
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	LocalQueues []ResolvedLocalQueue `json:"localQueues,omitempty" yaml:"localQueues,omitempty"`
	// +listType=set
	ClusterQueues []string `json:"clusterQueues,omitempty" yaml:"clusterQueues,omitempty"`
	// +listType=set
	ResourceFlavors []string `json:"resourceFlavors,omitempty" yaml:"resourceFlavors,omitempty"`
	// +listType=set
	Topologies []string `json:"topologies,omitempty" yaml:"topologies,omitempty"`
	// +listType=set
	WorkloadPriorityClasses []string `json:"workloadPriorityClasses,omitempty" yaml:"workloadPriorityClasses,omitempty"`
	// +listType=set
	PodPriorityClasses []string `json:"podPriorityClasses,omitempty" yaml:"podPriorityClasses,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// ProfileSetStatus is the ready-or-drifted resolved state for a workload
// profile declaration set.
type ProfileSetStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	Observed           int32  `json:"observed,omitempty" yaml:"observed,omitempty"`
	Ready              int32  `json:"ready,omitempty" yaml:"ready,omitempty"`
	Drifted            int32  `json:"drifted,omitempty" yaml:"drifted,omitempty"`
	ProfileSetHash     string `json:"profileSetHash,omitempty" yaml:"profileSetHash,omitempty"`
	// +listType=map
	// +listMapKey=name
	Profiles []ResolvedWorkloadProfile `json:"profiles,omitempty" yaml:"profiles,omitempty"`
}

// ProfileSetSnapshot is the sole versioned offline envelope for a ready
// resolved profile set.
type ProfileSetSnapshot struct {
	APIVersion           string                    `json:"apiVersion" yaml:"apiVersion"`
	Kind                 string                    `json:"kind" yaml:"kind"`
	TauClusterGeneration int64                     `json:"tauClusterGeneration" yaml:"tauClusterGeneration"`
	ProfileSetHash       string                    `json:"profileSetHash" yaml:"profileSetHash"`
	Profiles             []ResolvedWorkloadProfile `json:"profiles" yaml:"profiles"`
}

// NormalizeWorkloadProfile returns the canonical representation used by
// validation, hashing, reconciliation, and consumers.
func NormalizeWorkloadProfile(in WorkloadProfile) WorkloadProfile {
	out := in
	out.Name = normalizeName(in.Name)
	out.Description = strings.TrimSpace(in.Description)
	out.Applicability.Teams = normalizeList(in.Applicability.Teams, normalizeLabel)
	out.Applicability.Lanes = normalizeList(in.Applicability.Lanes, normalizeLabel)
	out.Applicability.Namespaces = normalizeList(in.Applicability.Namespaces, normalizeName)
	out.Mode = normalizeLabel(in.Mode)
	out.Placement = normalizeLabel(in.Placement)
	out.DefaultLocalQueue = normalizeName(in.DefaultLocalQueue)
	switch strings.ToLower(strings.TrimSpace(string(in.ExecutionTarget))) {
	case "", strings.ToLower(string(ExecutionTargetSingleCluster)):
		out.ExecutionTarget = ExecutionTargetSingleCluster
	case strings.ToLower(string(ExecutionTargetMultiKueueBeta)):
		out.ExecutionTarget = ExecutionTargetMultiKueueBeta
	default:
		out.ExecutionTarget = ExecutionTarget(strings.TrimSpace(string(in.ExecutionTarget)))
	}
	out.Priorities.WorkloadPriorityClassName = normalizeName(in.Priorities.WorkloadPriorityClassName)
	out.Priorities.PodPriorityClassName = normalizeName(in.Priorities.PodPriorityClassName)
	return out
}

// ValidateWorkloadProfile validates a normalized workload profile. Callers that
// accept external input should use NormalizeAndValidateWorkloadProfile.
func ValidateWorkloadProfile(p WorkloadProfile) error {
	if !reflect.DeepEqual(p, NormalizeWorkloadProfile(p)) {
		return errors.New("workload profile must be normalized before validation")
	}
	if err := validateDNSName("name", p.Name); err != nil {
		return err
	}
	for field, values := range map[string][]string{
		"applicability.teams":      p.Applicability.Teams,
		"applicability.lanes":      p.Applicability.Lanes,
		"applicability.namespaces": p.Applicability.Namespaces,
	} {
		for i, value := range values {
			if value == "" {
				return fmt.Errorf("%s values must not be empty", field)
			}
			if i > 0 && value == values[i-1] {
				return fmt.Errorf("%s contains duplicate value %q", field, value)
			}
			if field == "applicability.namespaces" {
				if err := validateDNSName(field, value); err != nil {
					return err
				}
				continue
			}
			if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
				return fmt.Errorf("%s value %q is invalid: %s", field, value, strings.Join(errs, ", "))
			}
		}
	}
	if p.GPUsPerWorker < 0 {
		return fmt.Errorf("gpusPerWorker must be non-negative, got %d", p.GPUsPerWorker)
	}
	if p.WorkerCount < 1 {
		return fmt.Errorf("workerCount must be positive, got %d", p.WorkerCount)
	}
	if p.Mode != ModeFixed && p.Mode != ModeElastic {
		return fmt.Errorf("mode must be %q or %q, got %q", ModeFixed, ModeElastic, p.Mode)
	}
	switch p.Placement {
	case PlacementIndependent, PlacementSingleNodeNVLink, PlacementMultiNodeNCCL:
	default:
		return fmt.Errorf("placement must be %q, %q, or %q, got %q", PlacementIndependent, PlacementSingleNodeNVLink, PlacementMultiNodeNCCL, p.Placement)
	}
	if p.WorkerCount > 1 && p.Placement != PlacementMultiNodeNCCL {
		return fmt.Errorf("workerCount > 1 (got %d) requires placement=%s", p.WorkerCount, PlacementMultiNodeNCCL)
	}
	if err := validateDNSName("defaultLocalQueue", p.DefaultLocalQueue); err != nil {
		return err
	}
	switch p.ExecutionTarget {
	case ExecutionTargetSingleCluster:
	case ExecutionTargetMultiKueueBeta:
		if len(p.Applicability.Teams) == 0 {
			return errors.New("multiKueueBeta executionTarget requires a non-empty applicability.teams allowlist")
		}
		if len(p.Applicability.Namespaces) == 0 {
			return errors.New("multiKueueBeta executionTarget requires a non-empty applicability.namespaces allowlist")
		}
	default:
		return fmt.Errorf(
			"executionTarget must be %q or %q, got %q",
			ExecutionTargetSingleCluster,
			ExecutionTargetMultiKueueBeta,
			p.ExecutionTarget,
		)
	}
	priorities := p.Priorities
	if priorities.DisableDefaultPriorities {
		if priorities.WorkloadPriorityClassName != "" || priorities.PodPriorityClassName != "" {
			return errors.New("disableDefaultPriorities cannot be combined with priority class names")
		}
		return nil
	}
	if priorities.WorkloadPriorityClassName == "" || priorities.PodPriorityClassName == "" {
		return errors.New("workload and pod priority class names are required unless disableDefaultPriorities is true")
	}
	if err := validateDNSName("priorities.workloadPriorityClassName", priorities.WorkloadPriorityClassName); err != nil {
		return err
	}
	return validateDNSName("priorities.podPriorityClassName", priorities.PodPriorityClassName)
}

func NormalizeAndValidateWorkloadProfile(in WorkloadProfile) (WorkloadProfile, error) {
	out := NormalizeWorkloadProfile(in)
	if err := ValidateWorkloadProfile(out); err != nil {
		return WorkloadProfile{}, err
	}
	return out, nil
}

// NormalizeResolvedWorkloadProfile canonicalizes intent and all resolved
// identities. Conditions are copied but intentionally do not affect hashing.
func NormalizeResolvedWorkloadProfile(in ResolvedWorkloadProfile) (ResolvedWorkloadProfile, error) {
	intent, err := NormalizeAndValidateWorkloadProfile(in.WorkloadProfile)
	if err != nil {
		return ResolvedWorkloadProfile{}, err
	}
	out := in
	out.WorkloadProfile = intent
	out.LocalQueues, err = normalizeLocalQueues(in.LocalQueues)
	if err != nil {
		return ResolvedWorkloadProfile{}, err
	}
	out.ClusterQueues = normalizeSet(in.ClusterQueues, normalizeName)
	out.ResourceFlavors = normalizeSet(in.ResourceFlavors, normalizeName)
	out.Topologies = normalizeSet(in.Topologies, normalizeName)
	out.WorkloadPriorityClasses = normalizeSet(in.WorkloadPriorityClasses, normalizeName)
	out.PodPriorityClasses = normalizeSet(in.PodPriorityClasses, normalizeName)
	out.Conditions = append([]metav1.Condition(nil), in.Conditions...)
	for _, queue := range out.LocalQueues {
		if err := validateDNSName("localQueues.namespace", queue.Namespace); err != nil {
			return ResolvedWorkloadProfile{}, err
		}
		if err := validateDNSName("localQueues.name", queue.Name); err != nil {
			return ResolvedWorkloadProfile{}, err
		}
		if err := validateDNSName("localQueues.clusterQueue", queue.ClusterQueue); err != nil {
			return ResolvedWorkloadProfile{}, err
		}
	}
	for field, values := range map[string][]string{
		"clusterQueues":           out.ClusterQueues,
		"resourceFlavors":         out.ResourceFlavors,
		"topologies":              out.Topologies,
		"workloadPriorityClasses": out.WorkloadPriorityClasses,
		"podPriorityClasses":      out.PodPriorityClasses,
	} {
		for _, value := range values {
			if err := validateDNSName(field, value); err != nil {
				return ResolvedWorkloadProfile{}, err
			}
		}
	}
	return out, nil
}

// ProfileSetHash returns a SHA-256 digest over sorted normalized intent and
// resolved identities. Conditions and all live operational data are excluded.
func ProfileSetHash(profiles []ResolvedWorkloadProfile) (string, error) {
	canonical := make([]resolvedProfileHashInput, 0, len(profiles))
	seen := make(map[string]struct{}, len(profiles))
	for _, raw := range profiles {
		p, err := NormalizeResolvedWorkloadProfile(raw)
		if err != nil {
			return "", fmt.Errorf("profile %q: %w", raw.Name, err)
		}
		if _, ok := seen[p.Name]; ok {
			return "", fmt.Errorf("duplicate workload profile %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		canonical = append(canonical, resolvedProfileHashInput{
			WorkloadProfile:         p.WorkloadProfile,
			LocalQueues:             p.LocalQueues,
			ClusterQueues:           p.ClusterQueues,
			ResourceFlavors:         p.ResourceFlavors,
			Topologies:              p.Topologies,
			WorkloadPriorityClasses: p.WorkloadPriorityClasses,
			PodPriorityClasses:      p.PodPriorityClasses,
		})
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Name < canonical[j].Name })
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal canonical profile set: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type resolvedProfileHashInput struct {
	WorkloadProfile
	LocalQueues             []ResolvedLocalQueue `json:"localQueues,omitempty"`
	ClusterQueues           []string             `json:"clusterQueues,omitempty"`
	ResourceFlavors         []string             `json:"resourceFlavors,omitempty"`
	Topologies              []string             `json:"topologies,omitempty"`
	WorkloadPriorityClasses []string             `json:"workloadPriorityClasses,omitempty"`
	PodPriorityClasses      []string             `json:"podPriorityClasses,omitempty"`
}

// NewProfileSetSnapshot creates a validated, canonically ordered snapshot.
func NewProfileSetSnapshot(generation int64, profiles []ResolvedWorkloadProfile) (ProfileSetSnapshot, error) {
	if generation < 1 {
		return ProfileSetSnapshot{}, errors.New("tauClusterGeneration must be positive")
	}
	normalized, err := normalizeResolvedProfiles(profiles)
	if err != nil {
		return ProfileSetSnapshot{}, err
	}
	if err := validateSnapshotReadiness(generation, normalized); err != nil {
		return ProfileSetSnapshot{}, err
	}
	hash, err := ProfileSetHash(normalized)
	if err != nil {
		return ProfileSetSnapshot{}, err
	}
	return ProfileSetSnapshot{
		APIVersion:           ProfileSnapshotAPIVersion,
		Kind:                 ProfileSnapshotKind,
		TauClusterGeneration: generation,
		ProfileSetHash:       hash,
		Profiles:             normalized,
	}, nil
}

// DecodeProfileSetSnapshot decodes JSON or YAML and rejects unknown envelope
// versions, kinds, invalid profiles, and mismatched hashes.
func DecodeProfileSetSnapshot(data []byte) (ProfileSetSnapshot, error) {
	var snapshot ProfileSetSnapshot
	if err := yaml.Unmarshal(data, &snapshot); err != nil {
		return ProfileSetSnapshot{}, fmt.Errorf("decode workload profile snapshot: %w", err)
	}
	if snapshot.APIVersion != ProfileSnapshotAPIVersion {
		return ProfileSetSnapshot{}, fmt.Errorf("unsupported workload profile snapshot apiVersion %q", snapshot.APIVersion)
	}
	if snapshot.Kind != ProfileSnapshotKind {
		return ProfileSetSnapshot{}, fmt.Errorf("unsupported workload profile snapshot kind %q", snapshot.Kind)
	}
	if snapshot.TauClusterGeneration < 1 {
		return ProfileSetSnapshot{}, errors.New("tauClusterGeneration must be positive")
	}
	normalized, err := normalizeResolvedProfiles(snapshot.Profiles)
	if err != nil {
		return ProfileSetSnapshot{}, err
	}
	if err := validateSnapshotReadiness(snapshot.TauClusterGeneration, normalized); err != nil {
		return ProfileSetSnapshot{}, err
	}
	hash, err := ProfileSetHash(normalized)
	if err != nil {
		return ProfileSetSnapshot{}, err
	}
	if snapshot.ProfileSetHash != hash {
		return ProfileSetSnapshot{}, fmt.Errorf("profileSetHash mismatch: got %q, want %q", snapshot.ProfileSetHash, hash)
	}
	snapshot.Profiles = normalized
	return snapshot, nil
}

// RenderProfile converts a resolved shared profile to the existing renderer
// contract for a namespace and selected applicability values.
func (p ResolvedWorkloadProfile) RenderProfile(namespace, team, lane string) (Profile, error) {
	normalized, err := NormalizeResolvedWorkloadProfile(p)
	if err != nil {
		return Profile{}, err
	}
	namespace = normalizeName(namespace)
	team = normalizeLabel(team)
	lane = normalizeLabel(lane)
	if !containsOrGlobal(normalized.Applicability.Namespaces, namespace) {
		return Profile{}, fmt.Errorf("profile %q does not apply to namespace %q", normalized.Name, namespace)
	}
	if !containsOrGlobal(normalized.Applicability.Teams, team) {
		return Profile{}, fmt.Errorf("profile %q does not apply to team %q", normalized.Name, team)
	}
	if !containsOrGlobal(normalized.Applicability.Lanes, lane) {
		return Profile{}, fmt.Errorf("profile %q does not apply to lane %q", normalized.Name, lane)
	}
	queue := normalized.DefaultLocalQueue
	for _, ref := range normalized.LocalQueues {
		if ref.Namespace == namespace {
			queue = ref.Name
			break
		}
	}
	return Profile{
		Name:            normalized.Name,
		Lane:            lane,
		Queue:           queue,
		ExecutionTarget: normalized.ExecutionTarget,
		Topology: Topology{
			Team:                      team,
			Mode:                      normalized.Mode,
			Placement:                 normalized.Placement,
			PodPriorityClassName:      normalized.Priorities.PodPriorityClassName,
			WorkloadPriorityClassName: normalized.Priorities.WorkloadPriorityClassName,
			DisableDefaultPriorities:  normalized.Priorities.DisableDefaultPriorities,
		},
		Resources: Resources{GPU: GPUContract{Count: int(normalized.GPUsPerWorker)}},
	}, nil
}

// ClusterQueueFor returns the observed ClusterQueue binding for the selected
// namespace and LocalQueue.
func (p ResolvedWorkloadProfile) ClusterQueueFor(namespace, localQueue string) (string, error) {
	normalized, err := NormalizeResolvedWorkloadProfile(p)
	if err != nil {
		return "", err
	}
	namespace = normalizeName(namespace)
	localQueue = normalizeName(localQueue)
	for _, ref := range normalized.LocalQueues {
		if ref.Namespace == namespace && ref.Name == localQueue {
			return ref.ClusterQueue, nil
		}
	}
	if len(normalized.ClusterQueues) == 1 {
		return normalized.ClusterQueues[0], nil
	}
	return "", fmt.Errorf(
		"profile %q has no observed LocalQueue binding for %s/%s",
		normalized.Name,
		namespace,
		localQueue,
	)
}

func normalizeResolvedProfiles(profiles []ResolvedWorkloadProfile) ([]ResolvedWorkloadProfile, error) {
	out := make([]ResolvedWorkloadProfile, 0, len(profiles))
	seen := make(map[string]struct{}, len(profiles))
	for _, raw := range profiles {
		p, err := NormalizeResolvedWorkloadProfile(raw)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", raw.Name, err)
		}
		if _, ok := seen[p.Name]; ok {
			return nil, fmt.Errorf("duplicate workload profile %q", p.Name)
		}
		sort.SliceStable(p.Conditions, func(i, j int) bool {
			return p.Conditions[i].Type < p.Conditions[j].Type
		})
		seen[p.Name] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func normalizeLocalQueues(in []ResolvedLocalQueue) ([]ResolvedLocalQueue, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ResolvedLocalQueue, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		item := ResolvedLocalQueue{
			Namespace:    normalizeName(raw.Namespace),
			Name:         normalizeName(raw.Name),
			ClusterQueue: normalizeName(raw.ClusterQueue),
		}
		key := item.Namespace + "\x00" + item.Name
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate resolved LocalQueue %s/%s", item.Namespace, item.Name)
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out, nil
}

func validateSnapshotReadiness(generation int64, profiles []ResolvedWorkloadProfile) error {
	for _, p := range profiles {
		ready := false
		for _, condition := range p.Conditions {
			if condition.Type != ConditionReady {
				continue
			}
			if condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != generation {
				return fmt.Errorf("profile %q Ready condition is not true at TauCluster generation %d", p.Name, generation)
			}
			ready = true
			break
		}
		if !ready {
			return fmt.Errorf("profile %q has no Ready condition", p.Name)
		}
	}
	return nil
}

func normalizeSet(in []string, normalize func(string) string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		value := normalize(raw)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeList(in []string, normalize func(string) string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, raw := range in {
		out[i] = normalize(raw)
	}
	sort.Strings(out)
	return out
}

func normalizeName(value string) string {
	return strings.Trim(strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-"), " ", "-"), ".")
}

func normalizeLabel(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-"), " ", "-")
}

func validateDNSName(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if errs := validation.IsDNS1123Subdomain(value); len(errs) != 0 {
		return fmt.Errorf("%s %q is invalid: %s", field, value, strings.Join(errs, ", "))
	}
	return nil
}

func containsOrGlobal(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

// DeepCopyInto allows API packages to embed shared types without shallow-copying
// slices during generated Kubernetes object deep copies.
func (in *WorkloadProfile) DeepCopyInto(out *WorkloadProfile) {
	*out = *in
	out.Applicability.Teams = append([]string(nil), in.Applicability.Teams...)
	out.Applicability.Lanes = append([]string(nil), in.Applicability.Lanes...)
	out.Applicability.Namespaces = append([]string(nil), in.Applicability.Namespaces...)
}

func (in *ResolvedWorkloadProfile) DeepCopyInto(out *ResolvedWorkloadProfile) {
	*out = *in
	in.WorkloadProfile.DeepCopyInto(&out.WorkloadProfile)
	out.LocalQueues = append([]ResolvedLocalQueue(nil), in.LocalQueues...)
	out.ClusterQueues = append([]string(nil), in.ClusterQueues...)
	out.ResourceFlavors = append([]string(nil), in.ResourceFlavors...)
	out.Topologies = append([]string(nil), in.Topologies...)
	out.WorkloadPriorityClasses = append([]string(nil), in.WorkloadPriorityClasses...)
	out.PodPriorityClasses = append([]string(nil), in.PodPriorityClasses...)
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ProfileSetStatus) DeepCopyInto(out *ProfileSetStatus) {
	*out = *in
	if in.Profiles != nil {
		out.Profiles = make([]ResolvedWorkloadProfile, len(in.Profiles))
		for i := range in.Profiles {
			in.Profiles[i].DeepCopyInto(&out.Profiles[i])
		}
	}
}
