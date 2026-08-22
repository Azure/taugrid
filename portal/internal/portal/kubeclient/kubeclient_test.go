// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kubeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestProfileSetGetsSingletonTauClusterThroughDynamicClient(t *testing.T) {
	const generation = int64(8)
	resolved := profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name: "research", GPUsPerWorker: 1, WorkerCount: 1,
			Mode: profile.ModeFixed, Placement: profile.PlacementIndependent,
			DefaultLocalQueue: "jobqueue", ExecutionTarget: profile.ExecutionTargetSingleCluster,
			Priorities: profile.ProfilePriorities{DisableDefaultPriorities: true},
		},
		Conditions: []metav1.Condition{{
			Type: profile.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: generation,
		}},
	}
	hash, err := profile.ProfileSetHash([]profile.ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile.ProfileSetStatus{
		ObservedGeneration: generation, Observed: 1, Ready: 1,
		ProfileSetHash: hash, Profiles: []profile.ResolvedWorkloadProfile{resolved},
	})
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{
		"apiVersion": "tau.azure.com/v1alpha1", "kind": "TauCluster",
		"metadata": map[string]any{"name": profile.TauClusterName, "generation": generation},
		"status": map[string]any{
			"workloadProfiles": status,
			"conditions": []any{map[string]any{
				"type": profile.ConditionWorkloadProfilesReady, "status": "True",
				"observedGeneration": generation, "reason": "Ready",
			}},
		},
	}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(object)
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewForDynamic(client).ProfileSet(context.Background())
	if err != nil {
		t.Fatalf("ProfileSet: %v", err)
	}
	if got.Generation != generation || got.ProfileSetHash != hash || len(got.Profiles) != 1 {
		t.Fatalf("profile set = %#v", got)
	}
	if gotPath != "/apis/tau.azure.com/v1alpha1/clusters/cluster" {
		t.Fatalf("TauCluster GET path = %q", gotPath)
	}
}
