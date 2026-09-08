// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

type connectedServeTestRunner struct {
	namespace    string
	queue        string
	clusterQueue string
	calls        [][]string
}

func (r *connectedServeTestRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	namespace := r.namespace
	if namespace == "" {
		namespace = "tau"
	}
	queue := r.queue
	if queue == "" {
		queue = "jobqueue"
	}
	clusterQueue := r.clusterQueue
	if clusterQueue == "" {
		clusterQueue = "gpu-cq"
	}
	switch {
	case len(args) >= 2 && args[0] == "auth" && args[1] == "can-i":
		return "yes\n", nil
	case len(args) >= 4 && args[0] == "-n" && args[2] == "get":
		return fmt.Sprintf(
			`{"metadata":{"name":%q},"spec":{"clusterQueue":%q}}`,
			queue,
			clusterQueue,
		), nil
	case len(args) > 0 && args[0] == "apply":
		return "applied\n", nil
	default:
		return "", fmt.Errorf("unexpected kubectl args: %s", strings.Join(args, " "))
	}
}

func newConnectedServeTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	profiles := []profile.ResolvedWorkloadProfile{
		serveTestProfile("model-serve", profile.ExecutionTargetSingleCluster, "jobqueue", 1, 1, 17),
		serveTestProfile("sample-project-stt-a100", profile.ExecutionTargetSingleCluster, "jobqueue", 1, 1, 17),
	}
	stubServeDependencies(t, &connectedServeTestRunner{}, readyClusterProfileClientForProfiles(t, 17, false, profiles...))
	return NewRoot()
}

func stubServeDependencies(t *testing.T, runner kubeRawRunner, client dynamic.Interface) {
	t.Helper()
	originalRunner := newServeRunner
	originalClient := newClusterProfileClient
	originalConnectionEnsurer := newServeConnectionEnsurer
	originalWorkspaceFetcher := fetchServeWorkspace
	namespace := "tau"
	queue := "jobqueue"
	clusterQueue := "gpu-cq"
	if connected, ok := runner.(*connectedServeTestRunner); ok {
		if connected.namespace != "" {
			namespace = connected.namespace
		}
		if connected.queue != "" {
			queue = connected.queue
		}
		if connected.clusterQueue != "" {
			clusterQueue = connected.clusterQueue
		}
	}
	connection := workspaceconnection.ActiveConnection{
		Workspace:    "sample",
		WorkspaceUID: "workspace-uid",
		ContextName:  "connected-context",
		Namespace:    namespace,
		Queue:        queue,
	}
	newServeRunner = func(string) kubeRawRunner { return runner }
	newClusterProfileClient = func(string) (dynamic.Interface, error) { return client, nil }
	newServeConnectionEnsurer = func(*cobra.Command) runConnectionEnsurer {
		return &fakeRunConnectionEnsurer{connection: connection}
	}
	fetchServeWorkspace = func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
		return tauworkspace.Workspace{
			Metadata: tauworkspace.ObjectMeta{
				Name:       connection.Workspace,
				UID:        connection.WorkspaceUID,
				Generation: 1,
			},
			Spec: tauworkspace.WorkspaceSpec{
				Target: tauworkspace.WorkspaceTarget{Namespace: namespace},
				Queue:  queue,
			},
			Status: tauworkspace.WorkspaceStatus{
				Phase:              "Ready",
				ObservedGeneration: 1,
				Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: namespace},
				Queue: tauworkspace.WorkspaceQueueStatus{
					LocalQueue:   queue,
					ClusterQueue: clusterQueue,
				},
			},
		}, nil
	}
	t.Cleanup(func() {
		newServeRunner = originalRunner
		newClusterProfileClient = originalClient
		newServeConnectionEnsurer = originalConnectionEnsurer
		fetchServeWorkspace = originalWorkspaceFetcher
	})
}

func serveTestProfile(
	name string,
	target profile.ExecutionTarget,
	queue string,
	gpus, workers int32,
	generation int64,
) profile.ResolvedWorkloadProfile {
	placement := profile.PlacementIndependent
	if workers > 1 {
		placement = profile.PlacementMultiNodeNCCL
	}
	return profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name:              name,
			GPUsPerWorker:     gpus,
			WorkerCount:       workers,
			Mode:              profile.ModeFixed,
			Placement:         placement,
			DefaultLocalQueue: queue,
			ExecutionTarget:   target,
			Priorities: profile.ProfilePriorities{
				WorkloadPriorityClassName: "tau-default",
				PodPriorityClassName:      "tau-default",
			},
		},
		LocalQueues: []profile.ResolvedLocalQueue{
			{Namespace: "alpha", Name: queue, ClusterQueue: "gpu-cq"},
			{Namespace: "tau", Name: queue, ClusterQueue: "gpu-cq"},
			{Namespace: "team-namespace", Name: queue, ClusterQueue: "gpu-cq"},
		},
		ClusterQueues:           []string{"gpu-cq"},
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

func executeAuthoritativeServe(t *testing.T, p profile.ResolvedWorkloadProfile, args ...string) (string, error) {
	t.Helper()
	runner := &connectedServeTestRunner{namespace: "alpha", queue: "jobqueue"}
	stubServeDependencies(t, runner, readyClusterProfileClientForProfiles(t, 23, false, p))
	cmd := NewRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"serve", "deploy"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func serveArgs(base []string, extras ...string) []string {
	out := append([]string{}, base...)
	return append(out, extras...)
}

func TestServeDeployRejectsAmbientContextConflict(t *testing.T) {
	t.Setenv(tauContextEnv, "ambient-context")
	root := newConnectedServeTestRoot(t)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"serve", "deploy", "endpoint",
		"--profile", "model-serve",
		"--image", "example.invalid/serve:v1",
		"--dry-run=client",
	})

	err := root.Execute()
	if err == nil ||
		!strings.Contains(err.Error(), `context "ambient-context" conflicts`) ||
		!strings.Contains(err.Error(), `connection context "connected-context"`) {
		t.Fatalf("ambient context conflict error = %v", err)
	}
}

func TestServeDeployAuthoritativeProfileContract(t *testing.T) {
	ordinary := serveTestProfile("serve-1gpu", profile.ExecutionTargetSingleCluster, "jobqueue", 1, 1, 23)
	ordinary.Applicability = profile.ProfileApplicability{
		Namespaces: []string{"alpha"}, Teams: []string{"research"}, Lanes: []string{"serving"},
	}
	base := []string{
		"endpoint", "--profile", "serve-1gpu", "--image", "example.invalid/serve:v1",
		"-n", "alpha", "--dry-run=client",
	}

	t.Run("ordinary profile render", func(t *testing.T) {
		rendered, err := executeAuthoritativeServe(t, ordinary, serveArgs(base, "--kind=deployment")...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"nvidia.com/gpu: 1", "kueue.x-k8s.io/queue-name: jobqueue"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("render missing %q:\n%s", want, rendered)
			}
		}
	})

	t.Run("explicit GPU conflict", func(t *testing.T) {
		_, err := executeAuthoritativeServe(t, ordinary, serveArgs(base, "--gpus", "2")...)
		if err == nil || !strings.Contains(err.Error(), "--gpus=2 conflicts") {
			t.Fatalf("GPU conflict error = %v", err)
		}
	})

	t.Run("queue mismatch", func(t *testing.T) {
		mismatch := ordinary
		mismatch.LocalQueues = append([]profile.ResolvedLocalQueue(nil), ordinary.LocalQueues...)
		mismatch.DefaultLocalQueue = "profile-queue"
		mismatch.LocalQueues[0].Name = "profile-queue"
		_, err := executeAuthoritativeServe(t, mismatch, base...)
		if err == nil || !strings.Contains(err.Error(), `LocalQueue "jobqueue" conflicts`) {
			t.Fatalf("queue mismatch error = %v", err)
		}
	})

	t.Run("ClusterQueue mismatch", func(t *testing.T) {
		runner := &connectedServeTestRunner{
			namespace:    "alpha",
			queue:        "jobqueue",
			clusterQueue: "other-cq",
		}
		stubServeDependencies(
			t,
			runner,
			readyClusterProfileClientForProfiles(t, 23, false, ordinary),
		)
		cmd := NewRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"serve", "deploy"}, base...))
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `points to ClusterQueue "other-cq"`) ||
			!strings.Contains(err.Error(), `expects "gpu-cq"`) {
			t.Fatalf("ClusterQueue mismatch error = %v", err)
		}
	})

	t.Run("ambiguous team applicability", func(t *testing.T) {
		denied := ordinary
		denied.Applicability.Teams = []string{"research", "experimental"}
		_, err := executeAuthoritativeServe(t, denied, base...)
		if err == nil || !strings.Contains(err.Error(), "authorizes multiple teams") {
			t.Fatalf("applicability error = %v", err)
		}
	})

	t.Run("another profile team in the same workspace", func(t *testing.T) {
		experimental := ordinary
		experimental.Applicability.Teams = []string{"experimental"}
		if _, err := executeAuthoritativeServe(t, experimental, base...); err != nil {
			t.Fatalf("experimental profile in the same workspace: %v", err)
		}
	})

	t.Run("stale provider", func(t *testing.T) {
		runner := &connectedServeTestRunner{namespace: "alpha", queue: "jobqueue"}
		stubServeDependencies(t, runner, readyClusterProfileClientForProfiles(t, 23, true, ordinary))
		cmd := NewRoot()
		cmd.SetArgs(append([]string{"serve", "deploy"}, base...))
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "workload profiles are stale") {
			t.Fatalf("stale provider error = %v", err)
		}
	})
}

func TestServeDeployMultiKueueAndRevisionMetadata(t *testing.T) {
	multiKueue := serveTestProfile("serve-multikueue", profile.ExecutionTargetMultiKueue, "jobqueue", 1, 1, 23)
	multiKueue.Applicability = profile.ProfileApplicability{
		Namespaces: []string{"alpha"}, Teams: []string{"research"}, Lanes: []string{"serving"},
	}
	base := []string{
		"endpoint", "--profile", "serve-multikueue", "--image", "example.invalid/serve:v1",
		"-n", "alpha",
	}

	for _, dryRun := range []string{"client", "server", ""} {
		name := dryRun
		if name == "" {
			name = "apply"
		}
		t.Run(name, func(t *testing.T) {
			args := serveArgs(base)
			if dryRun != "" {
				args = append(args, "--dry-run="+dryRun)
			}
			_, err := executeAuthoritativeServe(t, multiKueue, args...)
			if err != nil {
				t.Fatalf("multiKueue serve %s: %v", name, err)
			}
		})
	}

	for _, kind := range []string{"deployment", "rayservice"} {
		t.Run(kind+"-metadata", func(t *testing.T) {
			args := serveArgs(base, "--kind="+kind, "--dry-run=client")
			rendered, err := executeAuthoritativeServe(t, multiKueue, args...)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(strings.Split(rendered, "\n---\n")[0]), &doc); err != nil {
				t.Fatal(err)
			}
			rootAnnotations := nestedStringMap(t, doc, "metadata", "annotations")
			rootLabels := nestedStringMap(t, doc, "metadata", "labels")
			var podAnnotations map[string]string
			var podLabels map[string]string
			if kind == "deployment" {
				podAnnotations = nestedStringMap(t, doc, "spec", "template", "metadata", "annotations")
				podLabels = nestedStringMap(t, doc, "spec", "template", "metadata", "labels")
			} else {
				podAnnotations = nestedStringMap(t, doc, "spec", "rayClusterConfig", "headGroupSpec", "template", "metadata", "annotations")
				podLabels = nestedStringMap(t, doc, "spec", "rayClusterConfig", "headGroupSpec", "template", "metadata", "labels")
			}
			if rootLabels[workloadmeta.LabelWorkspace] != "sample" || podLabels[workloadmeta.LabelWorkspace] != "sample" {
				t.Fatalf("workspace metadata root=%q pod=%q:\n%s", rootLabels[workloadmeta.LabelWorkspace], podLabels[workloadmeta.LabelWorkspace], rendered)
			}
			for key, value := range map[string]string{
				workloadmeta.AnnotationTauClusterGeneration: strconv.FormatInt(23, 10),
				workloadmeta.AnnotationWorkloadProfileName:  "serve-multikueue",
			} {
				if rootAnnotations[key] != value || podAnnotations[key] != value {
					t.Fatalf("%s metadata root=%q pod=%q, want %q:\n%s", key, rootAnnotations[key], podAnnotations[key], value, rendered)
				}
			}
			hashKey := workloadmeta.AnnotationWorkloadProfileSetHash
			if rootAnnotations[hashKey] == "" || rootAnnotations[hashKey] != podAnnotations[hashKey] {
				t.Fatalf("profile-set hash root=%q pod=%q:\n%s", rootAnnotations[hashKey], podAnnotations[hashKey], rendered)
			}
		})
	}
}

func nestedStringMap(t *testing.T, root map[string]any, path ...string) map[string]string {
	t.Helper()
	var value any = root
	for _, key := range path {
		current, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("path %s is not a map: %#v", strings.Join(path, "."), value)
		}
		value = current[key]
	}
	raw, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("path %s is not a string map: %#v", strings.Join(path, "."), value)
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		out[key] = fmt.Sprint(value)
	}
	return out
}
