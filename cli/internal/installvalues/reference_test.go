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
		"tau-core-controller.tauCluster.nodeLabelRules",
		"taugrid-core.stellar.enabled",
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
