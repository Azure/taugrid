// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type fakeKubeRawRunner struct {
	responses map[string]string
	calls     []string
}

func (f *fakeKubeRawRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if response, ok := f.responses[key]; ok {
		return response, nil
	}
	return "", fmt.Errorf("unexpected kubectl args: %s", key)
}

func TestValidateFinetuneStorageNodeCompatibilityRejectsPinnedIncompatibleGPU(t *testing.T) {
	runner := fakeStorageCompatibilityRunner()
	manifest := storageCompatibilityManifest()
	err := validateStorageNodeCompatibility(
		context.Background(),
		runner,
		"ray",
		manifest,
		map[string]string{workloadmeta.LabelGPUClass: "a100-80gb"},
	)
	if err == nil {
		t.Fatalf("expected incompatible PVC/GPU selector to fail")
	}
	got := err.Error()
	for _, want := range []string{
		`storage.data_pvc "lustre-research"`,
		`azurelustre.csi.azure.com`,
		workloadmeta.LabelGPUClass + `=a100-80gb`,
		`flex-a100-scus-01000001`,
		`driver is registered on non-matching node(s): aks-a10-38546571-vmss000000`,
		`--node-selector/--gpu-class/--preset`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("error missing %q:\n%s", want, got)
		}
	}
}

func TestValidateFinetuneStorageNodeCompatibilityAllowsCompatiblePoolSelector(t *testing.T) {
	runner := fakeStorageCompatibilityRunner()
	manifest := storageCompatibilityManifest()
	err := validateStorageNodeCompatibility(
		context.Background(),
		runner,
		"ray",
		manifest,
		map[string]string{"kubernetes.azure.com/agentpool": "a10"},
	)
	if err != nil {
		t.Fatalf("compatible A10 selector should pass: %v", err)
	}
}

func TestValidateFinetuneStorageNodeCompatibilityRejectsBroadSelectorWithUnsafeNodes(t *testing.T) {
	runner := fakeStorageCompatibilityRunner()
	manifest := storageCompatibilityManifest()
	err := validateStorageNodeCompatibility(
		context.Background(),
		runner,
		"ray",
		manifest,
		nil,
	)
	if err == nil {
		t.Fatalf("expected broad selector to fail when a matching GPU node lacks the CSI driver")
	}
	got := err.Error()
	if !strings.Contains(got, `node selector <none> still matches node(s) without that driver: flex-a100-scus-01000001`) {
		t.Fatalf("error should name the unsafe matching node:\n%s", got)
	}
	if !strings.Contains(got, `compatible matching node(s): aks-a10-38546571-vmss000000`) {
		t.Fatalf("error should name compatible matching nodes:\n%s", got)
	}
}

func TestValidateFinetuneStorageNodeCompatibilityEvalIncludesCPUWorkerNodes(t *testing.T) {
	runner := fakeEvalStorageCompatibilityRunner()
	manifest := storageCompatibilityManifest()
	manifest.Eval.CPUWorkers = 2
	manifest.Eval.Upstream = "train-run"
	err := validateStorageNodeCompatibility(
		context.Background(),
		runner,
		"ray",
		manifest,
		nil,
	)
	if err == nil {
		t.Fatalf("expected eval worker-compatible selector to fail when a CPU worker node lacks the CSI driver")
	}
	if !strings.Contains(err.Error(), "cpu-no-lustre") {
		t.Fatalf("error should include unsafe CPU worker node:\n%s", err)
	}
}

func TestValidateFinetuneStorageNodeCompatibilitySkipsWhenNoExplicitPVC(t *testing.T) {
	runner := &fakeKubeRawRunner{}
	manifest := &manifest.Manifest{}
	manifest.Compute.GPUs = 1
	if err := validateStorageNodeCompatibility(context.Background(), runner, "ray", manifest, nil); err != nil {
		t.Fatalf("no explicit PVC should skip preflight: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no explicit PVC should not call kubectl, got calls: %v", runner.calls)
	}
}

func TestCSIDriverForPVCExplainsExternalProvisioningWhenClaimIsMissing(t *testing.T) {
	runner := &fakeKubeRawRunner{}
	_, err := csiDriverForPVC(context.Background(), runner, "research", storagePVCRef{
		Field: "storage.data_pvc",
		Name:  "training-data",
	})
	if err == nil {
		t.Fatal("expected missing platform-managed PVC to fail")
	}
	for _, want := range []string{"platform-managed PVC", `namespace "research"`, "pre-provision and bind it"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestCSIDriverForPVCExplainsExternalLifecycleWhenClaimIsPending(t *testing.T) {
	runner := &fakeKubeRawRunner{responses: map[string]string{
		"-n research get pvc training-data -o json": `{"status":{"phase":"Pending"}}`,
	}}
	_, err := csiDriverForPVC(context.Background(), runner, "research", storagePVCRef{
		Field: "storage.data_pvc",
		Name:  "training-data",
	})
	if err == nil {
		t.Fatal("expected unbound platform-managed PVC to fail")
	}
	for _, want := range []string{"platform-managed PVC is not Bound", `namespace "research"`, "platform storage lifecycle"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func storageCompatibilityManifest() *manifest.Manifest {
	m := &manifest.Manifest{}
	m.Name = "vision-smoke"
	m.Compute.GPUs = 1
	m.Storage.DataPVC = "lustre-research"
	return m
}

func fakeStorageCompatibilityRunner() *fakeKubeRawRunner {
	return &fakeKubeRawRunner{responses: map[string]string{
		"-n ray get pvc lustre-research -o json": `{
			"spec": {"volumeName": "pv-lustre-research"},
			"status": {"phase": "Bound"}
		}`,
		"get pv pv-lustre-research -o json": `{
			"spec": {"csi": {"driver": "azurelustre.csi.azure.com"}}
		}`,
		"get nodes -o json": `{
			"items": [
				{
					"metadata": {
						"name": "flex-a100-scus-01000001",
						"labels": {
							"` + workloadmeta.LabelGPUClass + `": "a100-80gb",
							"nvidia.com/gpu.count": "1"
						}
					},
					"status": {
						"allocatable": {"nvidia.com/gpu": "1"},
						"conditions": [{"type": "Ready", "status": "True"}]
					}
				},
				{
					"metadata": {
						"name": "aks-a10-38546571-vmss000000",
						"labels": {
							"kubernetes.azure.com/agentpool": "a10",
							"nvidia.com/gpu.present": "true"
						}
					},
					"status": {
						"allocatable": {"nvidia.com/gpu": "1"},
						"conditions": [{"type": "Ready", "status": "True"}]
					}
				},
				{
					"metadata": {
						"name": "aks-nodepool1-21720224-vmss000000",
						"labels": {"kubernetes.azure.com/agentpool": "nodepool1"}
					},
					"status": {
						"conditions": [{"type": "Ready", "status": "True"}]
					}
				}
			]
		}`,
		"get csinode -o json": `{
			"items": [
				{
					"metadata": {"name": "flex-a100-scus-01000001"},
					"spec": {"drivers": [{"name": "file.csi.azure.com"}]}
				},
				{
					"metadata": {"name": "aks-a10-38546571-vmss000000"},
					"spec": {"drivers": [{"name": "azurelustre.csi.azure.com"}]}
				},
				{
					"metadata": {"name": "aks-nodepool1-21720224-vmss000000"},
					"spec": {"drivers": [{"name": "azurelustre.csi.azure.com"}]}
				}
			]
		}`,
	}}
}

func fakeEvalStorageCompatibilityRunner() *fakeKubeRawRunner {
	return &fakeKubeRawRunner{responses: map[string]string{
		"-n ray get pvc lustre-research -o json": `{
			"spec": {"volumeName": "pv-lustre-research"},
			"status": {"phase": "Bound"}
		}`,
		"get pv pv-lustre-research -o json": `{
			"spec": {"csi": {"driver": "azurelustre.csi.azure.com"}}
		}`,
		"get nodes -o json": `{
			"items": [
				{
					"metadata": {
						"name": "gpu-with-lustre",
						"labels": {"nvidia.com/gpu.count": "1"}
					},
					"status": {
						"allocatable": {"nvidia.com/gpu": "1"},
						"conditions": [{"type": "Ready", "status": "True"}]
					}
				},
				{
					"metadata": {
						"name": "cpu-no-lustre",
						"labels": {"kubernetes.azure.com/agentpool": "cpu"}
					},
					"status": {
						"conditions": [{"type": "Ready", "status": "True"}]
					}
				}
			]
		}`,
		"get csinode -o json": `{
			"items": [
				{
					"metadata": {"name": "gpu-with-lustre"},
					"spec": {"drivers": [{"name": "azurelustre.csi.azure.com"}]}
				},
				{
					"metadata": {"name": "cpu-no-lustre"},
					"spec": {"drivers": [{"name": "file.csi.azure.com"}]}
				}
			]
		}`,
	}}
}
