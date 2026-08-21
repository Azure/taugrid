// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/queueresolve"
)

type serveQueueRunner struct {
	outputs map[string]string
	errors  map[string]error
	calls   [][]string
}

func (r *serveQueueRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	key := serveQueueKey(args...)
	if err := r.errors[key]; err != nil {
		return "", err
	}
	if out, ok := r.outputs[key]; ok {
		return out, nil
	}
	return "", errors.New("unexpected kubectl args: " + strings.Join(args, " "))
}

func serveQueueKey(args ...string) string {
	return strings.Join(args, "\x00")
}

func serveDeployRender(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newConnectedServeTestRoot(t)
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"serve", "deploy"}, args...))
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

// TestServeDeployStampsQueueOnPodTemplate is the regression guard for #1317.
// Client dry-run resolves the same authoritative queue and profile as apply.
func TestServeDeployStampsQueueOnPodTemplate(t *testing.T) {
	rendered, stderr, err := serveDeployRender(t,
		"h100-infer",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "example.invalid/infer:v1",
		"--gpus", "1",
		"-n", "team-namespace",
		"--dry-run=client",
	)
	if err != nil {
		t.Fatalf("serve deploy failed: %v\nstderr:\n%s", err, stderr)
	}
	podTemplateLabels := decodePodTemplateLabels(t, rendered)
	if podTemplateLabels["kueue.x-k8s.io/queue-name"] != "jobqueue" {
		t.Fatalf("pod template must carry the resolved queue or Kueue gates it forever: %v", podTemplateLabels)
	}
	if podTemplateLabels["kueue.x-k8s.io/managed"] != "true" {
		t.Fatalf("pod template lost the managed label: %v", podTemplateLabels)
	}
	if !strings.Contains(rendered, "kueue.x-k8s.io/pod-suspending-parent: deployment") {
		t.Fatalf("pod template lost the suspending-parent annotation:\n%s", rendered)
	}
	if stderr != "" {
		t.Fatalf("connected client dry-run emitted an unexpected warning:\n%s", stderr)
	}
}

func decodePodTemplateLabels(t *testing.T, rendered string) map[string]string {
	t.Helper()
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	for _, chunk := range strings.Split(rendered, "\n---\n") {
		if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
			t.Fatalf("decode rendered manifest: %v\n%s", err, chunk)
		}
		if doc.Kind == "Deployment" {
			return doc.Spec.Template.Metadata.Labels
		}
	}
	t.Fatalf("no Deployment in rendered output:\n%s", rendered)
	return nil
}

func TestServeDeployDoesNotExposeQueueFlag(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"serve", "deploy"})
	if err != nil {
		t.Fatalf("find serve deploy: %v", err)
	}
	if cmd.Flags().Lookup("queue") != nil {
		t.Fatal("the Kueue LocalQueue is a platform default, not a researcher-facing flag")
	}
	profileFlag := cmd.Flags().Lookup("profile")
	if profileFlag == nil || !strings.Contains(profileFlag.Usage, "TauCluster workload profile") {
		t.Fatalf("--profile help must describe the authoritative TauCluster profile: %#v", profileFlag)
	}
	for _, name := range []string{"team", "lane"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Fatalf("serve deploy must derive %s from the platform profile/namespace contract", name)
		}
	}
	if cmd.Flags().Lookup("acknowledge-beta-feature") != nil {
		t.Fatal("serve deploy exposes removed --acknowledge-beta-feature")
	}
}

func TestResolveServeTargetUsesKueueDefaultQueue(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("get", "namespaces", "-l", queueresolve.DefaultLocalQueueLabel, "-o", "json"): `{
			"items": [{"metadata": {"name": "team-namespace", "labels": {
				"kueue.x-k8s.io/default-local-queue": "operator-chosen-queue"
			}}}]
		}`,
		serveQueueKey("auth", "can-i", "create", "deployments.apps", "-n", "team-namespace"):        "yes\n",
		serveQueueKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "team-namespace"): "yes\n",
		serveQueueKey("-n", "team-namespace", "get", "localqueue.kueue.x-k8s.io", "operator-chosen-queue", "-o", "json"): `{
			"metadata": {"name": "operator-chosen-queue"},
			"spec": {"clusterQueue": "shared-cq"}
		}`,
	}}

	target, warning, err := resolveServeTarget(context.Background(), runner, "", "deployments.apps")
	if err != nil {
		t.Fatalf("resolveServeTarget: %v", err)
	}
	if warning != "" {
		t.Fatalf("live resolution should not warn, got %q", warning)
	}
	if target.Namespace != "team-namespace" || target.Queue != "operator-chosen-queue" {
		t.Fatalf("target = %+v, want the namespace's default LocalQueue", target)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("resolution should verify namespace, RBAC, and LocalQueue; calls=%v", runner.calls)
	}
}

func TestResolveServeTargetUsesNamespaceOnlyToDisambiguate(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("get", "namespaces", "-l", queueresolve.DefaultLocalQueueLabel, "-o", "json"): `{
			"items": [
				{"metadata": {"name": "team-a", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}},
				{"metadata": {"name": "team-b", "labels": {"kueue.x-k8s.io/default-local-queue": "serve-queue"}}}
			]
		}`,
		serveQueueKey("auth", "can-i", "create", "rayservices.ray.io", "-n", "team-b"):      "yes\n",
		serveQueueKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "team-b"): "yes\n",
		serveQueueKey("-n", "team-b", "get", "localqueue.kueue.x-k8s.io", "serve-queue", "-o", "json"): `{
			"metadata": {"name": "serve-queue"},
			"spec": {"clusterQueue": "shared-cq"}
		}`,
	}}

	target, _, err := resolveServeTarget(context.Background(), runner, "team-b", "rayservices.ray.io")
	if err != nil {
		t.Fatalf("resolveServeTarget: %v", err)
	}
	if target.Namespace != "team-b" || target.Queue != "serve-queue" {
		t.Fatalf("target = %+v, want team-b's platform default", target)
	}
}

func TestResolveServeTargetRequiresConnectedRunner(t *testing.T) {
	_, _, err := resolveServeTarget(context.Background(), nil, "", "deployments.apps")
	if err == nil || !strings.Contains(err.Error(), "Kubernetes runner is required") {
		t.Fatalf("connected serving resolution error = %v", err)
	}
}

func TestResolveServeTargetFailsWhenKueueHasNoDefault(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("get", "namespaces", "-l", queueresolve.DefaultLocalQueueLabel, "-o", "json"): `{"items":[]}`,
	}}

	_, _, err := resolveServeTarget(context.Background(), runner, "", "deployments.apps")
	if err == nil {
		t.Fatal("missing platform default must fail before rendering a permanently gated workload")
	}
	if !strings.Contains(err.Error(), queueresolve.DefaultLocalQueueLabel) {
		t.Fatalf("error should identify the missing platform configuration: %v", err)
	}
	if strings.Contains(err.Error(), "--queue") {
		t.Fatalf("error must not ask the researcher to choose platform queue policy: %v", err)
	}
}

func TestResolveServeTargetRejectsNamespaceWithoutKueueDefault(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("get", "namespaces", "-l", queueresolve.DefaultLocalQueueLabel, "-o", "json"): `{
			"items": [{"metadata": {"name": "other-team", "labels": {
				"kueue.x-k8s.io/default-local-queue": "jobqueue"
			}}}]
		}`,
	}}

	_, _, err := resolveServeTarget(context.Background(), runner, "unconfigured-team", "deployments.apps")
	if err == nil {
		t.Fatal("an explicit namespace without a platform default must fail")
	}
	for _, want := range []string{"unconfigured-team", queueresolve.DefaultLocalQueueLabel, "platform owner"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}
