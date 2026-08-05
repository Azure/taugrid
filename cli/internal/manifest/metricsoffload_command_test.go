package manifest

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/metricsoffload"
)

// The metrics offload sidecar runs taugrid-portal verbs, so it has to exec the
// taugrid-portal binary. Two of the three render paths are text templates,
// which no compiler checks against metricsoffload.SidecarCommand.
func TestMetricsOffloadTemplatesExecThePortalBinary(t *testing.T) {
	want := `command: ["` + metricsoffload.SidecarCommand + `"]`

	for _, name := range []string{"managed-workflow-rayjob.yaml.tmpl", "managed-workflow-rayjob-eval.yaml.tmpl"} {
		raw, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		if !strings.Contains(body, "metrics-offload") {
			t.Fatalf("%s no longer renders the metrics-offload sidecar; this guard is now vacuous", name)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("%s does not exec %s; got the sidecar command lines: %v",
				name, metricsoffload.SidecarCommand, commandLines(body))
		}
	}
}

func commandLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "command:") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}
