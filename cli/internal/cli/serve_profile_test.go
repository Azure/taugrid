// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
	case len(args) >= 2 && args[0] == "get" && args[1] == "namespaces":
		namespaces := []string{namespace, "tau", "team-namespace", "alpha"}
		seen := map[string]bool{}
		var items []map[string]any
		for _, ns := range namespaces {
			if seen[ns] {
				continue
			}
			seen[ns] = true
			items = append(items, map[string]any{"metadata": map[string]any{
				"name": ns,
				"labels": map[string]string{
					"kueue.x-k8s.io/default-local-queue": queue,
					"kueue.x-k8s.io/team":                "research",
				},
			}})
		}
		data, _ := json.Marshal(map[string]any{"items": items})
		return string(data), nil
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
	stubServeDependencies(t, &connectedServeTestRunner{}, serveTestClusterClient(t, 17, profiles, false))
	return NewRoot()
}

func stubServeDependencies(t *testing.T, runner kubeRawRunner, client dynamic.Interface) {
	t.Helper()
	originalRunner := newServeRunner
	originalClient := newClusterProfileClient
	newServeRunner = func(string) kubeRawRunner { return runner }
	newClusterProfileClient = func(string) (dynamic.Interface, error) { return client, nil }
	t.Cleanup(func() {
		newServeRunner = originalRunner
		newClusterProfileClient = originalClient
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

func serveTestClusterClient(
	t *testing.T,
	generation int64,
	profiles []profile.ResolvedWorkloadProfile,
	stale bool,
) dynamic.Interface {
	t.Helper()
	hash, err := profile.ProfileSetHash(profiles)
	if err != nil {
		t.Fatal(err)
	}
	observedGeneration := generation
	if stale {
		observedGeneration--
	}
	workloadProfiles, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile.ProfileSetStatus{
		ObservedGeneration: observedGeneration,
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
		Type:               profile.ConditionWorkloadProfilesReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "Ready",
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
			"conditions":       conditions["conditions"],
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(profile.TauClusterGVR).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return client
}

func executeAuthoritativeServe(t *testing.T, p profile.ResolvedWorkloadProfile, args ...string) (string, error) {
	t.Helper()
	runner := &connectedServeTestRunner{namespace: "alpha", queue: "jobqueue"}
	stubServeDependencies(t, runner, serveTestClusterClient(t, 23, []profile.ResolvedWorkloadProfile{p}, false))
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
			serveTestClusterClient(t, 23, []profile.ResolvedWorkloadProfile{ordinary}, false),
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

	t.Run("applicability denial", func(t *testing.T) {
		denied := ordinary
		denied.Applicability.Teams = []string{"other"}
		_, err := executeAuthoritativeServe(t, denied, base...)
		if err == nil || !strings.Contains(err.Error(), `does not authorize team "research"`) {
			t.Fatalf("applicability error = %v", err)
		}
	})

	t.Run("stale provider", func(t *testing.T) {
		runner := &connectedServeTestRunner{namespace: "alpha", queue: "jobqueue"}
		stubServeDependencies(t, runner, serveTestClusterClient(t, 23, []profile.ResolvedWorkloadProfile{ordinary}, true))
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
			var podAnnotations map[string]string
			if kind == "deployment" {
				podAnnotations = nestedStringMap(t, doc, "spec", "template", "metadata", "annotations")
			} else {
				podAnnotations = nestedStringMap(t, doc, "spec", "rayClusterConfig", "headGroupSpec", "template", "metadata", "annotations")
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
