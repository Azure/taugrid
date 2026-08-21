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

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

func TestRunConfigResolvesWorkloadProfileSnapshotRelativeToConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "experiment", "tau.yaml")
	got, err := configToDispatch(runconfig.Config{
		Policy: runconfig.Policy{WorkloadProfileSnapshot: "profiles/snapshot.yaml"},
	}, configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(configPath), "profiles", "snapshot.yaml")
	if got.workloadProfileSnapshot != want {
		t.Fatalf("snapshot path = %q, want %q", got.workloadProfileSnapshot, want)
	}
}

func TestRunHelpDocumentsProfileSource(t *testing.T) {
	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer {
		return &fakeRunConnectionEnsurer{}
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "policy.workload_profile_snapshot") {
		t.Fatalf("run help missing profile snapshot contract:\n%s", stdout.String())
	}
	for _, removed := range []string{"--acknowledge-beta-feature", "execution.beta_features"} {
		if strings.Contains(stdout.String(), removed) {
			t.Fatalf("run help unexpectedly contains removed acknowledgement %q:\n%s", removed, stdout.String())
		}
	}
	resumeCmd, _, err := cmd.Find([]string{"resume"})
	if err != nil {
		t.Fatal(err)
	}
	if resumeCmd.Flags().Lookup("acknowledge-beta-feature") != nil {
		t.Fatal("run resume exposes removed --acknowledge-beta-feature")
	}
}

func TestConnectedClientDryRunFetchesTauClusterProfiles(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: connected
engine: job
entrypoint: train.sh
runtime:
  image: busybox:1.36
compute:
  gpus: 1
policy:
  profile: research-1gpu
  namespace: alpha
  team: research
  lane: training
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "connected-context",
		Namespace:   "alpha",
	}}
	clientCalls := 0
	originalClient := newClusterProfileClient
	newClusterProfileClient = func(kubeContext string) (dynamic.Interface, error) {
		clientCalls++
		if kubeContext != "connected-context" {
			t.Fatalf("profile client context = %q", kubeContext)
		}
		return readyClusterProfileClient(t, 9, false), nil
	}
	t.Cleanup(func() { newClusterProfileClient = originalClient })

	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer { return ensurer })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", config, "--dry-run=client"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("connected client dry-run: %v\nstderr:\n%s", err, stderr.String())
	}
	if ensurer.calls != 1 || clientCalls != 1 {
		t.Fatalf("connection calls=%d TauCluster client calls=%d", ensurer.calls, clientCalls)
	}
	for _, want := range []string{
		workloadmeta.AnnotationTauClusterGeneration,
		workloadmeta.AnnotationWorkloadProfileSetHash,
		workloadmeta.AnnotationWorkloadProfileName,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("render missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestOfflineSnapshotClientDryRunSkipsConnectionAndTauCluster(t *testing.T) {
	dir := t.TempDir()
	writeOfflineSnapshotRun(t, dir)
	ensurer := &fakeRunConnectionEnsurer{}
	clientCalls := 0
	originalClient := newClusterProfileClient
	newClusterProfileClient = func(string) (dynamic.Interface, error) {
		clientCalls++
		return nil, nil
	}
	t.Cleanup(func() { newClusterProfileClient = originalClient })

	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer { return ensurer })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", filepath.Join(dir, "tau.yaml"), "--dry-run=client"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("offline snapshot dry-run: %v\nstderr:\n%s", err, stderr.String())
	}
	if ensurer.calls != 0 || clientCalls != 0 {
		t.Fatalf("offline snapshot made connection calls=%d TauCluster client calls=%d", ensurer.calls, clientCalls)
	}
	if !strings.Contains(stdout.String(), workloadmeta.AnnotationWorkloadProfileSetHash) {
		t.Fatalf("snapshot render missing profile revision:\n%s", stdout.String())
	}
}

func TestSnapshotRejectsServerDryRunAndApplyBeforeConnection(t *testing.T) {
	for _, dryRun := range []string{"", "server"} {
		t.Run("dry-run="+dryRun, func(t *testing.T) {
			dir := t.TempDir()
			writeOfflineSnapshotRun(t, dir)
			ensurer := &fakeRunConnectionEnsurer{}
			cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer { return ensurer })
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"--config", filepath.Join(dir, "tau.yaml")}
			if dryRun != "" {
				args = append(args, "--dry-run="+dryRun)
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "requires --dry-run=client") {
				t.Fatalf("error = %v", err)
			}
			if ensurer.calls != 0 {
				t.Fatalf("invalid snapshot mode contacted connection %d times", ensurer.calls)
			}
		})
	}
}

func TestSelectedProfileRejectsAuthoritativeConflicts(t *testing.T) {
	provider := testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 2, 3)
	base := defaultRunDispatchOptions()
	base.engine = runconfig.EngineRayJob
	base.profileName = "research-profile"
	base.namespace = "alpha"
	base.team = "research"
	base.lane = "training"

	tests := []struct {
		name   string
		mutate func(*unresolvedRunOptions)
		want   string
	}{
		{"queue", func(o *unresolvedRunOptions) { o.queue = "other" }, "policy.queue"},
		{"mode", func(o *unresolvedRunOptions) { o.mode = "elastic" }, "policy.mode"},
		{"placement", func(o *unresolvedRunOptions) { o.topology = profile.PlacementIndependent }, "policy.topology"},
		{"workers", func(o *unresolvedRunOptions) { o.workers, o.workersExplicit = 4, true }, "compute.workers"},
		{"gpus", func(o *unresolvedRunOptions) { o.gpusPerWorker, o.gpusPerWorkerExplicit = 1, true }, "compute.gpus_per_worker"},
		{"priority", func(o *unresolvedRunOptions) { o.podPriorityClass = "other" }, "policy.pod_priority_class"},
		{"disabled priorities", func(o *unresolvedRunOptions) {
			o.disableDefaultPriorities, o.disablePrioritiesExplicit = true, true
		}, "policy.disable_default_priorities"},
		{"node selector", func(o *unresolvedRunOptions) { o.nodeSelectors = []string{"gpu=other"} }, "policy.node_selector"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := base
			test.mutate(&options)
			_, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSelectedProfilePreservesExplicitRuntimeAndStorageSettings(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = "research-profile"
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	options.image = "registry.example/research:v1"
	options.cpuRequest = "4"
	options.memoryRequest = "32Gi"
	options.dataPVC = "research-data"
	options.output = "/data/results/run"

	got, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 1, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.image != options.image ||
		got.cpuRequest != options.cpuRequest ||
		got.memoryRequest != options.memoryRequest ||
		got.dataPVC != options.dataPVC ||
		got.output != options.output {
		t.Fatalf("runtime/storage settings changed:\nbefore=%#v\nafter=%#v", options, got)
	}
}

func TestRunSelectionAllowsUniqueImplicitProfiles(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	got, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 1, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.profileName != "research-profile" || got.selectedWorkloadProfile == nil {
		t.Fatalf("implicit normal selection = %#v", got.selectedWorkloadProfile)
	}

	got, err = selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProfileProvider(t, profile.ExecutionTargetMultiKueue, 1, 1),
	)
	if err != nil {
		t.Fatalf("implicit multiKueue selection: %v", err)
	}
	if got.selectedWorkloadProfile == nil ||
		got.selectedWorkloadProfile.Selection.Profile.ExecutionTarget != profile.ExecutionTargetMultiKueue {
		t.Fatalf("implicit multiKueue selection = %#v", got.selectedWorkloadProfile)
	}
}

func TestProfileRefreshDistinguishesConfigAssertionsFromPriorRevisionValues(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.profileName = "research-profile"
	options.profileNameExplicit = true
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	options.explicitPolicyFields = map[string]bool{}

	first, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 1, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	changed := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 2, 1, 12)
	changed.Mode = profile.ModeElastic
	changed.DefaultLocalQueue = "next-queue"
	changed.LocalQueues[0].Name = "next-queue"
	refreshed, err := selectRunWorkloadProfile(
		context.Background(),
		first,
		testRunProviderForResolved(t, 12, changed),
	)
	if err != nil {
		t.Fatalf("refresh treated prior profile values as explicit config: %v", err)
	}
	if refreshed.queue != "next-queue" || refreshed.mode != profile.ModeElastic ||
		refreshed.jobGPUs == nil || *refreshed.jobGPUs != 2 {
		t.Fatalf("refreshed profile values = %#v", refreshed.runPlacement)
	}

	first.explicitPolicyFields["queue"] = true
	_, err = selectRunWorkloadProfile(
		context.Background(),
		first,
		testRunProviderForResolved(t, 12, changed),
	)
	if err == nil || !strings.Contains(err.Error(), "policy.queue") {
		t.Fatalf("explicit queue assertion did not conflict with refreshed revision: %v", err)
	}
}

func TestMultiKueueProfileRunsInEverySubmissionMode(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetMultiKueue, 1, 1, 11)
	provider := profile.NewClusterProvider(readyClusterProfileClientForProfiles(t, 11, resolved))
	for _, dryRun := range []string{"client", "server", ""} {
		t.Run("dry-run="+dryRun, func(t *testing.T) {
			options := defaultRunDispatchOptions()
			options.engine = runconfig.EngineJob
			options.profileName = "research-profile"
			options.namespace = "alpha"
			options.team = "research"
			options.lane = "training"
			options.dryRun = dryRun

			selected, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err != nil {
				t.Fatalf("multiKueue selection: %v", err)
			}
			if err := validateSelectedWorkloadProfileMode(selected.selectedWorkloadProfile, dryRun); err != nil {
				t.Fatalf("multiKueue mode validation: %v", err)
			}
			labels, annotations, err := stampSelectedWorkloadProfile(nil, nil, selected.selectedWorkloadProfile)
			if err != nil {
				t.Fatal(err)
			}
			if labels[workloadmeta.LabelProfile] != "research-profile" {
				t.Fatalf("profile labels=%#v", labels)
			}
			if len(annotations) != 3 ||
				annotations[workloadmeta.AnnotationTauClusterGeneration] != "11" ||
				annotations[workloadmeta.AnnotationWorkloadProfileName] != "research-profile" ||
				annotations[workloadmeta.AnnotationWorkloadProfileSetHash] == "" {
				t.Fatalf("profile annotations=%#v", annotations)
			}
		})
	}
}

func TestProfileRevisionAnnotationsReachRayJobAndManagedWorkflowRenders(t *testing.T) {
	t.Run("rayjob", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "train.py")
		if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		options := defaultRunDispatchOptions()
		options.engine = runconfig.EngineRayJob
		options.script = script
		options.image = "rayproject/ray:2.9.0"
		options.namespace = "alpha"
		options.team = "research"
		options.lane = "training"
		options.profileName = "research-profile"
		options.dryRun = "client"
		var err error
		options, err = selectRunWorkloadProfile(
			context.Background(),
			options,
			testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 1, 1),
		)
		if err != nil {
			t.Fatal(err)
		}
		request, err := newRunRayJobRequest(options, "revision-ray")
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if err := executeRunRayJob(context.Background(), &stdout, &stderr, &request, "tau run"); err != nil {
			t.Fatalf("RayJob render: %v\nstderr:\n%s", err, stderr.String())
		}
		assertProfileRevisionMetadata(t, stdout.String(), options.selectedWorkloadProfile)
	})

	t.Run("managed workflow", func(t *testing.T) {
		manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: revision-workflow
compute: { gpus: 1, workers: 1 }
runtime:
  pip: [torch]
`)
		options := defaultRunDispatchOptions()
		options.file = manifestPath
		options.mainScript = writeMainScript(t)
		options.namespace = "alpha"
		options.team = "research"
		options.lane = "training"
		options.profileName = "research-profile"
		options.dryRun = "client"
		var err error
		options, err = selectRunWorkloadProfile(
			context.Background(),
			options,
			testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 1, 1),
		)
		if err != nil {
			t.Fatal(err)
		}
		request, err := newRunManagedWorkflowRequest(options)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if err := executeRunManagedWorkflow(context.Background(), &stdout, &stderr, &request, "tau run"); err != nil {
			t.Fatalf("managed workflow render: %v\nstderr:\n%s", err, stderr.String())
		}
		assertProfileRevisionMetadata(t, stdout.String(), options.selectedWorkloadProfile)
	})
}

func TestMultiKueueProfileRevisionAnnotationsReachRenderedJob(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineJob
	options.script = script
	options.image = "busybox:1.36"
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	options.profileName = "research-profile"
	options.dryRun = "client"
	var err error
	options, err = selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProfileProvider(t, profile.ExecutionTargetMultiKueue, 1, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := newRunJobRequest(options, "multikueue-job")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := executeRunJob(context.Background(), &stdout, &stderr, &request, "tau run"); err != nil {
		t.Fatalf("multiKueue Job render: %v\nstderr:\n%s", err, stderr.String())
	}
	assertProfileRevisionMetadata(t, stdout.String(), options.selectedWorkloadProfile)
}

func assertProfileRevisionMetadata(t *testing.T, rendered string, selected *selectedWorkloadProfile) {
	t.Helper()
	for _, want := range []string{
		workloadmeta.AnnotationTauClusterGeneration,
		workloadmeta.AnnotationWorkloadProfileSetHash,
		selected.Selection.ProfileSetHash,
		workloadmeta.AnnotationWorkloadProfileName,
		selected.Selection.Profile.Name,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing full profile revision value %q:\n%s", want, rendered)
		}
	}
}

func writeOfflineSnapshotRun(t *testing.T, dir string) {
	t.Helper()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	provider := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 1, 1, 11)
	snapshot, err := profile.NewProfileSetSnapshot(11, []profile.ResolvedWorkloadProfile{provider})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	config := `name: offline
engine: job
entrypoint: train.sh
runtime:
  image: busybox:1.36
compute:
  gpus: 1
policy:
  profile: research-profile
  namespace: alpha
  team: research
  lane: training
  workload_profile_snapshot: profiles.yaml
`
	if err := os.WriteFile(filepath.Join(dir, "tau.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testRunProfileProvider(
	t *testing.T,
	target profile.ExecutionTarget,
	gpus, workers int32,
) *profile.Provider {
	t.Helper()
	resolved := testRunResolvedProfile(target, gpus, workers, 11)
	return testRunProviderForResolved(t, 11, resolved)
}

func testRunProviderForResolved(
	t *testing.T,
	generation int64,
	resolved profile.ResolvedWorkloadProfile,
) *profile.Provider {
	t.Helper()
	snapshot, err := profile.NewProfileSetSnapshot(generation, []profile.ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := profile.NewSnapshotProvider(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func testRunResolvedProfile(
	target profile.ExecutionTarget,
	gpus, workers int32,
	generation int64,
) profile.ResolvedWorkloadProfile {
	placement := profile.PlacementIndependent
	if workers > 1 {
		placement = profile.PlacementMultiNodeNCCL
	}
	return profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name: "research-profile",
			Applicability: profile.ProfileApplicability{
				Namespaces: []string{"alpha"},
				Teams:      []string{"research"},
				Lanes:      []string{"training"},
			},
			GPUsPerWorker:     gpus,
			WorkerCount:       workers,
			Mode:              profile.ModeFixed,
			Placement:         placement,
			DefaultLocalQueue: "jobqueue",
			ExecutionTarget:   target,
			Priorities: profile.ProfilePriorities{
				WorkloadPriorityClassName: "tau-default",
				PodPriorityClassName:      "tau-default",
			},
		},
		LocalQueues: []profile.ResolvedLocalQueue{{
			Namespace: "alpha", Name: "jobqueue", ClusterQueue: "gpu-cq",
		}},
		ClusterQueues:           []string{"gpu-cq"},
		ResourceFlavors:         []string{"gpu"},
		WorkloadPriorityClasses: []string{"tau-default"},
		PodPriorityClasses:      []string{"tau-default"},
		Conditions: []metav1.Condition{{
			Type:               profile.ConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			Reason:             "Ready",
		}},
	}
}
