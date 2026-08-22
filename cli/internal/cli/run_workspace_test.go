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

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/experiment"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func workspaceDirectRunOptions(engine, script, image, profile, dryRun string, workers int, jobGPUs *int) runDispatchOptions {
	o := defaultRunDispatchOptions()
	o.engine = engine
	o.script = script
	o.image = image
	o.profileName = profile
	o.dryRun = dryRun
	o.workers = workers
	o.jobGPUs = jobGPUs
	return o
}

func TestApplyWorkspaceDefaultsFillsPolicyFields(t *testing.T) {
	o := defaultRunDispatchOptions()
	got, err := applyWorkspaceDefaults(o, readyWorkspace(), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.namespace != "sample" || got.queue != "sample" || got.priorityTier != "default" {
		t.Fatalf("policy defaults = namespace %q queue %q priority %q", got.namespace, got.queue, got.priorityTier)
	}
	if got.serviceAccountName != "tau-workload" {
		t.Fatalf("service account default = %q, want tau-workload", got.serviceAccountName)
	}
	if !got.azureWorkloadIdentity {
		t.Fatal("workspace workload identity should enable the AKS webhook label")
	}
	if got.workspace != "sample" || got.workspaceResultScope != "/data/projects/sample/runs" {
		t.Fatalf("workspace metadata = workspace %q result scope %q", got.workspace, got.workspaceResultScope)
	}
	if got.output != "" {
		t.Fatalf("output should not be defaulted without durable PVC/mount: %q", got.output)
	}
	if len(got.env) != 0 {
		t.Fatalf("workspace defaults should not inject reserved TAU_* env vars: %#v", got.env)
	}
	if got.experiment.Workspace != "sample" {
		t.Fatalf("experiment workspace = %q, want sample", got.experiment.Workspace)
	}
}

func TestApplyWorkspaceDefaultsPreservesAutoQueueForDiscovery(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.queue = "auto"
	got, err := applyWorkspaceDefaults(o, readyWorkspace(), "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if got.queue != "auto" {
		t.Fatalf("queue = %q, want auto sentinel", got.queue)
	}
	if got.workspaceQueueResolved {
		t.Fatal("auto queue must not be marked as the authoritative workspace queue")
	}
}

func TestApplyWorkspaceDefaultsUsesWorkspaceNameAsDefaultNamespace(t *testing.T) {
	w := readyWorkspace()
	w.Spec.Target.Namespace = ""
	w.Status.Target.ResolvedNamespace = ""

	got, err := applyWorkspaceDefaults(defaultRunDispatchOptions(), w, "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.namespace != w.Metadata.Name {
		t.Fatalf("namespace = %q, want workspace name %q", got.namespace, w.Metadata.Name)
	}
}

func TestApplyWorkspaceDefaultsSetsOutputOnlyWithDurableStorage(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.dataPVC = "blob-training"
	got, err := applyWorkspaceDefaults(o, readyWorkspace(), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}

	if got.output != "/data/projects/sample/runs/smoke" {
		t.Fatalf("output = %q", got.output)
	}
}

func TestApplyWorkspaceDefaultsSetsOutputForInferredRayJob(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.dataPVC = "blob-training"
	o.workers = 2
	workspace := readyWorkspace()
	workspace.Spec.Defaults.OutputRoot += "/"
	got, err := applyWorkspaceDefaults(o, workspace, "inferred-ray")
	if err != nil {
		t.Fatal(err)
	}
	if got.output != "/data/projects/sample/runs/inferred-ray" {
		t.Fatalf("inferred Ray output = %q", got.output)
	}
	if got.workspaceResultScope != "/data/projects/sample/runs/" {
		t.Fatalf("workspace result scope = %q", got.workspaceResultScope)
	}
}

func TestApplyWorkspaceDefaultsPreservesExplicitRunValues(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.priorityTier = "priority"
	o.output = "/data/projects/sample/runs/custom-output"
	o.serviceAccountName = "custom-workload"
	o.env = []string{"TAU_WORKSPACE=manual"}
	got, err := applyWorkspaceDefaults(o, readyWorkspace(), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}

	if got.namespace != "sample" || got.queue != "sample" || got.priorityTier != "priority" || got.output != "/data/projects/sample/runs/custom-output" {
		t.Fatalf("explicit values were overwritten: %#v", got)
	}
	if !containsString(got.env, "TAU_WORKSPACE=manual") {
		t.Fatalf("explicit env values should be preserved for normal validation: %#v", got.env)
	}
	if got.serviceAccountName != "custom-workload" {
		t.Fatalf("explicit service account was overwritten: %q", got.serviceAccountName)
	}
	if !got.azureWorkloadIdentity {
		t.Fatal("workspace workload identity should remain enabled with an explicit ServiceAccount override")
	}
}

func TestApplyWorkspaceDefaultsRejectsForeignOutputScope(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.engine = "ray"
	o.dataPVC = "research-workspace"
	o.output = "/data/another-workspace/run"
	_, err := applyWorkspaceDefaults(o, readyWorkspace(), "foreign-output")
	if err == nil || !strings.Contains(err.Error(), "outside TauWorkspace") {
		t.Fatalf("foreign output error = %v", err)
	}
}

func TestWorkspaceServiceAccountRendersDirectJobAndRayJobPods(t *testing.T) {
	zeroGPUs := 0
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.Mkdir(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "workspace-test.yaml"), []byte(`apiVersion: tau.azure.com/v1alpha1
kind: Profile
metadata:
  name: workspace-test
spec:
  queue: { localQueue: sample }
  resources:
    requests: { cpu: "1", memory: 1Gi }
    gpu: { count: 1, placement: single-device }
  runtime:
    image: busybox:1.36
`), 0o644); err != nil {
		t.Fatal(err)
	}

	jobScript := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(jobScript, []byte("#!/bin/sh\ntrue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rayScript := filepath.Join(dir, "train.py")
	if err := os.WriteFile(rayScript, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		opts         runDispatchOptions
		kind         string
		wantPodCount int
	}{
		{
			name:         "job",
			opts:         workspaceDirectRunOptions("job", jobScript, "busybox:1.36", "workspace-test", "client", 1, &zeroGPUs),
			kind:         "kind: Job",
			wantPodCount: 1,
		},
		{
			name:         "ray",
			opts:         workspaceDirectRunOptions("rayjob", rayScript, "example.com/research/ray:cuda13", "workspace-test", "client", 2, nil),
			kind:         "kind: RayJob",
			wantPodCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyWorkspaceDefaults(tt.opts, readyWorkspace(), "identity-render")
			if err != nil {
				t.Fatalf("applyWorkspaceDefaults: %v", err)
			}
			attachAuthoritativeProfileForTest(&got)
			if tt.name == "job" {
				setAuthoritativeProfileCardinalityForTest(&got, 0, 1)
			} else {
				setAuthoritativeProfileCardinalityForTest(&got, 1, 2)
			}
			target, err := resolveRunTarget(got, "identity-render")
			if err != nil {
				t.Fatalf("resolveRunTarget: %v", err)
			}
			parent := &cobra.Command{Use: "run"}
			var out, stderr bytes.Buffer
			parent.SetContext(context.Background())
			parent.SetOut(&out)
			parent.SetErr(&stderr)
			if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
				t.Fatalf("execute direct %s target: %v\nstderr:\n%s", tt.name, err, stderr.String())
			}
			rendered := out.String()
			if !strings.Contains(rendered, tt.kind) {
				t.Fatalf("direct %s output missing %q:\n%s", tt.name, tt.kind, rendered)
			}
			if count := strings.Count(rendered, "serviceAccountName: tau-workload"); count != tt.wantPodCount {
				t.Fatalf("direct %s serviceAccountName count=%d want %d:\n%s", tt.name, count, tt.wantPodCount, rendered)
			}
			if count := strings.Count(rendered, "azure.workload.identity/use: \"true\""); count != tt.wantPodCount {
				t.Fatalf("direct %s workload identity label count=%d want %d:\n%s", tt.name, count, tt.wantPodCount, rendered)
			}
		})
	}
}

func TestWorkspaceWithoutWorkloadIdentityDoesNotEnableWebhook(t *testing.T) {
	for _, workloadIdentity := range []*tauworkspace.WorkspaceWorkloadIdentity{
		nil,
		{},
	} {
		w := readyWorkspace()
		w.Spec.WorkloadIdentity = workloadIdentity
		got, err := applyWorkspaceDefaults(defaultRunDispatchOptions(), w, "no-identity")
		if err != nil {
			t.Fatalf("applyWorkspaceDefaults: %v", err)
		}
		if got.serviceAccountName != "" || got.azureWorkloadIdentity {
			t.Fatalf("workspace without workload identity ServiceAccount enabled pod identity: %#v", got)
		}
	}
}

func TestApplyWorkspaceDefaultsRejectsRoutingOutsideWorkspace(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runDispatchOptions)
		want   string
	}{
		{
			name: "namespace",
			mutate: func(o *runDispatchOptions) {
				o.namespace = "other"
			},
			want: `namespace "other" conflicts`,
		},
		{
			name: "queue",
			mutate: func(o *runDispatchOptions) {
				o.queue = "other"
			},
			want: `queue "other" conflicts`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := defaultRunDispatchOptions()
			tt.mutate(&o)
			_, err := applyWorkspaceDefaults(o, readyWorkspace(), "smoke")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("applyWorkspaceDefaults() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWorkspaceRunDispatchStampsJobAndRayJobAdmissionMetadata(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		engine       string
		name         string
		wantKind     string
		wantWorkload string
	}{
		{engine: "job", name: "workspace-job", wantKind: "Job", wantWorkload: experiment.WorkloadKindJob},
		{engine: "rayjob", name: "workspace-ray", wantKind: "RayJob", wantWorkload: experiment.WorkloadKindRayJob},
	}
	for _, tt := range tests {
		t.Run(tt.engine, func(t *testing.T) {
			options := defaultRunDispatchOptions()
			options.engine = tt.engine
			options.script = script
			options.image = "busybox:1.36"
			options.dryRun = "client"
			if tt.engine == "job" {
				options.profileName = "workspace.training"
			} else {
				options.profileName = "azure.research.training.l"
			}

			workspace := readyWorkspace()
			workspace.Spec.Queue = "workspace-jobqueue"
			workspace.Status.Queue.LocalQueue = "workspace-jobqueue"
			options, err := applyWorkspaceDefaults(options, workspace, tt.name)
			if err != nil {
				t.Fatalf("applyWorkspaceDefaults: %v", err)
			}
			attachAuthoritativeProfileForTest(&options)
			target, err := resolveRunTarget(options, tt.name)
			if err != nil {
				t.Fatalf("resolveRunTarget: %v", err)
			}
			parent := &cobra.Command{Use: "run"}
			parent.SetContext(context.Background())
			var out, stderr bytes.Buffer
			parent.SetOut(&out)
			parent.SetErr(&stderr)
			if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
				t.Fatalf("executeRunTarget: %v\nstderr:\n%s", err, stderr.String())
			}

			var rendered struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Labels      map[string]string `yaml:"labels"`
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Template struct {
						Metadata struct {
							Annotations map[string]string `yaml:"annotations"`
						} `yaml:"metadata"`
						Spec struct {
							NodeSelector map[string]string `yaml:"nodeSelector"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(out.Bytes(), &rendered); err != nil {
				t.Fatalf("parse rendered workload: %v\n%s", err, out.String())
			}
			if rendered.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", rendered.Kind, tt.wantKind)
			}
			if rendered.Metadata.Labels[workloadmeta.LabelManagedBy] != "tau" ||
				rendered.Metadata.Labels[experiment.LabelRunID] != tt.name ||
				rendered.Metadata.Labels[experiment.LabelWorkloadKind] != tt.wantWorkload ||
				rendered.Metadata.Labels[workloadmeta.LabelWorkspace] != "sample" ||
				rendered.Metadata.Labels["kueue.x-k8s.io/queue-name"] != "workspace-jobqueue" {
				t.Fatalf("admission labels = %#v", rendered.Metadata.Labels)
			}
			if rendered.Metadata.Annotations[experiment.AnnotationWorkspaceID] != "sample" ||
				rendered.Metadata.Annotations[experiment.AnnotationResultScope] != "/data/projects/sample/runs" {
				t.Fatalf("workspace annotations = %#v", rendered.Metadata.Annotations)
			}
			for _, stale := range []string{workloadmeta.AnnotationClusterQueue, workloadmeta.AnnotationResourceFlavor} {
				if _, ok := rendered.Metadata.Annotations[stale]; ok {
					t.Fatalf("workspace workload retained stale preset annotation %q: %#v", stale, rendered.Metadata.Annotations)
				}
			}
			if tt.engine == "job" {
				if strings.Count(out.String(), "nvidia.com/gpu: 1") != 2 {
					t.Fatalf("workspace Job must request and limit the profile GPU cardinality:\n%s", out.String())
				}
				if _, ok := rendered.Spec.Template.Spec.NodeSelector[runtopology.ManagedGPUSeriesLabel]; ok {
					t.Fatalf("workspace Job retained preset flavor selector: %#v", rendered.Spec.Template.Spec.NodeSelector)
				}
				if v := rendered.Spec.Template.Metadata.Annotations["kueue.x-k8s.io/podset-unconstrained-topology"]; v != "true" {
					t.Fatalf("workspace Job must preserve authoritative independent placement, got %q; annotations: %#v", v, rendered.Spec.Template.Metadata.Annotations)
				}
			} else if !strings.Contains(out.String(), `kueue.x-k8s.io/podset-unconstrained-topology: "true"`) {
				t.Fatalf("workspace RayJob must preserve TAS topology annotation from preset:\n%s", out.String())
			}
		})
	}
}

func TestApplyWorkspaceDefaultsRejectsDegradedWorkspace(t *testing.T) {
	w := readyWorkspace()
	w.Status.Phase = "Degraded"
	_, err := applyWorkspaceDefaults(defaultRunDispatchOptions(), w, "smoke")
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("expected not Ready error, got %v", err)
	}
}

func TestApplyWorkspaceDefaultsRejectsStaleReadyWorkspace(t *testing.T) {
	w := readyWorkspace()
	w.Metadata.Generation++
	_, err := applyWorkspaceDefaults(defaultRunDispatchOptions(), w, "smoke")
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("expected stale Ready error, got %v", err)
	}
}

func readyWorkspace() tauworkspace.Workspace {
	return tauworkspace.Workspace{
		Metadata: tauworkspace.ObjectMeta{Name: "sample", Namespace: tauworkspace.SystemNamespace, Generation: 3},
		Spec: tauworkspace.WorkspaceSpec{
			Queue:  "sample",
			Target: tauworkspace.WorkspaceTarget{Namespace: "sample"},
			Defaults: tauworkspace.WorkspaceDefaults{
				OutputRoot:   "/data/projects/sample/runs",
				ScratchMount: "/mnt",
				Priority:     "normal",
			},
			WorkloadIdentity: &tauworkspace.WorkspaceWorkloadIdentity{
				ServiceAccountName: "tau-workload",
			},
		},
		Status: tauworkspace.WorkspaceStatus{
			Phase:              "Ready",
			ObservedGeneration: 3,
			Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: "sample"},
			Queue:              tauworkspace.WorkspaceQueueStatus{LocalQueue: "sample"},
		},
	}
}

func containsArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
