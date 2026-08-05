package expcockpit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedAssetsUseExperimentCommand(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("assets", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	asset := string(raw)
	if strings.Contains(asset, "tau exp ") {
		t.Fatalf("generated cockpit asset must not depend on the removed exp compatibility root")
	}
	// Stellar split out of the tau CLI, so `tau experiment ...` no longer
	// resolves. Copy shown to users must name the binary that has the verb.
	if strings.Contains(asset, "\"tau experiment") || strings.Contains(asset, ", \"tau ") {
		t.Fatalf("generated cockpit asset must not tell users to run the tau binary; Stellar lives in taugrid-portal")
	}
	for _, want := range []string{
		"taugrid-portal experiment track RUN",
		"taugrid-portal experiment import jsonl",
		"taugrid-portal experiment observe --scope run:RUN",
	} {
		if !strings.Contains(asset, want) {
			t.Fatalf("generated cockpit asset missing %q", want)
		}
	}
}

func TestGeneratedAssetsUseExperimentGroupWording(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("assets", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	asset := string(raw)
	for _, want := range []string{
		"All groups",
		"Metric and group",
		"experiment: snapshot.experiment || summary.experiment",
	} {
		if !strings.Contains(asset, want) {
			t.Fatalf("generated cockpit asset missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"/api/stellar/labels",
		"/api/stellar/dashboards",
		"Set experiment target label",
		"Set run group label",
		"Save dashboard",
		"missing overlays",
	} {
		if strings.Contains(asset, forbidden) {
			t.Fatalf("read-only cockpit asset must not contain mutable UI surface %q", forbidden)
		}
	}
}

func TestPinnedMetricInteractionsLoadSnapshotsInPlace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("assets", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	asset := string(raw)
	for _, name := range []string{"pinVisibleMetricGroups", "setFocusMetric", "togglePinnedMetric"} {
		block := jsFunctionBlock(t, asset, name)
		if strings.Contains(block, "fetchSnapshot(") {
			t.Fatalf("%s should not trigger a full dashboard snapshot reload:\n%s", name, block)
		}
		if !strings.Contains(block, "updateMetricSelectionInPlace()") {
			t.Fatalf("%s should update pinned metric state in place:\n%s", name, block)
		}
	}
	for _, want := range []string{
		"immediatelyVisibleMetricNames(selectedMetricNames)",
		"loadMetricSnapshotsInBackground(backgroundMetricNames",
		"loadVisibleMetricSnapshots();",
	} {
		if !strings.Contains(asset, want) {
			t.Fatalf("generated cockpit asset missing in-place/lazy metric loading hook %q", want)
		}
	}
}

func jsFunctionBlock(t *testing.T, source, name string) string {
	t.Helper()
	prefix := "function " + name + "("
	start := strings.Index(source, prefix)
	if start < 0 {
		t.Fatalf("asset missing %s", prefix)
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		t.Fatalf("asset missing function body for %s", name)
	}
	index := start + open
	depth := 0
	for ; index < len(source); index++ {
		switch source[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : index+1]
			}
		}
	}
	t.Fatalf("asset has unterminated function body for %s", name)
	return ""
}
