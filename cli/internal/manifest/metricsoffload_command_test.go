// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/metricsoffload"
)

func TestMetricsOffloadTemplatesUseResolvedRuntimeCommand(t *testing.T) {
	for _, name := range []string{"managed-workflow-rayjob.yaml.tmpl", "managed-workflow-rayjob-eval.yaml.tmpl"} {
		raw, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		if !strings.Contains(body, "metrics-offload") {
			t.Fatalf("%s no longer renders the metrics-offload sidecar; this guard is now vacuous", name)
		}
		if !strings.Contains(body, `command: [{{.MetricsOffload.CommandYAML}}]`) ||
			!strings.Contains(body, `args: [{{.MetricsOffload.ArgsPrefixYAML}}, "--done-file"`) {
			t.Fatalf("%s does not render the resolved runtime command; got command lines: %v",
				name, commandLines(body))
		}
		for _, hardcoded := range []string{metricsoffload.SidecarCommand, metricsoffload.CollectorSidecarCommand} {
			if strings.Contains(body, hardcoded) {
				t.Fatalf("%s hard-codes runtime command %s", name, hardcoded)
			}
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
