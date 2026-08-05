package installvalues

import (
	"strings"
	"testing"
)

func TestReferenceMarkdownContainsCriticalFields(t *testing.T) {
	md := ReferenceMarkdown()

	required := []string{
		"baselineQueue.enabled",
		"baselineQueue.flavor.tolerations",
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
