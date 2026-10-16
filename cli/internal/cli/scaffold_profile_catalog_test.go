// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/taugrid/cli/internal/reposcaffold"
	profile "github.com/Azure/taugrid/core/resourceprofile"
)

const defaultCatalogNamespace = "taugrid-default"

func TestGeneratedScaffoldSelectsDefaultCatalogProfiles(t *testing.T) {
	provider := defaultCatalogProvider(t)
	dir := t.TempDir()
	if _, err := reposcaffold.Render(reposcaffold.Options{
		Name:      "catalog-check",
		OutputDir: dir,
		Image:     "example.azurecr.io/catalog-check:test",
		Workspace: defaultCatalogNamespace,
	}); err != nil {
		t.Fatalf("render scaffold: %v", err)
	}

	for _, target := range []string{"smoke", "train"} {
		t.Run(target, func(t *testing.T) {
			options := scaffoldTargetOptions(t, dir, target)
			got, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err != nil {
				t.Fatalf("unedited %s target cannot use the default catalog: %v", target, err)
			}
			if got.profileName != "azure.workspace.cpu" {
				t.Fatalf("unedited %s target selected %q, want azure.workspace.cpu", target, got.profileName)
			}
		})
	}

	t.Run("train-gpu", func(t *testing.T) {
		options := scaffoldTargetOptions(t, dir, "train-gpu")
		_, err := selectRunWorkloadProfile(context.Background(), options, provider)
		if err == nil ||
			!strings.Contains(err.Error(), "gpusPerWorker=1") ||
			!strings.Contains(err.Error(), "azure.workspace.cpu (gpusPerWorker=0)") {
			t.Fatalf("unedited train-gpu selection error = %v", err)
		}
	})
}

func scaffoldTargetOptions(t *testing.T, dir, target string) unresolvedRunOptions {
	t.Helper()
	options, _, err := loadRunConfig(filepath.Join(dir, "tau", target+".yaml"))
	if err != nil {
		t.Fatalf("load %s config: %v", target, err)
	}
	options.namespace = defaultCatalogNamespace
	return options
}

func defaultCatalogProvider(t *testing.T) *profile.Provider {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "tau-core-controller", "kustomize", "taucluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cluster struct {
		Spec struct {
			WorkloadProfiles []profile.WorkloadProfile `yaml:"workloadProfiles"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cluster); err != nil {
		t.Fatalf("decode default catalog: %v", err)
	}
	if len(cluster.Spec.WorkloadProfiles) == 0 {
		t.Fatal("default catalog has no workload profiles")
	}
	const generation = 1
	resolved := make([]profile.ResolvedWorkloadProfile, 0, len(cluster.Spec.WorkloadProfiles))
	for _, intent := range cluster.Spec.WorkloadProfiles {
		item := profile.ResolvedWorkloadProfile{
			WorkloadProfile: intent,
			LocalQueues: []profile.ResolvedLocalQueue{{
				Namespace: defaultCatalogNamespace, Name: intent.DefaultLocalQueue, ClusterQueue: "jobqueue",
			}},
			ClusterQueues:   []string{"jobqueue"},
			ResourceFlavors: []string{"taugrid-default-cpu"},
			Conditions: []metav1.Condition{{
				Type:               profile.ConditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: generation,
				Reason:             "Ready",
				Message:            "ready",
			}},
		}
		if !intent.Priorities.DisableDefaultPriorities {
			item.WorkloadPriorityClasses = []string{intent.Priorities.WorkloadPriorityClassName}
			item.PodPriorityClasses = []string{intent.Priorities.PodPriorityClassName}
		}
		resolved = append(resolved, item)
	}
	snapshot, err := profile.NewProfileSetSnapshot(generation, resolved)
	if err != nil {
		t.Fatalf("snapshot default catalog: %v", err)
	}
	provider, err := profile.NewSnapshotProvider(snapshot)
	if err != nil {
		t.Fatalf("default catalog provider: %v", err)
	}
	return provider
}
