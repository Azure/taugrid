package queueresolve

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolveAccessibleQueueSelectsOnlyAuthorizedNamespace(t *testing.T) {
	runner := &validationFakeRunner{outputs: map[string]string{
		validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [
    {"metadata": {"name": "taugrid-team", "labels": {"kueue.x-k8s.io/team": "research", "kueue.x-k8s.io/default-local-queue": "gpu"}}},
    {"metadata": {"name": "taugrid-team-test", "labels": {"kueue.x-k8s.io/team": "experimental", "kueue.x-k8s.io/default-local-queue": "gpu"}}}
  ]
}`,
		validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "taugrid-team"):               "yes\n",
		validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "taugrid-team"):      "yes\n",
		validationKey("-n", "taugrid-team", "get", "localqueue.kueue.x-k8s.io", "gpu", "-o", "json"):   localQueueObject("gpu", "taugrid-cq", nil),
		validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "taugrid-team-test"):          "no\n",
		validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "taugrid-team-test"): "yes\n",
	}}

	got, candidates, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
	if err != nil {
		t.Fatalf("ResolveAccessibleQueue: %v", err)
	}
	if got.Namespace != "taugrid-team" || got.QueueName != "gpu" {
		t.Fatalf("queue = %+v, candidates=%+v", got, candidates)
	}
}

func TestResolveAccessibleQueueRequiresDisambiguation(t *testing.T) {
	runner := &validationFakeRunner{outputs: map[string]string{
		validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [
    {"metadata": {"name": "taugrid-team", "labels": {"kueue.x-k8s.io/team": "research", "kueue.x-k8s.io/default-local-queue": "gpu"}}},
    {"metadata": {"name": "taugrid-team-test", "labels": {"kueue.x-k8s.io/team": "experimental", "kueue.x-k8s.io/default-local-queue": "gpu"}}}
  ]
}`,
		validationKey("auth", "can-i", "create", "jobs.batch", "-n", "taugrid-team"):                      "yes\n",
		validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "taugrid-team"):         "yes\n",
		validationKey("-n", "taugrid-team", "get", "localqueue.kueue.x-k8s.io", "gpu", "-o", "json"):      localQueueObject("gpu", "taugrid-cq", nil),
		validationKey("auth", "can-i", "create", "jobs.batch", "-n", "taugrid-team-test"):                 "yes\n",
		validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "taugrid-team-test"):    "yes\n",
		validationKey("-n", "taugrid-team-test", "get", "localqueue.kueue.x-k8s.io", "gpu", "-o", "json"): localQueueObject("gpu", "taugrid-cq", nil),
	}}

	_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{WorkloadResource: "jobs.batch"})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	got, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{Team: "experimental", WorkloadResource: "jobs.batch"})
	if err != nil {
		t.Fatalf("team-filtered ResolveAccessibleQueue: %v", err)
	}
	if got.Namespace != "taugrid-team-test" {
		t.Fatalf("namespace=%q want taugrid-team-test", got.Namespace)
	}
}

func TestResolveAccessibleQueueUsesExplicitNamespaceDefault(t *testing.T) {
	runner := &validationFakeRunner{outputs: map[string]string{
		validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [
    {"metadata": {"name": "team-a", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}},
    {"metadata": {"name": "team-b", "labels": {"kueue.x-k8s.io/default-local-queue": "serve-queue"}}}
  ]
}`,
		validationKey("auth", "can-i", "create", "deployments.apps", "-n", "team-b"):                   "yes\n",
		validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "team-b"):            "yes\n",
		validationKey("-n", "team-b", "get", "localqueue.kueue.x-k8s.io", "serve-queue", "-o", "json"): localQueueObject("serve-queue", "shared-cq", nil),
	}}

	got, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{
		Namespace:        "team-b",
		WorkloadResource: "deployments.apps",
	})
	if err != nil {
		t.Fatalf("ResolveAccessibleQueue: %v", err)
	}
	if got.Namespace != "team-b" || got.QueueName != "serve-queue" {
		t.Fatalf("queue = %+v, want team-b/serve-queue", got)
	}
}

func TestResolveAccessibleQueueRejectsNamespaceWithoutDefault(t *testing.T) {
	runner := &validationFakeRunner{outputs: map[string]string{
		validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "team-a", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}}]
}`,
	}}

	_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{
		Namespace: "team-b",
	})
	requireErrContains(t, err, "team-b", DefaultLocalQueueLabel, "platform owner")
}

func TestResolveAccessibleQueueNamesTheMissingOnboardingStep(t *testing.T) {
	t.Run("cluster never onboarded", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{"items": []}`,
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
		requireErrContains(t, err,
			"no namespace carries the "+DefaultLocalQueueLabel+" label",
			"TauWorkspace")
	})

	t.Run("rbac denied", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "research", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}}]
}`,
			validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "research"): "no\n",
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
		requireErrContains(t, err,
			"research: not authorized to create rayjobs.ray.io (RBAC)",
			"TauWorkspace subject")
	})

	t.Run("workspace not reconciled", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "research", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}}]
}`,
			validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "research"):          "yes\n",
			validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "research"): "yes\n",
		}, errors: map[string]error{
			validationKey("-n", "research", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): errors.New(`Error from server (NotFound): localqueues.kueue.x-k8s.io "jobqueue" not found`),
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
		requireErrContains(t, err,
			`research: LocalQueue "jobqueue" not found`,
			"kubectl get workspaces.tau.azure.com -n tau-platform")
	})

	t.Run("authorization check failed", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "research", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}}]
}`,
		}, errors: map[string]error{
			validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "research"): errors.New("API server unavailable"),
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
		requireErrContains(t, err,
			"authorization check for create rayjobs.ray.io failed: API server unavailable")
		if strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("transport failure must not be reported as an RBAC denial:\n%s", err)
		}
	})

	t.Run("localqueue read failed", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "research", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue"}}}]
}`,
			validationKey("auth", "can-i", "create", "rayjobs.ray.io", "-n", "research"):          "yes\n",
			validationKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "research"): "yes\n",
		}, errors: map[string]error{
			validationKey("-n", "research", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): errors.New("InternalError: API server unavailable"),
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{})
		requireErrContains(t, err,
			`cannot read LocalQueue "jobqueue": InternalError: API server unavailable`)
		if strings.Contains(err.Error(), "not found") {
			t.Fatalf("API failure must not be reported as a missing LocalQueue:\n%s", err)
		}
	})

	t.Run("filtered out by team", func(t *testing.T) {
		runner := &validationFakeRunner{outputs: map[string]string{
			validationKey("get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"): `{
  "items": [{"metadata": {"name": "research", "labels": {"kueue.x-k8s.io/default-local-queue": "jobqueue", "kueue.x-k8s.io/team": "physics"}}}]
}`,
		}}
		_, _, err := ResolveAccessibleQueue(context.Background(), runner, ResolveAccessibleQueueOptions{Team: "chem"})
		requireErrContains(t, err, `none match --team "chem"`, QueueTeamLabel)
	})
}

func requireErrContains(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error missing %q\ngot:\n%s", w, err)
		}
	}
}
