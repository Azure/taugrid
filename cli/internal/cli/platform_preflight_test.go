package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/platform"
)

const platformPreflightJobManifest = `
apiVersion: batch/v1
kind: Job
metadata:
  name: demo-job
  namespace: ray
spec:
  template:
    spec:
      serviceAccountName: tau-workload
      imagePullSecrets:
        - name: acr-secret
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: blob-training
      containers:
        - name: main
          image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          env:
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
`

const platformPreflightJobManifestWithoutNamespace = `
apiVersion: batch/v1
kind: Job
metadata:
  name: demo-job
spec:
  template:
    spec:
      serviceAccountName: tau-workload
      containers:
        - name: main
          image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          env:
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
`

func healthyPlatformPreflightRunner(namespace string) *fakeKubeRawRunner {
	return &fakeKubeRawRunner{responses: map[string]string{
		"-n " + namespace + " get serviceaccount tau-workload -o name": "serviceaccount/tau-workload",
		"-n " + namespace + " get secret acr-secret -o name":           "secret/acr-secret",
		"-n " + namespace + " get secret hf-token -o name":             "secret/hf-token",
		"-n " + namespace + " get pvc blob-training -o json":           `{"spec":{"storageClassName":"blob-premium-rwx"},"status":{"phase":"Bound"}}`,
		"get storageclass blob-premium-rwx -o name":                    "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
}

// platformPreflightManifestWithNamespace renders the same Job shape as
// platformPreflightJobManifest but with an explicit metadata.namespace, for
// tests that need to prove a --namespace override wins over it.
func platformPreflightManifestWithNamespace(namespace string) string {
	return `
apiVersion: batch/v1
kind: Job
metadata:
  name: demo-job
  namespace: ` + namespace + `
spec:
  template:
    spec:
      serviceAccountName: tau-workload
      imagePullSecrets:
        - name: acr-secret
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: blob-training
      containers:
        - name: main
          image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          env:
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
`
}

func TestRunPlatformPreflightMultiKueue_Success(t *testing.T) {
	runnerA := healthyPlatformPreflightRunner("ray")
	runnerB := healthyPlatformPreflightRunner("ray")
	factory := func(workerContext string) platform.Runner {
		switch workerContext {
		case "worker-a":
			return runnerA
		case "worker-b":
			return runnerB
		default:
			t.Fatalf("unexpected worker context requested: %s", workerContext)
			return nil
		}
	}

	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte(platformPreflightJobManifest),
		[]string{"worker-a", "worker-b"},
		"",
		factory,
		&out,
	)
	if err != nil {
		t.Fatalf("expected success, got: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hf-token") {
		t.Fatalf("expected summary output to mention hf-token, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "INFO") {
		t.Fatalf("expected an INFO line for the image dependency, got:\n%s", out.String())
	}
}

func TestRunPlatformPreflightMultiKueue_MissingDependencyNamesWorker(t *testing.T) {
	runnerA := healthyPlatformPreflightRunner("ray")
	runnerB := &fakeKubeRawRunner{responses: map[string]string{
		"-n ray get serviceaccount tau-workload -o name": "serviceaccount/tau-workload",
		"-n ray get secret acr-secret -o name":           "secret/acr-secret",
		// hf-token and blob-training intentionally omitted from worker-b:
		// fakeKubeRawRunner returns "unexpected kubectl args" for them,
		// which the preflight package treats as a not-found failure.
	}}
	factory := func(workerContext string) platform.Runner {
		switch workerContext {
		case "worker-a":
			return runnerA
		case "worker-b":
			return runnerB
		default:
			t.Fatalf("unexpected worker context requested: %s", workerContext)
			return nil
		}
	}

	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte(platformPreflightJobManifest),
		[]string{"worker-a", "worker-b"},
		"",
		factory,
		&out,
	)
	if err == nil {
		t.Fatalf("expected a preflight failure, got success:\n%s", out.String())
	}
	msg := err.Error()
	for _, want := range []string{"Secret", "hf-token", "worker-b"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestRunPlatformPreflightMultiKueue_NamespaceOverrideWinsOverManifestNamespace(t *testing.T) {
	runner := &fakeKubeRawRunner{responses: map[string]string{
		"-n other get serviceaccount tau-workload -o name": "serviceaccount/tau-workload",
		"-n other get secret acr-secret -o name":           "secret/acr-secret",
		"-n other get secret hf-token -o name":             "secret/hf-token",
		"-n other get pvc blob-training -o json":           `{"spec":{"storageClassName":"blob-premium-rwx"},"status":{"phase":"Bound"}}`,
		"get storageclass blob-premium-rwx -o name":        "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
	factory := func(workerContext string) platform.Runner {
		if workerContext != "worker-a" {
			t.Fatalf("unexpected worker context requested: %s", workerContext)
		}
		return runner
	}

	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte(platformPreflightManifestWithNamespace("ray")),
		[]string{"worker-a"},
		"other",
		factory,
		&out,
	)
	if err != nil {
		t.Fatalf("expected --namespace to override the manifest's own namespace, got: %v\noutput:\n%s", err, out.String())
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "-n ray ") {
			t.Fatalf("expected no kubectl call against manifest namespace ray when --namespace other is set, got call %q (all calls: %v)", call, runner.calls)
		}
	}
	if !strings.Contains(out.String(), "other/hf-token") {
		t.Fatalf("expected summary to reflect override namespace, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "ray/hf-token") {
		t.Fatalf("summary must not report manifest namespace when override is set:\n%s", out.String())
	}
}

func TestRunPlatformPreflightMultiKueue_MissingManifestNamespaceFailsBeforeKubectl(t *testing.T) {
	runner := healthyPlatformPreflightRunner("ray")
	factory := func(workerContext string) platform.Runner {
		if workerContext != "worker-a" {
			t.Fatalf("unexpected worker context requested: %s", workerContext)
		}
		return runner
	}

	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte(platformPreflightJobManifestWithoutNamespace),
		[]string{"worker-a"},
		"",
		factory,
		&out,
	)
	if err == nil {
		t.Fatalf("expected missing manifest namespace to fail")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected fail-fast before any kubectl call, got %+v", runner.calls)
	}
	msg := err.Error()
	for _, want := range []string{"Secret", "hf-token", "metadata.namespace", "--namespace"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to mention %q, got: %s", want, msg)
		}
	}
	if !strings.Contains(out.String(), "hf-token") {
		t.Fatalf("expected summary output to mention failing dependency, got:\n%s", out.String())
	}
}

func TestRunPlatformPreflightMultiKueue_RequiresWorkerContext(t *testing.T) {
	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte(platformPreflightJobManifest),
		nil,
		"",
		func(string) platform.Runner { return &fakeKubeRawRunner{} },
		&out,
	)
	if err == nil || !strings.Contains(err.Error(), "worker-context") {
		t.Fatalf("expected an error requiring --worker-context, got: %v", err)
	}
}

func TestRunPlatformPreflightMultiKueue_RejectsManifestWithNoWorkload(t *testing.T) {
	var out bytes.Buffer
	err := runPlatformPreflightMultiKueue(
		context.Background(),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n"),
		[]string{"worker-a"},
		"",
		func(string) platform.Runner { return &fakeKubeRawRunner{} },
		&out,
	)
	if err == nil {
		t.Fatalf("expected an error when the manifest has no Job/RayJob document")
	}
}

func TestReadPlatformPreflightManifest_RequiresPath(t *testing.T) {
	if _, err := readPlatformPreflightManifest(""); err == nil {
		t.Fatalf("expected an error for an empty manifest path")
	}
}

func TestNewPlatformCmd_HiddenAndWired(t *testing.T) {
	cmd := newPlatformCmd()
	if !cmd.Hidden {
		t.Fatalf("tau platform must be Hidden (operator-only, not researcher-facing)")
	}
	sub, _, err := cmd.Find([]string{"preflight-multikueue"})
	if err != nil {
		t.Fatalf("expected preflight-multikueue subcommand to be registered: %v", err)
	}
	if sub.Use != "preflight-multikueue" {
		t.Fatalf("unexpected subcommand found: %s", sub.Use)
	}
}
