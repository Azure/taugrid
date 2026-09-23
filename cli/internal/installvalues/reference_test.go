// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installvalues

import (
	"strings"
	"testing"
)

func TestReferenceMarkdownContainsCriticalFields(t *testing.T) {
	md := ReferenceMarkdown()

	required := []string{
		"baselineQueue.enabled",
		"baselineQueue.gpu.flavors",
		"baselineQueue.name",
		"components.kueue.enabled",
		"components.gpuMonitoring.enabled",
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

func TestReferenceMarkdownIsMarkdownTable(t *testing.T) {
	md := ReferenceMarkdown()
	if !strings.Contains(md, "| Field | Type | Default | Description |") {
		t.Error("ReferenceMarkdown() missing table header")
	}
}

func TestReferenceMarkdownGPUExampleUsesKECFlavor(t *testing.T) {
	md := ReferenceMarkdown()
	for _, required := range []string{
		"name: aks-h200-ndisr-v5",
		"name: nvidia.com/gpu",
		`nominalQuota: "1"`,
	} {
		if !strings.Contains(md, required) {
			t.Errorf("ReferenceMarkdown() GPU example missing %q", required)
		}
	}
}
