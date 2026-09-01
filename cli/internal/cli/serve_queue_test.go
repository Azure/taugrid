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

	"github.com/Azure/taugrid/core/workloadmeta"
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
	if podTemplateLabels[workloadmeta.LabelWorkspace] != "sample" {
		t.Fatalf("pod template lost the active workspace label: %v", podTemplateLabels)
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
			t.Fatalf("serve deploy must derive %s from the selected workload profile", name)
		}
	}
}

func TestServeDeployRejectsNamespaceOutsideActiveWorkspace(t *testing.T) {
	_, _, err := serveDeployRender(
		t,
		"endpoint",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "example.invalid/infer:v1",
		"--namespace", "other-workspace",
		"--dry-run=client",
	)
	if err == nil || !strings.Contains(err.Error(), `conflicts with TauWorkspace "sample" target namespace "tau"`) {
		t.Fatalf("namespace conflict error = %v", err)
	}
}

func TestResolveServeTargetUsesWorkspaceQueue(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("auth", "can-i", "create", "deployments.apps", "-n", "team-namespace"):        "yes\n",
		serveQueueKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "team-namespace"): "yes\n",
		serveQueueKey("-n", "team-namespace", "get", "localqueue.kueue.x-k8s.io", "operator-chosen-queue", "-o", "json"): `{
			"metadata": {"name": "operator-chosen-queue"},
			"spec": {"clusterQueue": "shared-cq"}
		}`,
	}}

	target, err := resolveServeTarget(
		context.Background(),
		runner,
		"team-namespace",
		"operator-chosen-queue",
		"shared-cq",
		"deployments.apps",
	)
	if err != nil {
		t.Fatalf("resolveServeTarget: %v", err)
	}
	if target.Namespace != "team-namespace" || target.Queue != "operator-chosen-queue" {
		t.Fatalf("target = %+v, want the workspace LocalQueue", target)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("resolution should verify RBAC and the exact LocalQueue without listing namespaces; calls=%v", runner.calls)
	}
	for _, call := range runner.calls {
		if len(call) >= 2 && call[0] == "get" && call[1] == "namespaces" {
			t.Fatalf("workspace routing must not discover namespaces from labels: %v", runner.calls)
		}
	}
}

func TestResolveServeTargetRejectsWorkspaceClusterQueueMismatch(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("auth", "can-i", "create", "rayservices.ray.io", "-n", "team-b"):      "yes\n",
		serveQueueKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "team-b"): "yes\n",
		serveQueueKey("-n", "team-b", "get", "localqueue.kueue.x-k8s.io", "serve-queue", "-o", "json"): `{
			"metadata": {"name": "serve-queue"},
			"spec": {"clusterQueue": "shared-cq"}
		}`,
	}}

	_, err := resolveServeTarget(
		context.Background(),
		runner,
		"team-b",
		"serve-queue",
		"workspace-cq",
		"rayservices.ray.io",
	)
	if err == nil || !strings.Contains(err.Error(), `expects LocalQueue "serve-queue" to use ClusterQueue "workspace-cq"`) {
		t.Fatalf("ClusterQueue mismatch error = %v", err)
	}
}

func TestResolveServeTargetRequiresConnectedRunner(t *testing.T) {
	_, err := resolveServeTarget(context.Background(), nil, "tau", "jobqueue", "", "deployments.apps")
	if err == nil || !strings.Contains(err.Error(), "Kubernetes runner is required") {
		t.Fatalf("connected serving resolution error = %v", err)
	}
}

func TestResolveServeTargetRequiresWorkspaceQueue(t *testing.T) {
	_, err := resolveServeTarget(context.Background(), &serveQueueRunner{}, "tau", "", "", "deployments.apps")
	if err == nil {
		t.Fatal("missing workspace LocalQueue must fail before rendering")
	}
	if !strings.Contains(err.Error(), "workspace LocalQueue is required") {
		t.Fatalf("error should identify missing workspace placement: %v", err)
	}
}

func TestResolveServeTargetRejectsServingRBACDenial(t *testing.T) {
	runner := &serveQueueRunner{outputs: map[string]string{
		serveQueueKey("auth", "can-i", "create", "deployments.apps", "-n", "team-namespace"): "no\n",
	}}

	_, err := resolveServeTarget(
		context.Background(),
		runner,
		"team-namespace",
		"jobqueue",
		"",
		"deployments.apps",
	)
	if err == nil {
		t.Fatal("serving RBAC denial must fail")
	}
	for _, want := range []string{"not authorized", "deployments.apps", "team-namespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}
