// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

func TestClusterProfilesExportCommand(t *testing.T) {
	client := readyClusterProfileClient(t, 9, false)
	original := newClusterProfileClient
	newClusterProfileClient = func(kubeContext string) (dynamic.Interface, error) {
		if kubeContext != "aks-dev" {
			t.Fatalf("context = %q, want aks-dev", kubeContext)
		}
		return client, nil
	}
	t.Cleanup(func() { newClusterProfileClient = original })

	out, err := runCluster(t, "profiles", "export", "--context", "aks-dev")
	if err != nil {
		t.Fatalf("profiles export error = %v", err)
	}
	var snapshot profile.ProfileSetSnapshot
	if err := yaml.Unmarshal([]byte(out), &snapshot); err != nil {
		t.Fatalf("decode export:\n%s\nerror: %v", out, err)
	}
	if snapshot.Kind != profile.ProfileSnapshotKind || snapshot.TauClusterGeneration != 9 ||
		len(snapshot.Profiles) != 1 || snapshot.Profiles[0].Name != "research-1gpu" {
		t.Fatalf("exported snapshot = %#v", snapshot)
	}
	decoded, err := profile.DecodeProfileSetSnapshot([]byte(out))
	if err != nil {
		t.Fatalf("export is not a valid shared snapshot: %v", err)
	}
	if decoded.ProfileSetHash != snapshot.ProfileSetHash {
		t.Fatalf("decoded hash = %q, exported %q", decoded.ProfileSetHash, snapshot.ProfileSetHash)
	}
}

func TestClusterProfilesExportIsDeterministic(t *testing.T) {
	client := readyClusterProfileClient(t, 9, false)
	var first, second bytes.Buffer
	if err := exportClusterProfiles(context.Background(), client, &first, ""); err != nil {
		t.Fatal(err)
	}
	if err := exportClusterProfiles(context.Background(), client, &second, ""); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("exports differ:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
}

func TestClusterProfilesExportWritesExplicitOutputPath(t *testing.T) {
	client := readyClusterProfileClient(t, 9, false)
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	var out bytes.Buffer
	if err := exportClusterProfiles(context.Background(), client, &out, path); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("explicit output path also wrote stdout:\n%s", out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.DecodeProfileSetSnapshot(data); err != nil {
		t.Fatalf("output path does not contain a valid snapshot: %v", err)
	}
}

func TestClusterProfilesExportWritesNoPartialOutputOnError(t *testing.T) {
	client := readyClusterProfileClient(t, 9, true)
	var out bytes.Buffer
	err := exportClusterProfiles(context.Background(), client, &out, "")
	if err == nil || !strings.Contains(err.Error(), "workload profiles are stale") {
		t.Fatalf("export error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("partial output on error:\n%s", out.String())
	}
}

func TestClusterProfilesHelpDocumentsExport(t *testing.T) {
	out, err := runCluster(t, "profiles", "export", "--help")
	if err != nil {
		t.Fatalf("profiles export help error = %v", err)
	}
	for _, want := range []string{"TauWorkloadProfileSnapshot", "--output", "--context", "ready TauCluster status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

func readyClusterProfileClient(t *testing.T, generation int64, stale bool) dynamic.Interface {
	t.Helper()
	resolved := profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name: "research-1gpu",
			Applicability: profile.ProfileApplicability{
				Teams: []string{"research"}, Lanes: []string{"training"}, Namespaces: []string{"alpha"},
			},
			GPUsPerWorker:     1,
			WorkerCount:       1,
			Mode:              profile.ModeFixed,
			Placement:         profile.PlacementIndependent,
			DefaultLocalQueue: "jobqueue",
			ExecutionTarget:   profile.ExecutionTargetSingleCluster,
			Priorities: profile.ProfilePriorities{
				WorkloadPriorityClassName: "tau-default",
				PodPriorityClassName:      "tau-default",
			},
		},
		LocalQueues: []profile.ResolvedLocalQueue{{
			Namespace: "alpha", Name: "jobqueue", ClusterQueue: "gpu-cq",
		}},
		ClusterQueues:           []string{"gpu-cq"},
		ResourceFlavors:         []string{"a100"},
		Topologies:              []string{"default"},
		WorkloadPriorityClasses: []string{"tau-default"},
		PodPriorityClasses:      []string{"tau-default"},
		Conditions: []metav1.Condition{{
			Type:               profile.ConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             "Ready",
			Message:            "ready",
		}},
	}
	hash, err := profile.ProfileSetHash([]profile.ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatal(err)
	}
	observedGeneration := generation
	if stale {
		observedGeneration--
	}
	workloadProfiles, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile.ProfileSetStatus{
		ObservedGeneration: observedGeneration,
		Observed:           1,
		Ready:              1,
		ProfileSetHash:     hash,
		Profiles:           []profile.ResolvedWorkloadProfile{resolved},
	})
	if err != nil {
		t.Fatal(err)
	}
	conditionDoc, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&struct {
		Conditions []metav1.Condition `json:"conditions"`
	}{Conditions: []metav1.Condition{{
		Type:               profile.ConditionWorkloadProfilesReady,
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
			"name":       profile.TauClusterName,
			"generation": generation,
		},
		"status": map[string]any{
			"workloadProfiles": workloadProfiles,
			"conditions":       conditionDoc["conditions"],
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(profile.TauClusterGVR).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return client
}
