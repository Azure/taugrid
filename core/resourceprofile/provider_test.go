// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func TestClusterProviderSelectsReadyApplicableProfile(t *testing.T) {
	profile := readyProviderProfile("research-1gpu", 7)
	profile.ExecutionTarget = ExecutionTargetMultiKueue
	provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{profile}, nil))

	got, err := provider.Select(context.Background(), SelectionRequest{
		Name: "research-1gpu", Namespace: "alpha", Team: "research", Lane: "training",
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Generation != 7 || got.ProfileSetHash == "" || got.Source != ProfileSourceCluster {
		t.Fatalf("selection revision = %#v", got)
	}
	if got.Profile.ExecutionTarget != ExecutionTargetMultiKueue {
		t.Fatalf("execution target = %q, want %q", got.Profile.ExecutionTarget, ExecutionTargetMultiKueue)
	}
}

func TestClusterProviderReadFailures(t *testing.T) {
	t.Run("missing singleton", func(t *testing.T) {
		provider := NewClusterProvider(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
		_, err := provider.ProfileSet(context.Background())
		if err == nil || !strings.Contains(err.Error(), `singleton "cluster" was not found`) {
			t.Fatalf("ProfileSet() error = %v", err)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
		client.PrependReactor("get", "clusters", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: TauClusterGVR.Group, Resource: TauClusterGVR.Resource},
				TauClusterName,
				errors.New("denied"),
			)
		})
		provider := NewClusterProvider(client)
		_, err := provider.ProfileSet(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is forbidden; grant get") {
			t.Fatalf("ProfileSet() error = %v", err)
		}
	})

	t.Run("missing CRD discovery", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
		client.PrependReactor("get", "clusters", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, &meta.NoResourceMatchError{PartialResource: TauClusterGVR}
		})
		provider := NewClusterProvider(client)
		_, err := provider.ProfileSet(context.Background())
		if err == nil || !strings.Contains(err.Error(), "CRD clusters.tau.azure.com is not installed") {
			t.Fatalf("ProfileSet() error = %v", err)
		}
	})

	t.Run("missing CRD endpoint", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
		client.PrependReactor("get", "clusters", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    404,
				Reason:  metav1.StatusReasonNotFound,
				Message: "the server could not find the requested resource",
			}}
		})
		provider := NewClusterProvider(client)
		_, err := provider.ProfileSet(context.Background())
		if err == nil || !strings.Contains(err.Error(), "CRD clusters.tau.azure.com is not installed") {
			t.Fatalf("ProfileSet() error = %v", err)
		}
	})
}

func TestClusterProviderRejectsUnreadySets(t *testing.T) {
	ready := readyProviderProfile("research-1gpu", 7)
	tests := []struct {
		name       string
		generation int64
		profiles   []ResolvedWorkloadProfile
		mutate     func(*unstructured.Unstructured)
		want       string
	}{
		{
			name: "stale set", generation: 7, profiles: []ResolvedWorkloadProfile{ready},
			mutate: func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, int64(6), "status", "workloadProfiles", "observedGeneration")
			},
			want: "workload profiles are stale",
		},
		{
			name: "missing top readiness", generation: 7, profiles: []ResolvedWorkloadProfile{ready},
			mutate: func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(object.Object, nil, "status", "conditions")
			},
			want: "is missing condition WorkloadProfilesReady",
		},
		{
			name: "hash mismatch", generation: 7, profiles: []ResolvedWorkloadProfile{ready},
			mutate: func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "wrong", "status", "workloadProfiles", "profileSetHash")
			},
			want: "profileSetHash mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := clusterClient(t, tt.generation, tt.profiles, tt.mutate)
			_, err := NewClusterProvider(client).ProfileSet(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ProfileSet() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestClusterProviderAllowsReadyProfileWhenAggregateReadinessIsFalse(t *testing.T) {
	ordinary := readyProviderProfile("ordinary", 7)
	multiKueue := readyProviderProfile("multi-kueue", 7)
	multiKueue.ExecutionTarget = ExecutionTargetMultiKueue
	multiKueue.Conditions[0].Status = metav1.ConditionFalse
	multiKueue.Conditions[0].Reason = "Unavailable"
	provider := NewClusterProvider(clusterClient(
		t,
		7,
		[]ResolvedWorkloadProfile{ordinary, multiKueue},
		func(object *unstructured.Unstructured) {
			conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
			conditions[0].(map[string]any)["status"] = string(metav1.ConditionFalse)
			_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
		},
	))

	if _, err := provider.Select(context.Background(), SelectionRequest{
		Name: "ordinary", Namespace: "alpha", Team: "research", Lane: "training",
	}); err != nil {
		t.Fatalf("ready ordinary profile was blocked by aggregate readiness: %v", err)
	}
	if _, err := provider.Select(context.Background(), SelectionRequest{
		Name: "multi-kueue", Namespace: "alpha", Team: "research", Lane: "training",
	}); err == nil || !strings.Contains(err.Error(), "condition Ready is False") {
		t.Fatalf("unready MultiKueue profile selection error = %v", err)
	}
}

func TestClusterProviderRejectsUnavailableAndUnreadyProfile(t *testing.T) {
	ready := readyProviderProfile("zeta", 7)
	other := readyProviderProfile("alpha", 7)
	provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{ready, other}, nil))
	_, err := provider.Select(context.Background(), SelectionRequest{Name: "missing"})
	if err == nil || !strings.Contains(err.Error(), "available profiles: alpha, zeta") {
		t.Fatalf("unavailable Select() error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*ResolvedWorkloadProfile)
		want   string
	}{
		{
			name: "stale",
			mutate: func(profile *ResolvedWorkloadProfile) {
				profile.Conditions[0].ObservedGeneration = 6
			},
			want: "condition Ready is stale",
		},
		{
			name: "false",
			mutate: func(profile *ResolvedWorkloadProfile) {
				profile.Conditions[0].Status = metav1.ConditionFalse
			},
			want: "condition Ready is False",
		},
		{
			name: "missing",
			mutate: func(profile *ResolvedWorkloadProfile) {
				profile.Conditions = nil
			},
			want: "is missing condition Ready",
		},
	} {
		t.Run(tt.name+" profile readiness", func(t *testing.T) {
			resolved := readyProviderProfile("research-1gpu", 7)
			tt.mutate(&resolved)
			provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{resolved}, nil))
			_, err := provider.Select(context.Background(), SelectionRequest{
				Name: "research-1gpu", Namespace: "alpha", Team: "research", Lane: "training",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Select() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestProviderEnforcesEveryApplicabilityDimension(t *testing.T) {
	resolved := readyProviderProfile("research-1gpu", 7)
	provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{resolved}, nil))
	tests := []struct {
		name      string
		request   SelectionRequest
		wantField string
	}{
		{"namespace", SelectionRequest{Name: resolved.Name, Namespace: "other", Team: "research", Lane: "training"}, "namespace"},
		{"team", SelectionRequest{Name: resolved.Name, Namespace: "alpha", Team: "other", Lane: "training"}, "team"},
		{"lane", SelectionRequest{Name: resolved.Name, Namespace: "alpha", Team: "research", Lane: "other"}, "lane"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := provider.Select(context.Background(), tt.request)
			if err == nil || !strings.Contains(err.Error(), "does not authorize "+tt.wantField) {
				t.Fatalf("Select() error = %v", err)
			}
		})
	}
}

func TestSnapshotProviderUsesSameSelectionShape(t *testing.T) {
	resolved := readyProviderProfile("research-1gpu", 7)
	snapshot, err := NewProfileSetSnapshot(7, []ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := DecodeSnapshotProvider(data)
	if err != nil {
		t.Fatalf("DecodeSnapshotProvider() error = %v", err)
	}
	got, err := provider.Select(context.Background(), SelectionRequest{
		Name: resolved.Name, Namespace: "alpha", Team: "research", Lane: "training",
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Source != ProfileSourceSnapshot || got.Generation != 7 || got.ProfileSetHash != snapshot.ProfileSetHash {
		t.Fatalf("snapshot selection = %#v", got)
	}
}

func TestProviderImplicitSelectionRequiresUniqueReadyApplicableMatch(t *testing.T) {
	normal := readyProviderProfile("normal", 7)
	multiKueue := readyProviderProfile("multi-kueue", 7)
	multiKueue.ExecutionTarget = ExecutionTargetMultiKueue

	t.Run("single and MultiKueue matches are ambiguous", func(t *testing.T) {
		provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{multiKueue, normal}, nil))
		_, err := provider.Select(context.Background(), SelectionRequest{
			Namespace: "alpha", Team: "research", Lane: "training",
		})
		if err == nil || !strings.Contains(err.Error(), "multiple ready workload profiles") ||
			!strings.Contains(err.Error(), "multi-kueue, normal") {
			t.Fatalf("Select() error = %v", err)
		}
	})

	t.Run("unique MultiKueue match is selected", func(t *testing.T) {
		provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{multiKueue}, nil))
		got, err := provider.Select(context.Background(), SelectionRequest{
			Namespace: "alpha", Team: "research", Lane: "training",
		})
		if err != nil {
			t.Fatalf("Select() error = %v", err)
		}
		if got.Profile.Name != multiKueue.Name || got.Profile.ExecutionTarget != ExecutionTargetMultiKueue {
			t.Fatalf("implicit selection = %#v", got.Profile)
		}
	})

	t.Run("no ready applicable match uses neutral error", func(t *testing.T) {
		unready := multiKueue
		unready.Conditions[0].Status = metav1.ConditionFalse
		provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{unready}, nil))
		_, err := provider.Select(context.Background(), SelectionRequest{
			Namespace: "alpha", Team: "research", Lane: "training",
		})
		if err == nil || !strings.Contains(err.Error(), "no ready workload profile matches") ||
			strings.Contains(strings.ToLower(err.Error()), "beta") {
			t.Fatalf("Select() error = %v", err)
		}
	})

	t.Run("ambiguous normal profiles fail", func(t *testing.T) {
		other := readyProviderProfile("other", 7)
		provider := NewClusterProvider(clusterClient(t, 7, []ResolvedWorkloadProfile{other, normal}, nil))
		_, err := provider.Select(context.Background(), SelectionRequest{
			Namespace: "alpha", Team: "research", Lane: "training",
		})
		if err == nil || !strings.Contains(err.Error(), "multiple ready workload profiles") ||
			!strings.Contains(err.Error(), "normal, other") {
			t.Fatalf("Select() error = %v", err)
		}
	})
}

func TestProfileSnapshotEncodingIsDeterministic(t *testing.T) {
	alpha := readyProviderProfile("alpha", 7)
	alpha.Conditions = append(alpha.Conditions, metav1.Condition{
		Type: "ExecutionReady", Status: metav1.ConditionTrue, ObservedGeneration: 7,
	})
	zeta := readyProviderProfile("zeta", 7)
	left, err := NewProfileSetSnapshot(7, []ResolvedWorkloadProfile{zeta, alpha})
	if err != nil {
		t.Fatal(err)
	}
	alpha.Conditions[0], alpha.Conditions[1] = alpha.Conditions[1], alpha.Conditions[0]
	right, err := NewProfileSetSnapshot(7, []ResolvedWorkloadProfile{alpha, zeta})
	if err != nil {
		t.Fatal(err)
	}
	leftYAML, err := yaml.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightYAML, err := yaml.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if string(leftYAML) != string(rightYAML) {
		t.Fatalf("snapshot YAML is not deterministic:\nleft:\n%s\nright:\n%s", leftYAML, rightYAML)
	}
}

func clusterClient(
	t *testing.T,
	generation int64,
	profiles []ResolvedWorkloadProfile,
	mutate func(*unstructured.Unstructured),
) dynamic.Interface {
	t.Helper()
	hash, err := ProfileSetHash(profiles)
	if err != nil {
		t.Fatal(err)
	}
	profileStatus, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ProfileSetStatus{
		ObservedGeneration: generation,
		Observed:           int32(len(profiles)),
		Ready:              int32(len(profiles)),
		ProfileSetHash:     hash,
		Profiles:           profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	conditions, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&struct {
		Conditions []metav1.Condition `json:"conditions"`
	}{Conditions: []metav1.Condition{{
		Type:               ConditionWorkloadProfilesReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "WorkloadProfilesReady",
		Message:            "ready",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tau.azure.com/v1alpha1",
		"kind":       "TauCluster",
		"metadata": map[string]any{
			"name":       TauClusterName,
			"generation": generation,
		},
		"status": map[string]any{
			"workloadProfiles": profileStatus,
			"conditions":       conditions["conditions"],
		},
	}}
	if mutate != nil {
		mutate(object)
	}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(TauClusterGVR).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return client
}

func readyProviderProfile(name string, generation int64) ResolvedWorkloadProfile {
	profile := testResolvedProfile(name)
	profile.Conditions = []metav1.Condition{{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "Ready",
		Message:            "ready",
	}}
	return profile
}
