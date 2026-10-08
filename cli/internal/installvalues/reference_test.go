// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installvalues

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReferenceMarkdownContainsCriticalFields(t *testing.T) {
	md := ReferenceMarkdown()

	required := []string{
		"baselineQueue.enabled",
		"baselineQueue.gpu.flavors",
		"baselineQueue.name",
		"components.kueue.enabled",
		"components.gpuMonitoring.enabled",
		"tau-core-controller.tauCluster.nodeLabelRules",
		"taugrid-core.stellar.enabled",
		"taugrid-core.portal.enabled",
		"taugrid-core.portal.serviceAccount.create",
		"taugrid-core.portal.rbac.create",
	}
	for _, field := range required {
		if !strings.Contains(md, field) {
			t.Errorf("ReferenceMarkdown() missing field %q", field)
		}
	}
}

func TestReferenceMarkdownKubeRayVersionMatchesChart(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/taugrid/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Dependencies []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	for _, dependency := range chart.Dependencies {
		if dependency.Name != "kuberay-operator" {
			continue
		}
		parts := strings.Split(dependency.Version, ".")
		if len(parts) != 3 {
			t.Fatalf("unexpected KubeRay chart version %q", dependency.Version)
		}
		want := "embedded KubeRay chart (v" + strings.Join(parts[:2], ".") + ")"
		if !strings.Contains(ReferenceMarkdown(), want) {
			t.Fatalf("ReferenceMarkdown() must describe %q", want)
		}
		return
	}
	t.Fatal("TauGrid chart has no kuberay-operator dependency")
}

func TestReferenceMarkdownIsMarkdownTable(t *testing.T) {
	md := ReferenceMarkdown()
	if !strings.Contains(md, "| Field | Type | Default | Description |") {
		t.Error("ReferenceMarkdown() missing table header")
	}
}

func TestReferenceMarkdownGPUExamplePassesQueueSafetyContract(t *testing.T) {
	md := ReferenceMarkdown()
	for _, required := range []string{
		"nodeTaints:",
		"tolerations: []",
		"name: nvidia.com/gpu",
		`nominalQuota: "1"`,
	} {
		if !strings.Contains(md, required) {
			t.Errorf("ReferenceMarkdown() GPU example missing %q", required)
		}
	}
}
