// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

type crdUpgradeRunner struct {
	steps  *[]string
	failAt string
}

func (r crdUpgradeRunner) Raw(_ context.Context, args []string, stdin []byte) (string, error) {
	if args[0] == "get" {
		return `{"items":[]}`, nil
	}
	step := args[0]
	if containsArg(args, "--dry-run=server") {
		step = "validate"
	}
	*r.steps = append(*r.steps, step)
	if !strings.Contains(string(stdin), "workspaces.tau.azure.com") {
		return "", errors.New("CRD upgrade did not supply the workspace schema")
	}
	if step == r.failAt {
		return "", errors.New("fixture failure")
	}
	return "", nil
}

func TestClusterInstallUpgradesCRDsBeforeExistingRelease(t *testing.T) {
	crd, err := os.ReadFile("../../../charts/tau-core-controller/crds/tau.azure.com_workspaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, failAt := range []string{"", "validate", "apply", "wait"} {
		t.Run("fail="+failAt, func(t *testing.T) {
			installFakeInstallationValidation(t)
			originalHelm := runHelmCommand
			var steps []string
			runHelmCommand = func(_ context.Context, _ io.Reader, out, _ io.Writer, args []string) error {
				switch args[0] {
				case "list":
					_, _ = io.WriteString(out, `[{"name":"taugrid","namespace":"tau-system"}]`)
				case "get":
					_, _ = io.WriteString(out, "{}")
				case "template":
					if !containsArgPair(args, "--version", "0.4.3") ||
						!containsArgPair(args, "--values", "cluster.yaml") ||
						!containsArg(args, "--include-crds") {
						t.Fatalf("CRD render must use the install's chart version and values: %v", args)
					}
					steps = append(steps, "render")
					_, _ = out.Write(crd)
				case "upgrade":
					steps = append(steps, "helm")
				default:
					t.Fatalf("unexpected Helm command: %v", args)
				}
				return nil
			}
			t.Cleanup(func() { runHelmCommand = originalHelm })
			newInstallationCheckRunner = func(kubeContext string) installationcheck.Runner {
				if kubeContext != "kind-fixture" {
					t.Fatalf("CRD upgrade context = %q", kubeContext)
				}
				return crdUpgradeRunner{steps: &steps, failAt: failAt}
			}
			_, err := runCluster(t, "install", "--context=kind-fixture", "--values=cluster.yaml")
			want := []string{"render", "validate", "apply", "wait", "helm"}
			if failAt != "" {
				for i, step := range want {
					if step == failAt {
						want = want[:i+1]
						break
					}
				}
				if err == nil || !strings.Contains(err.Error(), "fixture failure") {
					t.Fatalf("upgrade error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(steps, want) {
				t.Fatalf("upgrade order = %v, want %v", steps, want)
			}
		})
	}
}

func TestTauGridCRDManifestExcludesWorkloadsAndThirdPartyCRDs(t *testing.T) {
	workspaceCRD, err := os.ReadFile("../../../charts/tau-core-controller/crds/tau.azure.com_workspaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rendered := "Updating chart dependencies\n---\n" + string(workspaceCRD) + `
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: rayservices.ray.io
spec:
  group: ray.io
---
apiVersion: tau.azure.com/v1alpha1
kind: TauWorkspace
metadata:
  name: must-not-apply
`
	got, err := tauGridCRDManifest([]byte(rendered))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workspaces.tau.azure.com", "- researcher", "- tau-researcher-v1"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("CRD output missing %q", want)
		}
	}
	for _, unwanted := range []string{"rayservices.ray.io", "must-not-apply"} {
		if strings.Contains(string(got), unwanted) {
			t.Fatalf("CRD output must not include %q", unwanted)
		}
	}
}
