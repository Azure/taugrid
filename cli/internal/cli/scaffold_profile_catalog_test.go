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
	"github.com/Azure/taugrid/core/runconfig"
)

const defaultCatalogNamespace = "taugrid-default"

const catalogGeneration = 1

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
			!strings.Contains(err.Error(), "no ready workload profile matches") ||
			strings.Contains(err.Error(), "azure.workspace.cpu") {
			t.Fatalf("unedited train-gpu selection error = %v", err)
		}
	})
}

func TestDefaultCatalogCPUProfilesDoNotChangeScopedSelection(t *testing.T) {
	full := defaultCatalogProfiles(t)
	gpuOnly := gpuCatalogProfiles(full)
	if len(gpuOnly) == len(full) {
		t.Fatal("default catalog has no CPU-only profile; this test no longer guards anything")
	}
	before := catalogProviderFor(t, gpuOnly)
	after := catalogProviderFor(t, full)

	type scope struct{ team, lane string }
	seen := map[scope]bool{}
	for _, candidate := range gpuOnly {
		for _, team := range candidate.Applicability.Teams {
			for _, lane := range candidate.Applicability.Lanes {
				seen[scope{team, lane}] = true
			}
		}
	}
	for s := range seen {
		request := profile.SelectionRequest{Namespace: defaultCatalogNamespace, Team: s.team, Lane: s.lane}
		want, wantErr := before.Select(context.Background(), request)
		got, gotErr := after.Select(context.Background(), request)
		if (wantErr == nil) != (gotErr == nil) || (wantErr == nil && want.Profile.Name != got.Profile.Name) {
			t.Errorf("team=%q lane=%q without a GPU count: before CPU profiles = %q (%v), after = %q (%v)",
				s.team, s.lane, want.Profile.Name, wantErr, got.Profile.Name, gotErr)
		}
	}
}

func TestScopedRunsKeepTheirProfileWithDefaultCatalog(t *testing.T) {
	provider := defaultCatalogProvider(t)
	one := 1
	for _, tc := range []struct {
		name string
		gpus *int
	}{{name: "omitted GPU count", gpus: nil}, {name: "explicit GPU count", gpus: &one}} {
		t.Run(tc.name, func(t *testing.T) {
			options := defaultRunDispatchOptions()
			options.engine = runconfig.EngineJob
			options.namespace = defaultCatalogNamespace
			options.team = "research"
			options.lane = "eval"
			if tc.gpus != nil {
				options.jobGPUs = tc.gpus
				options.jobGPUsExplicit = true
			}
			got, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err != nil {
				t.Fatalf("select research/eval: %v", err)
			}
			if got.profileName != "azure.research.eval.gpu" {
				t.Fatalf("selected %q, want azure.research.eval.gpu", got.profileName)
			}
		})
	}
}

func TestCPULaneFailsClosedWithoutCPUProfile(t *testing.T) {
	provider := catalogProviderFor(t, gpuCatalogProfiles(defaultCatalogProfiles(t)))
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.namespace = defaultCatalogNamespace
	options.lane = "cpu"
	_, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err == nil || !strings.Contains(err.Error(), "no ready workload profile matches") {
		t.Fatalf("select cpu lane without a CPU profile error = %v", err)
	}
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
	return catalogProviderFor(t, defaultCatalogProfiles(t))
}

func defaultCatalogProfiles(t *testing.T) []profile.ResolvedWorkloadProfile {
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
				ObservedGeneration: catalogGeneration,
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
	return resolved
}

func gpuCatalogProfiles(profiles []profile.ResolvedWorkloadProfile) []profile.ResolvedWorkloadProfile {
	var gpu []profile.ResolvedWorkloadProfile
	for _, candidate := range profiles {
		if candidate.GPUsPerWorker > 0 {
			gpu = append(gpu, candidate)
		}
	}
	return gpu
}

func catalogProviderFor(t *testing.T, resolved []profile.ResolvedWorkloadProfile) *profile.Provider {
	t.Helper()
	snapshot, err := profile.NewProfileSetSnapshot(catalogGeneration, resolved)
	if err != nil {
		t.Fatalf("snapshot default catalog: %v", err)
	}
	provider, err := profile.NewSnapshotProvider(snapshot)
	if err != nil {
		t.Fatalf("default catalog provider: %v", err)
	}
	return provider
}
