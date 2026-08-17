// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kube"
)

// The per-run SecretProviderClass that manifest.Render emits for Key Vault
// workloads is applied as an ancillary document. An ownerReference cannot be
// rendered into it because the owning Job/RayJob UID does not exist until the
// API server has created the workload, so tau adopts it immediately after the
// apply. Without that adoption every Key Vault run leaves an orphan that
// Kubernetes garbage collection can never reclaim.
//
// These tests drive the real argv construction through a kubectl stub rather
// than a mocked seam, so a change to the resource name, the patch shape, or the
// owner lookup is caught here.

// ownerRefArgsLog installs a kubectl stub that records every invocation and
// answers the owner UID lookup. It returns the path of the argv log.
func ownerRefArgsLog(t *testing.T, uid string) (*kube.Runner, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("TAU_OWNERREF_ARGS_LOG", log)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TAU_OWNERREF_ARGS_LOG"
case "$*" in
  *jsonpath*) printf '%s' '` + uid + `' ;;
  *)          printf '%s' 'patched' ;;
esac
`
	return fakeKubectlRunner(t, script), log
}

func readArgsLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestPatchSecretProviderClassOwnerRefAdoptsJob(t *testing.T) {
	runner, log := ownerRefArgsLog(t, "uid-job-1")

	if err := patchSecretProviderClassOwnerRef(context.Background(), runner, "tau-default", "demo-train-kv", "demo-train", "job"); err != nil {
		t.Fatalf("patchSecretProviderClassOwnerRef: %v", err)
	}

	lines := readArgsLog(t, log)
	if len(lines) != 2 {
		t.Fatalf("expected a UID lookup then a patch, got %d invocations: %v", len(lines), lines)
	}

	// The owner UID must be read from the batch Job, not the SPC.
	if !strings.Contains(lines[0], "get jobs.batch demo-train") {
		t.Errorf("owner UID lookup did not target the Job: %q", lines[0])
	}
	if !strings.Contains(lines[0], "jsonpath={.metadata.uid}") {
		t.Errorf("owner UID lookup did not request the uid: %q", lines[0])
	}

	patch := lines[1]
	if !strings.Contains(patch, "patch "+secretProviderClassResource+" demo-train-kv") {
		t.Errorf("patch did not target the SecretProviderClass: %q", patch)
	}
	// Both flags matter: controller=true makes this the managing reference, and
	// blockOwnerDeletion=true keeps the SPC from outliving a foreground delete.
	for _, want := range []string{
		`"apiVersion":"batch/v1"`,
		`"kind":"Job"`,
		`"name":"demo-train"`,
		`"uid":"uid-job-1"`,
		`"controller":true`,
		`"blockOwnerDeletion":true`,
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %s: %q", want, patch)
		}
	}
}

func TestPatchSecretProviderClassOwnerRefAdoptsRayJob(t *testing.T) {
	runner, log := ownerRefArgsLog(t, "uid-ray-1")

	if err := patchSecretProviderClassOwnerRef(context.Background(), runner, "tau-default", "demo-ray-kv", "demo-ray", "rayjob"); err != nil {
		t.Fatalf("patchSecretProviderClassOwnerRef: %v", err)
	}

	lines := readArgsLog(t, log)
	if len(lines) != 2 {
		t.Fatalf("expected a UID lookup then a patch, got %d invocations: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "get rayjobs.ray.io demo-ray") {
		t.Errorf("owner UID lookup did not target the RayJob: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"apiVersion":"ray.io/v1"`) || !strings.Contains(lines[1], `"kind":"RayJob"`) {
		t.Errorf("patch did not name the RayJob owner: %q", lines[1])
	}
}

// An empty UID means the owner lookup succeeded but returned nothing. Patching
// an ownerReference with an empty UID produces an object Kubernetes will never
// collect, so this must fail loudly and let the caller run cleanup.
func TestPatchSecretProviderClassOwnerRefRejectsEmptyUID(t *testing.T) {
	runner, _ := ownerRefArgsLog(t, "")

	err := patchSecretProviderClassOwnerRef(context.Background(), runner, "tau-default", "demo-train-kv", "demo-train", "job")
	if err == nil {
		t.Fatal("expected an error when the owner UID is empty")
	}
	if !strings.Contains(err.Error(), "empty uid") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOwnerRefTargetForWorkloadKinds(t *testing.T) {
	for _, tc := range []struct {
		workloadKind string
		wantKind     string
		wantAPI      string
		wantResource string
	}{
		{"job", "Job", "batch/v1", "jobs.batch"},
		{"rayjob", "RayJob", "ray.io/v1", "rayjobs.ray.io"},
		{"rayjob-eval", "RayJob", "ray.io/v1", "rayjobs.ray.io"},
	} {
		got := ownerRefTargetFor(tc.workloadKind, "demo")
		if got.Kind != tc.wantKind || got.APIVersion != tc.wantAPI || got.Resource != tc.wantResource {
			t.Errorf("ownerRefTargetFor(%q) = %+v, want kind=%s api=%s resource=%s",
				tc.workloadKind, got, tc.wantKind, tc.wantAPI, tc.wantResource)
		}
		if got.Name != "demo" {
			t.Errorf("ownerRefTargetFor(%q) name = %q, want demo", tc.workloadKind, got.Name)
		}
	}
}

// Cleanup of a half-submitted run deletes the SPC by GVR, so the resource
// string used for cleanup has to resolve.
func TestRunSubmissionGVRResolvesSecretProviderClass(t *testing.T) {
	for _, resource := range []string{
		secretProviderClassResource,
		"secretproviderclass",
		"secretproviderclasses",
	} {
		gvr, err := runSubmissionGVR(resource)
		if err != nil {
			t.Fatalf("runSubmissionGVR(%q): %v", resource, err)
		}
		if gvr.Group != "secrets-store.csi.x-k8s.io" || gvr.Version != "v1" || gvr.Resource != "secretproviderclasses" {
			t.Errorf("runSubmissionGVR(%q) = %+v", resource, gvr)
		}
	}
}
