// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type verifierFakeRunner struct {
	responses map[string]string
}

func (f *verifierFakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	key := strings.Join(args, " ")
	if response, ok := f.responses[key]; ok {
		return response, nil
	}
	return "", fmt.Errorf("unexpected kubectl args: %s", key)
}

func TestKubectlVerifierProjectsWorkspaceAndPermissions(t *testing.T) {
	descriptor, err := Parse([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatal(err)
	}
	runner := &verifierFakeRunner{responses: map[string]string{
		"-n tau-platform get workspace.tau.azure.com sample -o json": `{
		  "apiVersion": "tau.azure.com/v1alpha1",
		  "kind": "TauWorkspace",
		  "metadata": {"name": "sample", "namespace": "tau-platform", "uid": "workspace-uid", "generation": 7},
		  "spec": {"queue": "jobqueue"},
		  "status": {
		    "phase": "Ready",
		    "observedGeneration": 7,
		    "target": {"resolvedNamespace": "sample"},
		    "queue": {"localQueue": "jobqueue"}
		  }
		}`,
		"-n sample get localqueue.kueue.x-k8s.io jobqueue -o name": "localqueue.kueue.x-k8s.io/jobqueue\n",
		"auth can-i * * --all-namespaces":                          "yes\n",
	}}
	verifier := KubectlVerifier{NewRunner: func(string, string) rawRunner { return runner }}

	got, err := verifier.Verify(context.Background(), descriptor, "/tmp/kubeconfig")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Namespace != "sample" || got.Queue != "jobqueue" || got.WorkspaceUID != "workspace-uid" || got.WorkspaceRevision != "7" {
		t.Fatalf("verification = %#v", got)
	}
}

func TestKubectlVerifierRejectsStaleReadyGeneration(t *testing.T) {
	descriptor, err := Parse([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatal(err)
	}
	runner := &verifierFakeRunner{responses: map[string]string{
		"-n tau-platform get workspace.tau.azure.com sample -o json": `{
		  "apiVersion": "tau.azure.com/v1alpha1",
		  "kind": "TauWorkspace",
		  "metadata": {"name": "sample", "namespace": "tau-platform", "uid": "workspace-uid", "generation": 8},
		  "spec": {"queue": "jobqueue"},
		  "status": {
		    "phase": "Ready",
		    "observedGeneration": 7,
		    "target": {"resolvedNamespace": "sample"},
		    "queue": {"localQueue": "jobqueue"}
		  }
		}`,
	}}
	verifier := KubectlVerifier{NewRunner: func(string, string) rawRunner { return runner }}

	_, err = verifier.Verify(context.Background(), descriptor, "/tmp/kubeconfig")
	if err == nil ||
		!strings.Contains(err.Error(), "is not Ready") ||
		!strings.Contains(err.Error(), "observedGeneration=7") ||
		!strings.Contains(err.Error(), "generation=8") {
		t.Fatalf("expected stale-generation readiness failure, got %v", err)
	}
}

func TestKubectlVerifierReportsMissingPermission(t *testing.T) {
	descriptor, err := Parse([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatal(err)
	}
	descriptor.Authorization.Mode = AuthorizationModeWorkspaceRBAC
	descriptor.Authorization.RequiredRole = "tau-researcher-v1"
	runner := &verifierFakeRunner{responses: map[string]string{
		"-n tau-platform get workspace.tau.azure.com sample -o json": `{
		  "apiVersion": "tau.azure.com/v1alpha1",
		  "kind": "TauWorkspace",
		  "metadata": {"name": "sample", "uid": "workspace-uid", "generation": 1},
		  "spec": {"queue": "jobqueue"},
		  "status": {
		    "phase": "Ready",
		    "observedGeneration": 1,
		    "target": {"resolvedNamespace": "sample"},
		    "queue": {"localQueue": "jobqueue"}
		  }
		}`,
		"-n sample get localqueue.kueue.x-k8s.io jobqueue -o name": "localqueue.kueue.x-k8s.io/jobqueue\n",
		"-n sample auth can-i create jobs.batch":                   "no\n",
	}}
	verifier := KubectlVerifier{NewRunner: func(string, string) rawRunner { return runner }}
	_, err = verifier.Verify(context.Background(), descriptor, "/tmp/kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "create jobs.batch") || !strings.Contains(err.Error(), "tau-researcher-v1") {
		t.Fatalf("expected actionable permission error, got %v", err)
	}
}

func TestKubectlVerifierRejectsNonBroadClusterWideCredential(t *testing.T) {
	descriptor, err := Parse([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatal(err)
	}
	runner := &verifierFakeRunner{responses: map[string]string{
		"-n tau-platform get workspace.tau.azure.com sample -o json": `{
		  "apiVersion": "tau.azure.com/v1alpha1",
		  "kind": "TauWorkspace",
		  "metadata": {"name": "sample", "uid": "workspace-uid", "generation": 1},
		  "spec": {"queue": "jobqueue"},
		  "status": {
		    "phase": "Ready",
		    "observedGeneration": 1,
		    "target": {"resolvedNamespace": "sample"},
		    "queue": {"localQueue": "jobqueue"}
		  }
		}`,
		"-n sample get localqueue.kueue.x-k8s.io jobqueue -o name": "localqueue.kueue.x-k8s.io/jobqueue\n",
		"auth can-i * * --all-namespaces":                          "no\n",
	}}
	verifier := KubectlVerifier{NewRunner: func(string, string) rawRunner { return runner }}
	_, err = verifier.Verify(context.Background(), descriptor, "/tmp/kubeconfig")
	if err == nil || !strings.Contains(err.Error(), "not authorized across the cluster") {
		t.Fatalf("expected cluster-wide authorization error, got %v", err)
	}
}
