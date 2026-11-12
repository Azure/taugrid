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

	"github.com/spf13/cobra"
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
	dir := renderCatalogScaffold(t)

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
			if got.lane != "cpu" {
				t.Fatalf("unedited %s target resolved lane %q, want cpu", target, got.lane)
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

func TestLegacyScaffoldHasNoLaneAndKeepsResolvingOnReselection(t *testing.T) {
	provider := defaultCatalogProvider(t)
	dir := renderCatalogScaffold(t)
	for _, target := range []string{"smoke", "train"} {
		t.Run(target, func(t *testing.T) {
			options := scaffoldTargetOptions(t, dir, target)
			if options.lane != "" {
				t.Fatalf("generated %s target sets policy.lane=%q; pre-change scaffolds have none", target, options.lane)
			}
			submitted, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			for name, dispatch := range map[string]unresolvedRunOptions{
				"unresolved dispatch": options,
				"resolved dispatch":   submitted,
			} {
				reselected, err := selectRunWorkloadProfile(context.Background(), dispatch, provider)
				if err != nil {
					t.Fatalf("reselect %s: %v", name, err)
				}
				if reselected.profileName != "azure.workspace.cpu" || reselected.lane != "cpu" {
					t.Fatalf("reselect %s = %q lane %q, want azure.workspace.cpu lane cpu", name, reselected.profileName, reselected.lane)
				}
			}
		})
	}
}

func TestLegacyScaffoldRendersLikeExplicitCPULane(t *testing.T) {
	provider := defaultCatalogProvider(t)
	dir := renderCatalogScaffold(t)
	for _, target := range []string{"smoke", "train"} {
		t.Run(target, func(t *testing.T) {
			legacy := scaffoldTargetOptions(t, dir, target)
			explicit := scaffoldTargetOptions(t, dir, target)
			explicit.lane = "cpu"
			if got, want := renderSelectedForTest(t, legacy, provider), renderSelectedForTest(t, explicit, provider); got != want {
				t.Fatalf("legacy %s render differs from explicit lane cpu\nlegacy:\n%s\nexplicit:\n%s", target, got, want)
			}
		})
	}
}

func TestLegacyCPUFallbackKeepsSiteUnscopedCPUProfile(t *testing.T) {
	profiles := gpuCatalogProfiles(defaultCatalogProfiles(t))
	site := unscopedCPUProfileForTest(t, "site.cpu")
	provider := catalogProviderFor(t, append(profiles, site))
	options := scaffoldTargetOptions(t, renderCatalogScaffold(t), "smoke")
	got, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err != nil {
		t.Fatalf("select with site CPU profile: %v", err)
	}
	if got.profileName != "site.cpu" || got.lane != "" {
		t.Fatalf("selected %q lane %q, want site.cpu with no lane", got.profileName, got.lane)
	}
}

func TestLegacyCPUFallbackDoesNotHideAmbiguity(t *testing.T) {
	profiles := append(defaultCatalogProfiles(t),
		unscopedCPUProfileForTest(t, "site.cpu-a"),
		unscopedCPUProfileForTest(t, "site.cpu-b"),
	)
	options := scaffoldTargetOptions(t, renderCatalogScaffold(t), "smoke")
	_, err := selectRunWorkloadProfile(context.Background(), options, catalogProviderFor(t, profiles))
	if err == nil || !strings.Contains(err.Error(), "multiple ready workload profiles match") {
		t.Fatalf("ambiguous unscoped CPU profiles error = %v", err)
	}
}

func TestLegacyCPUFallbackRequiresCPUOnlyRequest(t *testing.T) {
	provider := defaultCatalogProvider(t)
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.namespace = defaultCatalogNamespace
	_, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err == nil || !strings.Contains(err.Error(), `lane=""`) {
		t.Fatalf("lane-less run without a GPU count error = %v", err)
	}
}

func TestLegacyCPUFallbackReportsOriginalScopeWithoutCPUProfile(t *testing.T) {
	provider := catalogProviderFor(t, gpuCatalogProfiles(defaultCatalogProfiles(t)))
	options := scaffoldTargetOptions(t, renderCatalogScaffold(t), "smoke")
	_, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err == nil || !strings.Contains(err.Error(), `lane=""`) {
		t.Fatalf("lane-less CPU run without a CPU profile error = %v", err)
	}
}

func TestExplicitCPUProfileNameWithoutLaneResolves(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.namespace = defaultCatalogNamespace
	options.profileName = "azure.workspace.cpu"
	options.profileNameExplicit = true
	got, err := selectRunWorkloadProfile(context.Background(), options, defaultCatalogProvider(t))
	if err != nil {
		t.Fatalf("select explicit azure.workspace.cpu without a lane: %v", err)
	}
	if got.profileName != "azure.workspace.cpu" || got.lane != "cpu" {
		t.Fatalf("selected %q lane %q, want azure.workspace.cpu lane cpu", got.profileName, got.lane)
	}
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

func renderCatalogScaffold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := reposcaffold.Render(reposcaffold.Options{
		Name:      "catalog-check",
		OutputDir: dir,
		Image:     "example.azurecr.io/catalog-check:test",
		Workspace: defaultCatalogNamespace,
	}); err != nil {
		t.Fatalf("render scaffold: %v", err)
	}
	return dir
}

func renderSelectedForTest(t *testing.T, options unresolvedRunOptions, provider *profile.Provider) string {
	t.Helper()
	options.dryRun = "client"
	options.submissionID = "catalog-check"
	selected, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	target, err := resolveRunTarget(selected, "catalog-check")
	if err != nil {
		t.Fatalf("resolve run target: %v", err)
	}
	var stdout, stderr bytes.Buffer
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	parent.SetOut(&stdout)
	parent.SetErr(&stderr)
	if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
		t.Fatalf("render: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "kind: Job") {
		t.Fatalf("dry run did not render a Job:\n%s", stdout.String())
	}
	return stdout.String()
}

func unscopedCPUProfileForTest(t *testing.T, name string) profile.ResolvedWorkloadProfile {
	t.Helper()
	for _, candidate := range defaultCatalogProfiles(t) {
		if candidate.Name == "azure.workspace.cpu" {
			candidate.Name = name
			candidate.Applicability.Lanes = nil
			return candidate
		}
	}
	t.Fatal("default catalog has no azure.workspace.cpu profile")
	return profile.ResolvedWorkloadProfile{}
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

func TestDefaultCatalogCPUProfileReachesManagedCPURayJobRender(t *testing.T) {
	for _, lane := range []string{"cpu", ""} {
		t.Run("lane="+lane, func(t *testing.T) {
			assertManagedCPURayJobRender(t, lane)
		})
	}
}

func assertManagedCPURayJobRender(t *testing.T, lane string) {
	t.Helper()
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: cpu-workflow
compute: { gpus: 0, workers: 1 }
runtime:
  image: example.azurecr.io/cpu-workflow:test
  pip: [torch]
`)
	options, _, err := loadRunConfig(manifestPath)
	if err != nil {
		t.Fatalf("load managed CPU manifest: %v", err)
	}
	if options.file != manifestPath {
		t.Fatalf("manifest loaded as %q, want managed workflow %q", options.file, manifestPath)
	}
	options.mainScript = writeMainScript(t)
	options.namespace = defaultCatalogNamespace
	options.lane = lane
	options.workloadKind = "rayjob"
	options.dryRun = "client"
	options, err = selectRunWorkloadProfile(context.Background(), options, defaultCatalogProvider(t))
	if err != nil {
		t.Fatalf("select default catalog CPU profile: %v", err)
	}
	if options.profileName != "azure.workspace.cpu" {
		t.Fatalf("selected %q, want azure.workspace.cpu", options.profileName)
	}
	request, err := newRunManagedWorkflowRequest(options)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := executeRunManagedWorkflow(context.Background(), &stdout, &stderr, &request, "tau run"); err != nil {
		t.Fatalf("managed CPU RayJob render: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "kind: RayJob") {
		t.Fatalf("managed CPU workflow did not render a RayJob:\n%s", stdout.String())
	}
}
