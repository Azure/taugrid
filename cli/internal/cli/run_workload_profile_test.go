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

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
}

func TestCapacityWorkspaceRunWithoutProfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.sh"), []byte("#!/bin/sh\necho train\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: direct
engine: job
entrypoint: train.sh
runtime:
  image: mcr.microsoft.com/azurelinux/base/core:3.0
compute:
  gpus: 1
  cpu_request: "1"
  memory_request: 1Gi
`), 0o644); err != nil {
		t.Fatal(err)
	}
	originalFetcher, originalClient := fetchRunWorkspace, newClusterProfileClient
	t.Cleanup(func() { fetchRunWorkspace, newClusterProfileClient = originalFetcher, originalClient })
	for _, name := range []string{"research", "development"} {
		t.Run(name, func(t *testing.T) {
			queue := managedWorkspaceClusterQueueName(name)
			ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
				Workspace: name, ContextName: "test-context", Namespace: name, Queue: queue,
			}}
			fetchRunWorkspace = func(_ *cobra.Command, _, _, _ string) (tauworkspace.Workspace, error) {
				w := readyWorkspace()
				w.Metadata.Name = name
				w.Spec.TeamRef = &tauworkspace.ClusterObjectReference{Name: "research"}
				w.Spec.Target.Namespace, w.Status.Target.ResolvedNamespace = name, name
				w.Spec.Queue, w.Status.Queue.LocalQueue, w.Status.Queue.ClusterQueue = queue, queue, queue
				return w, nil
			}
			newClusterProfileClient = func(string) (dynamic.Interface, error) {
				client := readyClusterProfileClientForProfiles(t, 4, false)
				cluster, err := client.Resource(profile.TauClusterGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				cluster.Object["spec"] = map[string]any{"managementMode": "Reconcile", "queues": map[string]any{
					"capacityTeamRef": map[string]any{"name": "research"},
				}, "workloadProfiles": []any{}}
				if err := unstructured.SetNestedField(cluster.Object, "Ready", "status", "phase"); err != nil {
					return nil, err
				}
				if err := unstructured.SetNestedField(cluster.Object, int64(4), "status", "observedGeneration"); err != nil {
					return nil, err
				}
				_, err = client.Resource(profile.TauClusterGVR).Update(context.Background(), cluster, metav1.UpdateOptions{})
				return client, err
			}
			cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer { return ensurer })
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"--config", config, "--dry-run=client"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("direct Tau run: %v\n%s", err, stderr.String())
			}
			var job struct {
				Metadata struct {
					Labels      map[string]string `yaml:"labels"`
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Suspend  bool `yaml:"suspend"`
					Template struct {
						Spec struct {
							Containers []struct {
								Resources struct {
									Requests map[string]string `yaml:"requests"`
									Limits   map[string]string `yaml:"limits"`
								} `yaml:"resources"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(stdout.Bytes(), &job); err != nil {
				t.Fatal(err)
			}
			if !job.Spec.Suspend || job.Metadata.Labels["kueue.x-k8s.io/queue-name"] != queue {
				t.Fatalf("job must be suspended on verified workspace queue:\n%s", stdout.String())
			}
			if job.Metadata.Annotations[workloadmeta.AnnotationWorkloadProfileName] != "" ||
				job.Metadata.Labels[workloadmeta.LabelProfile] != "" {
				t.Fatal("direct job must not claim a workload profile")
			}
			resources := job.Spec.Template.Spec.Containers[0].Resources
			if resources.Requests["nvidia.com/gpu"] != "1" || resources.Limits["nvidia.com/gpu"] != "1" ||
				resources.Requests["cpu"] != "1" || resources.Requests["memory"] != "1Gi" {
				t.Fatalf("explicit resources not preserved: %+v", resources)
			}
		})
	}
}

func TestCapacityJobPolicyGuards(t *testing.T) {
	originalClient := newClusterProfileClient
	t.Cleanup(func() { newClusterProfileClient = originalClient })
	for _, tt := range []struct {
		name   string
		change func(*unresolvedRunOptions, *unstructured.Unstructured)
		want   string
	}{
		{name: "explicit CPU", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { *o.jobGPUs = 0 }},
		{name: "missing cardinality", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.jobGPUs = nil }, want: "explicit non-negative compute.gpus"},
		{name: "negative cardinality", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { *o.jobGPUs = -1 }, want: "explicit non-negative compute.gpus"},
		{name: "shape", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.shape = "1x" }, want: "not policy.shape"},
		{name: "unverified queue", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.workspaceQueueResolved = false }, want: "verified team-managed"},
		{name: "external queue", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.workspaceClusterQueue = "external" }, want: "verified team-managed"},
		{name: "external LocalQueue", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.queue = "external" }, want: "verified team-managed"},
		{name: "team conflict", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.team = "another" }, want: "conflicts with TauWorkspace team"},
		{name: "stale cluster", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) { cluster.SetGeneration(5) }, want: "current observedGeneration"},
		{name: "degraded cluster", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) {
			cluster.Object["status"].(map[string]any)["phase"] = "Degraded"
		}, want: "Ready TauCluster"},
		{name: "observe mode", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) {
			cluster.Object["spec"].(map[string]any)["managementMode"] = "Observe"
		}, want: "Reconcile mode"},
		{name: "deleted cluster", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) {
			now := metav1.Now()
			cluster.SetDeletionTimestamp(&now)
		}, want: "Ready TauCluster"},
		{name: "malformed catalog", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) {
			cluster.Object["spec"].(map[string]any)["workloadProfiles"] = "invalid"
		}, want: "expected []interface{}"},
		{name: "nonempty catalog requires profile", change: func(_ *unresolvedRunOptions, cluster *unstructured.Unstructured) {
			cluster.Object["spec"].(map[string]any)["workloadProfiles"] = []any{map[string]any{"name": "pending"}}
		}, want: "no ready workload profile"},
		{name: "different pool team", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.workspaceTeam = "another" }, want: "no ready workload profile"},
		{name: "teamless workspace", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.workspaceTeam = "" }, want: "no ready workload profile"},
		{name: "explicit profile", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) {
			o.profileNameExplicit = true
			o.profileName = "requested"
		}, want: "requested"},
		{name: "Ray requires profile", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.engine = runconfig.EngineRayJob }, want: "no ready workload profile"},
		{name: "managed workflow requires profile", change: func(o *unresolvedRunOptions, _ *unstructured.Unstructured) { o.file = "workflow.yaml" }, want: "no ready workload profile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := readyClusterProfileClientForProfiles(t, 4, false)
			cluster, err := client.Resource(profile.TauClusterGVR).Get(context.Background(), profile.TauClusterName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cluster.Object["spec"] = map[string]any{
				"managementMode":   "Reconcile",
				"queues":           map[string]any{"capacityTeamRef": map[string]any{"name": "research"}},
				"workloadProfiles": []any{},
			}
			status := cluster.Object["status"].(map[string]any)
			status["phase"], status["observedGeneration"] = "Ready", int64(4)
			o := defaultRunDispatchOptions()
			o.engine = runconfig.EngineJob
			o.workspace, o.workspaceTeam = "sample", "research"
			o.workspaceQueueResolved = true
			o.queue, o.workspaceClusterQueue = "tau-ws-sample", "tau-ws-sample"
			gpus := 1
			o.jobGPUs = &gpus
			tt.change(&o, cluster)
			if _, err := client.Resource(profile.TauClusterGVR).Update(context.Background(), cluster, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			newClusterProfileClient = func(string) (dynamic.Interface, error) { return client, nil }
			got, err := resolveClusterRunWorkloadProfile(context.Background(), o)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.directJobProfile == nil || got.selectedWorkloadProfile != nil || got.directJobProfile.Resources.GPU.Count != *o.jobGPUs {
				t.Fatalf("unexpected direct GPU contract: %#v", got.directJobProfile)
			}
		})
	}
}

func TestCapacityJobRetainsReadyProfileSelection(t *testing.T) {
	originalClient := newClusterProfileClient
	t.Cleanup(func() { newClusterProfileClient = originalClient })
	client := readyClusterProfileClientForProfiles(t, 4, false, testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 1, 1, 4))
	cluster, err := client.Resource(profile.TauClusterGVR).Get(context.Background(), profile.TauClusterName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cluster.Object["spec"] = map[string]any{
		"queues":           map[string]any{"capacityTeamRef": map[string]any{"name": "research"}},
		"workloadProfiles": []any{map[string]any{"name": "research-profile"}},
	}
	if _, err := client.Resource(profile.TauClusterGVR).Update(context.Background(), cluster, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	newClusterProfileClient = func(string) (dynamic.Interface, error) { return client, nil }
	o := defaultRunDispatchOptions()
	o.engine = runconfig.EngineJob
	o.workspaceTeam, o.team, o.lane, o.namespace = "research", "research", "training", "alpha"
	got, err := resolveClusterRunWorkloadProfile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got.selectedWorkloadProfile == nil || got.selectedWorkloadProfile.Selection.Profile.Name != "research-profile" || got.directJobProfile != nil {
		t.Fatalf("ready profile was bypassed: %#v", got.selectedWorkloadProfile)
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
		Workspace:    "sample",
		WorkspaceUID: "workspace-uid",
		ContextName:  "connected-context",
		Namespace:    "alpha",
		Queue:        "jobqueue",
	}}
	originalWorkspaceFetcher := fetchRunWorkspace
	fetchRunWorkspace = func(_ *cobra.Command, kubeContext, systemNamespace, name string) (tauworkspace.Workspace, error) {
		if kubeContext != "connected-context" || systemNamespace != tauworkspace.SystemNamespace || name != "sample" {
			t.Fatalf(
				"workspace fetch context=%q systemNamespace=%q name=%q",
				kubeContext,
				systemNamespace,
				name,
			)
		}
		workspace := readyWorkspace()
		workspace.Metadata.UID = "workspace-uid"
		workspace.Spec.Target.Namespace = "alpha"
		workspace.Spec.Queue = "jobqueue"
		workspace.Status.Target.ResolvedNamespace = "alpha"
		workspace.Status.Queue.LocalQueue = "jobqueue"
		return workspace, nil
	}
	t.Cleanup(func() { fetchRunWorkspace = originalWorkspaceFetcher })
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

	fetchRunWorkspace = func(_ *cobra.Command, _, _, _ string) (tauworkspace.Workspace, error) {
		workspace := readyWorkspace()
		workspace.Metadata.UID = "replacement-uid"
		workspace.Spec.Target.Namespace = "alpha"
		workspace.Spec.Queue = "jobqueue"
		workspace.Status.Target.ResolvedNamespace = "alpha"
		workspace.Status.Queue.LocalQueue = "jobqueue"
		return workspace, nil
	}
	recreated := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer { return ensurer })
	recreated.SetOut(&bytes.Buffer{})
	recreated.SetErr(&bytes.Buffer{})
	recreated.SetArgs([]string{"--config", config, "--dry-run=client"})
	recreatedErr := recreated.Execute()
	if recreatedErr == nil || !strings.Contains(recreatedErr.Error(), "workspace UID") {
		t.Fatalf("recreated workspace error = %v", recreatedErr)
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
		{"placement", func(o *unresolvedRunOptions) { o.topology = profile.PlacementUnconstrained }, "policy.topology"},
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

func TestSelectedProfileAllowsBuiltInPriorityTier(t *testing.T) {
	provider := testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 2, 3)
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = "research-profile"
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	options.priorityTier = "priority"
	options.explicitPolicyFields = map[string]bool{"priority_tier": true}

	selected, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err != nil {
		t.Fatalf("selectRunWorkloadProfile: %v", err)
	}
	if selected.priorityTier != "priority" {
		t.Fatalf("priority tier = %q, want priority", selected.priorityTier)
	}
	if selected.workloadPriorityClass != "" || selected.podPriorityClass != "" {
		t.Fatalf(
			"explicit tier should defer class selection to topology: workload=%q pod=%q",
			selected.workloadPriorityClass,
			selected.podPriorityClass,
		)
	}
}

func TestSelectedProfilePriorityTierOverridesDisabledProfilePriorities(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 2, 3, 11)
	resolved.Priorities = profile.ProfilePriorities{DisableDefaultPriorities: true}
	resolved.WorkloadPriorityClasses = nil
	resolved.PodPriorityClasses = nil
	provider := testRunProviderForResolved(t, 11, resolved)

	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = "research-profile"
	options.namespace = "alpha"
	options.team = "research"
	options.lane = "training"
	options.priorityTier = "priority"
	options.explicitPolicyFields = map[string]bool{"priority_tier": true}

	selected, err := selectRunWorkloadProfile(context.Background(), options, provider)
	if err != nil {
		t.Fatalf("selectRunWorkloadProfile: %v", err)
	}
	if selected.disableDefaultPriorities {
		t.Fatal("explicit tier should enable TauGrid-managed priority classes")
	}
}

func TestSelectedProfileRejectsAmbiguousPriorityTier(t *testing.T) {
	provider := testRunProfileProvider(t, profile.ExecutionTargetSingleCluster, 2, 3)
	for _, test := range []struct {
		name   string
		mutate func(*unresolvedRunOptions)
	}{
		{"exact class", func(o *unresolvedRunOptions) { o.podPriorityClass = "tau-default" }},
		{"disabled priorities", func(o *unresolvedRunOptions) {
			o.disableDefaultPriorities, o.disablePrioritiesExplicit = true, true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := defaultRunDispatchOptions()
			options.engine = runconfig.EngineRayJob
			options.profileName = "research-profile"
			options.namespace = "alpha"
			options.team = "research"
			options.lane = "training"
			options.priorityTier = "priority"
			options.explicitPolicyFields = map[string]bool{"priority_tier": true}
			test.mutate(&options)

			_, err := selectRunWorkloadProfile(context.Background(), options, provider)
			if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("error = %v, want ambiguous priority rejection", err)
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

func TestRunSelectionUsesExplicitGPUCountToDisambiguate(t *testing.T) {
	cpu := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 0, 1, 11)
	cpu.Name = "cpu-profile"
	gpu := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 1, 1, 11)
	snapshot, err := profile.NewProfileSetSnapshot(11, []profile.ResolvedWorkloadProfile{cpu, gpu})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := profile.NewSnapshotProvider(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		gpus int
		want string
	}{{gpus: 0, want: "cpu-profile"}, {gpus: 1, want: "research-profile"}} {
		options := defaultRunDispatchOptions()
		options.engine = runconfig.EngineJob
		options.namespace = "alpha"
		options.team = "research"
		options.lane = "training"
		gpus := tc.gpus
		options.jobGPUs = &gpus
		options.jobGPUsExplicit = true
		got, err := selectRunWorkloadProfile(context.Background(), options, provider)
		if err != nil {
			t.Fatalf("select with gpus=%d: %v", tc.gpus, err)
		}
		if got.profileName != tc.want {
			t.Fatalf("select with gpus=%d = %q, want %q", tc.gpus, got.profileName, tc.want)
		}
	}
}

func TestRunSelectionAuthorizesReadyProfileForCustomWorkspaceNamespace(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 1, 1, 11)
	resolved.Name = "azure.research.training.l"
	resolved.Applicability.Namespaces = []string{"team-alpha"}
	resolved.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "gpu-cq",
	}}
	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = "azure.research.training.l"
	options.profileNameExplicit = true
	options.namespace = "team-alpha"
	options.team = "research"
	options.lane = "training"

	selected, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProviderForResolved(t, 11, resolved),
	)
	if err != nil {
		t.Fatalf("select profile for custom workspace namespace: %v", err)
	}
	if selected.selectedWorkloadProfile == nil ||
		selected.selectedWorkloadProfile.ClusterQueue != "gpu-cq" ||
		selected.queue != "jobqueue" {
		t.Fatalf("custom workspace selection = %#v", selected.selectedWorkloadProfile)
	}
}

func TestRunSelectionRejectsWorkspaceQueueThatChangesSingleClusterClusterQueue(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 2, 4, 11)
	resolved.Name = "azure.research.training.l"
	resolved.Applicability.Namespaces = []string{"team-alpha"}
	resolved.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "legacy-cq",
	}}
	workspace := readyWorkspace()
	workspace.Metadata.Name = "training"
	workspace.Spec.Target.Namespace = "team-alpha"
	workspace.Status.Target.ResolvedNamespace = "team-alpha"
	workspace.Status.Queue.LocalQueue = "tau-ws-training"
	workspace.Status.Queue.ClusterQueue = "tau-ws-training"

	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = resolved.Name
	options.profileNameExplicit = true
	options.team = "research"
	options.lane = "training"
	options, err := applyWorkspaceDefaults(options, workspace, "train")
	if err != nil {
		t.Fatalf("apply workspace defaults: %v", err)
	}
	selected, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProviderForResolved(t, 11, resolved),
	)
	if err == nil ||
		!strings.Contains(err.Error(), `cannot replace singleCluster workload profile`) ||
		!strings.Contains(err.Error(), `"legacy-cq"`) {
		t.Fatalf("selection = %#v, error = %v, want singleCluster ClusterQueue transition rejection", selected, err)
	}
}

func TestRunSelectionRejectsWorkspaceQueueThatChangesMultiKueueClusterQueue(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetMultiKueue, 2, 4, 11)
	resolved.Name = "azure.research.training.l"
	resolved.Applicability.Namespaces = []string{"team-alpha"}
	resolved.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "multikueue-cq",
	}}
	workspace := readyWorkspace()
	workspace.Metadata.Name = "training"
	workspace.Spec.Target.Namespace = "team-alpha"
	workspace.Status.Target.ResolvedNamespace = "team-alpha"
	workspace.Status.Queue.LocalQueue = "tau-ws-training"
	workspace.Status.Queue.ClusterQueue = "tau-ws-training"

	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = resolved.Name
	options.profileNameExplicit = true
	options.team = "research"
	options.lane = "training"
	options, err := applyWorkspaceDefaults(options, workspace, "train")
	if err != nil {
		t.Fatalf("apply workspace defaults: %v", err)
	}
	_, err = selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProviderForResolved(t, 11, resolved),
	)
	if err == nil ||
		!strings.Contains(err.Error(), `cannot replace multiKueue workload profile`) ||
		!strings.Contains(err.Error(), `"multikueue-cq"`) {
		t.Fatalf("error = %v, want MultiKueue ClusterQueue transition rejection", err)
	}
}

func TestRunSelectionAllowsRetainedWorkspaceQueueWithOriginalClusterQueue(t *testing.T) {
	for _, executionTarget := range []profile.ExecutionTarget{
		profile.ExecutionTargetSingleCluster,
		profile.ExecutionTargetMultiKueue,
	} {
		t.Run(string(executionTarget), func(t *testing.T) {
			resolved := testRunResolvedProfile(executionTarget, 2, 4, 11)
			resolved.Name = "azure.research.training.l"
			resolved.Applicability.Namespaces = []string{"team-alpha"}
			resolved.LocalQueues = []profile.ResolvedLocalQueue{{
				Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "legacy-cq",
			}}
			workspace := readyWorkspace()
			workspace.Metadata.Name = "training"
			workspace.Spec.TeamRef = &tauworkspace.ClusterObjectReference{Name: "research"}
			workspace.Spec.Queue = "jobqueue"
			workspace.Spec.Target.Namespace = "team-alpha"
			workspace.Status.Target.ResolvedNamespace = "team-alpha"
			workspace.Status.Queue.LocalQueue = "tau-ws-training"
			workspace.Status.Queue.ClusterQueue = "tau-ws-training"
			connection := workspaceconnection.ActiveConnection{
				Workspace:     "training",
				Namespace:     "team-alpha",
				Queue:         "jobqueue",
				ClusterQueue:  "legacy-cq",
				QueueVerified: true,
			}

			options := defaultRunDispatchOptions()
			options.engine = runconfig.EngineRayJob
			options.profileName = resolved.Name
			options.profileNameExplicit = true
			options.team = "research"
			options.lane = "training"
			options, err := applyWorkspaceDefaultsWithConnection(options, workspace, "train", connection)
			if err != nil {
				t.Fatalf("apply workspace defaults: %v", err)
			}
			selected, err := selectRunWorkloadProfile(
				context.Background(),
				options,
				testRunProviderForResolved(t, 11, resolved),
			)
			if err != nil {
				t.Fatalf("select profile with retained LocalQueue: %v", err)
			}
			if selected.queue != "jobqueue" ||
				selected.selectedWorkloadProfile == nil ||
				selected.selectedWorkloadProfile.ClusterQueue != "legacy-cq" {
				t.Fatalf("retained workspace selection = %#v", selected)
			}
		})
	}
}

func TestRunSelectionAllowsWorkspaceQueueThatPreservesMultiKueueClusterQueue(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetMultiKueue, 2, 4, 11)
	resolved.Name = "azure.research.training.l"
	resolved.Applicability.Namespaces = []string{"team-alpha"}
	resolved.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "multikueue-cq",
	}}
	workspace := readyWorkspace()
	workspace.Metadata.Name = "training"
	workspace.Spec.Target.Namespace = "team-alpha"
	workspace.Status.Target.ResolvedNamespace = "team-alpha"
	workspace.Status.Queue.LocalQueue = "workspace-jobqueue"
	workspace.Status.Queue.ClusterQueue = "multikueue-cq"

	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = resolved.Name
	options.profileNameExplicit = true
	options.team = "research"
	options.lane = "training"
	options, err := applyWorkspaceDefaults(options, workspace, "train")
	if err != nil {
		t.Fatalf("apply workspace defaults: %v", err)
	}
	selected, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProviderForResolved(t, 11, resolved),
	)
	if err != nil {
		t.Fatalf("select MultiKueue profile with preserved ClusterQueue: %v", err)
	}
	if selected.queue != "workspace-jobqueue" ||
		selected.selectedWorkloadProfile == nil ||
		selected.selectedWorkloadProfile.ClusterQueue != "multikueue-cq" {
		t.Fatalf("MultiKueue workspace selection = %#v", selected)
	}
}

func TestRunSelectionAllowsWorkspaceQueueThatPreservesSingleClusterClusterQueue(t *testing.T) {
	resolved := testRunResolvedProfile(profile.ExecutionTargetSingleCluster, 2, 4, 11)
	resolved.Name = "azure.research.training.l"
	resolved.Applicability.Namespaces = []string{"team-alpha"}
	resolved.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "team-alpha", Name: "jobqueue", ClusterQueue: "local-cq",
	}}
	workspace := readyWorkspace()
	workspace.Metadata.Name = "training"
	workspace.Spec.Target.Namespace = "team-alpha"
	workspace.Status.Target.ResolvedNamespace = "team-alpha"
	workspace.Status.Queue.LocalQueue = "workspace-jobqueue"
	workspace.Status.Queue.ClusterQueue = "local-cq"

	options := defaultRunDispatchOptions()
	options.engine = runconfig.EngineRayJob
	options.profileName = resolved.Name
	options.profileNameExplicit = true
	options.team = "research"
	options.lane = "training"
	options, err := applyWorkspaceDefaults(options, workspace, "train")
	if err != nil {
		t.Fatalf("apply workspace defaults: %v", err)
	}
	selected, err := selectRunWorkloadProfile(
		context.Background(),
		options,
		testRunProviderForResolved(t, 11, resolved),
	)
	if err != nil {
		t.Fatalf("select singleCluster profile with preserved ClusterQueue: %v", err)
	}
	if selected.queue != "workspace-jobqueue" ||
		selected.selectedWorkloadProfile == nil ||
		selected.selectedWorkloadProfile.ClusterQueue != "local-cq" {
		t.Fatalf("singleCluster workspace selection = %#v", selected)
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
	provider := profile.NewClusterProvider(readyClusterProfileClientForProfiles(t, 11, false, resolved))
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
	placement := profile.PlacementUnconstrained
	if workers > 1 {
		placement = profile.PlacementSameNetworkDomain
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
