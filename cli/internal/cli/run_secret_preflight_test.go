// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRayJobMissingRequiredSecretFailsBeforeWorkloadApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}

	appliedMarker := filepath.Join(t.TempDir(), "applied")
	fakeKubectl(t, `
case "$*" in
  *"auth can-i get -n tau-default -- secret/missing-credentials"*)
    printf '%s\n' yes
    ;;
  *"get secret -n tau-default "*" -- missing-credentials"*)
    printf '%s\n' 'Error from server (NotFound): secrets "missing-credentials" not found' >&2
    exit 1
    ;;
  *"create "*)
    : > `+appliedMarker+`
    printf '%s\n' applied
    ;;
  *)
    printf '%s\n' "unexpected kubectl call: $*" >&2
    exit 1
    ;;
esac
`)

	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.script = writeRayScript(t, t.TempDir())
	options.namespace = "tau-default"
	options.envSecrets = []string{"CDS_KEY=missing-credentials:key"}
	attachAuthoritativeProfileForTest(&options)
	request, err := newRunRayJobRequest(options, "solar-irradiation")
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = executeRunRayJob(context.Background(), &stdout, &stderr, &request, "tau run --config tau.yaml")
	if err == nil || !strings.Contains(err.Error(), "required Secret tau-default/missing-credentials does not exist") {
		t.Fatalf("error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if _, statErr := os.Stat(appliedMarker); !os.IsNotExist(statErr) {
		t.Fatalf("workload apply was reached after Secret preflight failure: %v", statErr)
	}
	if strings.Contains(stdout.String(), "applied") {
		t.Fatalf("submission output reported apply after preflight failure: %s", stdout.String())
	}
}

func TestManagedWorkflowPendingSecretUsesPayloadKeysInsteadOfClusterState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}

	logPath := fakeKubectl(t, `
case "$*" in
  *"auth can-i get "*" -- secret/"*|*"get secret "*" -- "*)
    printf '%s\n' "pending Secret must not be read from the cluster: $*" >&2
    exit 1
    ;;
  *localqueue.kueue.x-k8s.io*)
    printf '%s\n' '{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"tau-cq"}}'
    ;;
  *clusterqueue.kueue.x-k8s.io*)
    printf '%s\n' '{"metadata":{"name":"tau-cq"},"spec":{"resourceGroups":[]},"status":{"conditions":[{"type":"Active","status":"True"}]}}'
    ;;
  *"create "*)
    printf '%s\n' 'rayjob.ray.io/tau-secret-preflight created (server dry run)'
    ;;
  *)
    printf '%s\n' "unexpected kubectl call: $*" >&2
    exit 1
    ;;
esac
`)
	request := managedWorkflowSecretRequest(t, "token", map[string]string{"token": "not-printed"})
	request.Options.dryRun = "server"

	var stdout, stderr bytes.Buffer
	if err := executeRunManagedWorkflow(context.Background(), &stdout, &stderr, &request, "tau run --config tau.yaml"); err != nil {
		t.Fatalf("managed workflow submit: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), " -- secret/") || strings.Contains(string(calls), "get secret ") {
		t.Fatalf("pending Secret was read from stale cluster state:\n%s", calls)
	}
	if !strings.Contains(string(calls), "create -n tau-default -f - --dry-run=server") {
		t.Fatalf("server dry-run apply was not reached:\n%s", calls)
	}
}

func TestManagedWorkflowMissingPendingSecretKeyFailsBeforeApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}

	logPath := fakeKubectl(t, `printf '%s\n' "unexpected kubectl call: $*" >&2; exit 1`)
	request := managedWorkflowSecretRequest(t, "token", map[string]string{"other": "not-printed"})

	var stdout, stderr bytes.Buffer
	err := executeRunManagedWorkflow(context.Background(), &stdout, &stderr, &request, "tau run --config tau.yaml")
	if err == nil || err.Error() != "required Secret tau-default/tau-pending-secret is missing keys: token" {
		t.Fatalf("error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if strings.Contains(string(calls), "create ") {
		t.Fatalf("workload create was reached after pending Secret preflight failure:\n%s", calls)
	}
}

func managedWorkflowSecretRequest(t *testing.T, referencedKey string, payload map[string]string) runManagedWorkflowRequest {
	t.Helper()
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: secret-preflight
compute:
  gpus: 0
  workers: 1
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - torch==2.4.0
  env:
    - name: TOKEN
      valueFrom:
        secretKeyRef:
          name: tau-pending-secret
          key: `+referencedKey+`
`)
	payloadPath := filepath.Join(t.TempDir(), "secret-payload.json")
	var entries []string
	for key, value := range payload {
		entries = append(entries, `"`+key+`":"`+value+`"`)
	}
	if err := os.WriteFile(payloadPath, []byte(`{"name":"tau-pending-secret","stringData":{`+strings.Join(entries, ",")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	options := defaultRunDispatchOptions()
	options.file = manifestPath
	options.mainScript = writeMainScript(t)
	options.workloadKind = "rayjob"
	options.namespace = "tau-default"
	options.secretPayloadPath = payloadPath
	attachAuthoritativeProfileForTest(&options)
	setAuthoritativeProfileCardinalityForTest(&options, 0, 1)
	request, err := newRunManagedWorkflowRequest(options)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
