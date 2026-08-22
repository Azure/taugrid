// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	TauClusterName                               = "cluster"
	ConditionWorkloadProfilesReady               = "WorkloadProfilesReady"
	ProfileSourceCluster           ProfileSource = "cluster"
	ProfileSourceSnapshot          ProfileSource = "snapshot"
)

var TauClusterGVR = schema.GroupVersionResource{
	Group: "tau.azure.com", Version: "v1alpha1", Resource: "clusters",
}

// ProfileSource identifies the explicitly selected profile source.
type ProfileSource string

// ProfileSet is a validated ready profile-set revision.
type ProfileSet struct {
	Generation     int64
	ProfileSetHash string
	Profiles       []ResolvedWorkloadProfile
	Source         ProfileSource
}

// SelectionRequest identifies one profile and the caller scope it must
// authorize.
type SelectionRequest struct {
	Name      string
	Namespace string
	Team      string
	Lane      string
}

// Selection is the common result returned by cluster and snapshot providers.
type Selection struct {
	Generation     int64
	ProfileSetHash string
	Profile        ResolvedWorkloadProfile
	Source         ProfileSource
}

// Provider reads exactly one explicitly configured source. It never falls back
// to or auto-detects another source.
type Provider struct {
	client   dynamic.Interface
	snapshot *ProfileSetSnapshot
	source   ProfileSource
}

// NewClusterProvider creates a provider for the singleton TauCluster.
func NewClusterProvider(client dynamic.Interface) *Provider {
	return &Provider{client: client, source: ProfileSourceCluster}
}

// NewSnapshotProvider validates an already-decoded local snapshot.
func NewSnapshotProvider(snapshot ProfileSetSnapshot) (*Provider, error) {
	set, err := profileSetFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	validated := ProfileSetSnapshot{
		APIVersion:           ProfileSnapshotAPIVersion,
		Kind:                 ProfileSnapshotKind,
		TauClusterGeneration: set.Generation,
		ProfileSetHash:       set.ProfileSetHash,
		Profiles:             set.Profiles,
	}
	return &Provider{snapshot: &validated, source: ProfileSourceSnapshot}, nil
}

// DecodeSnapshotProvider loads a local snapshot through the canonical decoder.
func DecodeSnapshotProvider(data []byte) (*Provider, error) {
	snapshot, err := DecodeProfileSetSnapshot(data)
	if err != nil {
		return nil, err
	}
	return NewSnapshotProvider(snapshot)
}

// ProfileSet returns the validated ready set from this provider's sole source.
func (p *Provider) ProfileSet(ctx context.Context) (ProfileSet, error) {
	switch p.source {
	case ProfileSourceCluster:
		if p.client == nil {
			return ProfileSet{}, errors.New("workload profile cluster provider has no Kubernetes client")
		}
		object, err := p.client.Resource(TauClusterGVR).Get(ctx, TauClusterName, metav1.GetOptions{})
		if err != nil {
			return ProfileSet{}, classifyTauClusterReadError(err)
		}
		return decodeTauClusterProfileSet(object.Object)
	case ProfileSourceSnapshot:
		if p.snapshot == nil {
			return ProfileSet{}, errors.New("workload profile snapshot provider has no snapshot")
		}
		return profileSetFromSnapshot(*p.snapshot)
	default:
		return ProfileSet{}, fmt.Errorf("unsupported workload profile source %q", p.source)
	}
}

// Select returns one ready, applicable profile from the validated set.
func (p *Provider) Select(ctx context.Context, request SelectionRequest) (Selection, error) {
	set, err := p.ProfileSet(ctx)
	if err != nil {
		return Selection{}, err
	}
	return set.Select(request)
}

// Select returns one ready, applicable profile from an already validated set.
func (set ProfileSet) Select(request SelectionRequest) (Selection, error) {
	name := normalizeName(request.Name)
	if name == "" {
		return selectUniqueProfile(set, request)
	}
	for _, candidate := range set.Profiles {
		if candidate.Name != name {
			continue
		}
		if err := requireProfileReady(candidate, set.Generation); err != nil {
			return Selection{}, err
		}
		if err := authorizeProfile(candidate, request); err != nil {
			return Selection{}, err
		}
		return Selection{
			Generation:     set.Generation,
			ProfileSetHash: set.ProfileSetHash,
			Profile:        candidate,
			Source:         set.Source,
		}, nil
	}
	names := make([]string, 0, len(set.Profiles))
	for _, candidate := range set.Profiles {
		names = append(names, candidate.Name)
	}
	return Selection{}, fmt.Errorf(
		"workload profile %q is unavailable; available profiles: %s",
		name,
		formatAvailableProfiles(names),
	)
}

// ValidateReady reports whether a resolved profile is current and Ready for the
// TauCluster generation that published it.
func ValidateReady(resolved ResolvedWorkloadProfile, generation int64) error {
	return requireProfileReady(resolved, generation)
}

// ValidateApplicability reports whether a resolved profile authorizes the
// caller's namespace, team, and lane.
func ValidateApplicability(resolved ResolvedWorkloadProfile, request SelectionRequest) error {
	return authorizeProfile(resolved, request)
}

func selectUniqueProfile(set ProfileSet, request SelectionRequest) (Selection, error) {
	var matches []ResolvedWorkloadProfile
	for _, candidate := range set.Profiles {
		if err := requireProfileReady(candidate, set.Generation); err != nil {
			continue
		}
		if err := authorizeProfile(candidate, request); err != nil {
			continue
		}
		matches = append(matches, candidate)
	}
	scope := fmt.Sprintf(
		"namespace=%q, team=%q, lane=%q",
		normalizeName(request.Namespace),
		normalizeLabel(request.Team),
		normalizeLabel(request.Lane),
	)
	switch len(matches) {
	case 0:
		return Selection{}, fmt.Errorf(
			"no ready workload profile matches %s; set policy.profile explicitly",
			scope,
		)
	case 1:
		return Selection{
			Generation:     set.Generation,
			ProfileSetHash: set.ProfileSetHash,
			Profile:        matches[0],
			Source:         set.Source,
		}, nil
	default:
		names := make([]string, 0, len(matches))
		for _, candidate := range matches {
			names = append(names, candidate.Name)
		}
		return Selection{}, fmt.Errorf(
			"multiple ready workload profiles match %s: %s; set policy.profile explicitly",
			scope,
			formatAvailableProfiles(names),
		)
	}
}

type tauClusterProfileTransport struct {
	Metadata struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Status struct {
		WorkloadProfiles ProfileSetStatus   `json:"workloadProfiles"`
		Conditions       []metav1.Condition `json:"conditions"`
	} `json:"status"`
}

func decodeTauClusterProfileSet(object map[string]any) (ProfileSet, error) {
	var cluster tauClusterProfileTransport
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &cluster); err != nil {
		return ProfileSet{}, fmt.Errorf("decode TauCluster workload profile status: %w", err)
	}
	if cluster.Metadata.Name != TauClusterName {
		return ProfileSet{}, fmt.Errorf("TauCluster must be named %q, got %q", TauClusterName, cluster.Metadata.Name)
	}
	generation := cluster.Metadata.Generation
	status := cluster.Status.WorkloadProfiles
	if status.ObservedGeneration != generation {
		return ProfileSet{}, fmt.Errorf(
			"TauCluster %q workload profiles are stale: observedGeneration %d does not match metadata.generation %d",
			TauClusterName, status.ObservedGeneration, generation,
		)
	}
	if err := requireCondition(
		cluster.Status.Conditions,
		ConditionWorkloadProfilesReady,
		generation,
		fmt.Sprintf("TauCluster %q workload profile set", TauClusterName),
		false,
	); err != nil {
		return ProfileSet{}, err
	}
	if strings.TrimSpace(status.ProfileSetHash) == "" {
		return ProfileSet{}, fmt.Errorf("TauCluster %q workload profile set has an empty profileSetHash", TauClusterName)
	}
	profiles, err := normalizeResolvedProfiles(status.Profiles)
	if err != nil {
		return ProfileSet{}, fmt.Errorf("decode TauCluster %q workload profiles: %w", TauClusterName, err)
	}
	hash, err := ProfileSetHash(profiles)
	if err != nil {
		return ProfileSet{}, fmt.Errorf("hash TauCluster %q workload profiles: %w", TauClusterName, err)
	}
	if hash != status.ProfileSetHash {
		return ProfileSet{}, fmt.Errorf(
			"TauCluster %q workload profileSetHash mismatch: status has %q, calculated %q",
			TauClusterName, status.ProfileSetHash, hash,
		)
	}
	return ProfileSet{
		Generation:     generation,
		ProfileSetHash: hash,
		Profiles:       profiles,
		Source:         ProfileSourceCluster,
	}, nil
}

func profileSetFromSnapshot(snapshot ProfileSetSnapshot) (ProfileSet, error) {
	if snapshot.APIVersion != ProfileSnapshotAPIVersion {
		return ProfileSet{}, fmt.Errorf("unsupported workload profile snapshot apiVersion %q", snapshot.APIVersion)
	}
	if snapshot.Kind != ProfileSnapshotKind {
		return ProfileSet{}, fmt.Errorf("unsupported workload profile snapshot kind %q", snapshot.Kind)
	}
	if snapshot.TauClusterGeneration < 1 {
		return ProfileSet{}, errors.New("tauClusterGeneration must be positive")
	}
	profiles, err := normalizeResolvedProfiles(snapshot.Profiles)
	if err != nil {
		return ProfileSet{}, err
	}
	if err := validateSnapshotReadiness(snapshot.TauClusterGeneration, profiles); err != nil {
		return ProfileSet{}, err
	}
	hash, err := ProfileSetHash(profiles)
	if err != nil {
		return ProfileSet{}, err
	}
	if snapshot.ProfileSetHash != hash {
		return ProfileSet{}, fmt.Errorf("profileSetHash mismatch: got %q, want %q", snapshot.ProfileSetHash, hash)
	}
	return ProfileSet{
		Generation:     snapshot.TauClusterGeneration,
		ProfileSetHash: hash,
		Profiles:       profiles,
		Source:         ProfileSourceSnapshot,
	}, nil
}

func classifyTauClusterReadError(err error) error {
	switch {
	case meta.IsNoMatchError(err), isMissingTauClusterResourceEndpoint(err):
		return fmt.Errorf("TauCluster CRD %s is not installed: %w", TauClusterGVR.GroupResource(), err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("access to TauCluster %q is forbidden; grant get on %s: %w", TauClusterName, TauClusterGVR.GroupResource(), err)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("TauCluster singleton %q was not found: %w", TauClusterName, err)
	default:
		return fmt.Errorf("read TauCluster singleton %q: %w", TauClusterName, err)
	}
}

func isMissingTauClusterResourceEndpoint(err error) bool {
	if !apierrors.IsNotFound(err) {
		return false
	}
	var status interface{ Status() metav1.Status }
	if !errors.As(err, &status) {
		return strings.Contains(strings.ToLower(err.Error()), "could not find the requested resource")
	}
	details := status.Status().Details
	return details == nil || details.Name == ""
}

func requireProfileReady(profile ResolvedWorkloadProfile, generation int64) error {
	return requireCondition(
		profile.Conditions,
		ConditionReady,
		generation,
		fmt.Sprintf("workload profile %q", profile.Name),
		true,
	)
}

func requireCondition(
	conditions []metav1.Condition,
	conditionType string,
	generation int64,
	subject string,
	requireTrue bool,
) error {
	for _, condition := range conditions {
		if condition.Type != conditionType {
			continue
		}
		if condition.ObservedGeneration != generation {
			return fmt.Errorf(
				"%s condition %s is stale: observedGeneration %d does not match generation %d",
				subject, conditionType, condition.ObservedGeneration, generation,
			)
		}
		if requireTrue && condition.Status != metav1.ConditionTrue {
			return fmt.Errorf(
				"%s condition %s is %s at generation %d: %s: %s",
				subject, conditionType, condition.Status, generation, condition.Reason, condition.Message,
			)
		}
		return nil
	}
	return fmt.Errorf("%s is missing condition %s at generation %d", subject, conditionType, generation)
}

func authorizeProfile(profile ResolvedWorkloadProfile, request SelectionRequest) error {
	checks := []struct {
		field  string
		value  string
		values []string
	}{
		{field: "namespace", value: normalizeName(request.Namespace), values: profile.Applicability.Namespaces},
		{field: "team", value: normalizeLabel(request.Team), values: profile.Applicability.Teams},
		{field: "lane", value: normalizeLabel(request.Lane), values: profile.Applicability.Lanes},
	}
	for _, check := range checks {
		if containsOrGlobal(check.values, check.value) {
			continue
		}
		return fmt.Errorf(
			"workload profile %q does not authorize %s %q; allowed %ss: %s",
			profile.Name,
			check.field,
			check.value,
			check.field,
			strings.Join(check.values, ", "),
		)
	}
	return nil
}

func formatAvailableProfiles(names []string) string {
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
