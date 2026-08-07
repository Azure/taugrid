package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/runs"
)

type listRawRunner struct {
	args [][]string
}

func (r *listRawRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	r.args = append(r.args, append([]string(nil), args...))
	return `{"items":[]}`, nil
}

func TestRunListReaderScopesKubernetesListsToNamespace(t *testing.T) {
	raw := &listRawRunner{}
	reader := runListReader{raw: raw}
	if _, err := reader.ListJobs(context.Background(), "team-alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ListRayJobs(context.Background(), "team-beta"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(raw.args[0], " "); !strings.Contains(got, "-n team-alpha") {
		t.Fatalf("Job args = %q", got)
	}
	if got := strings.Join(raw.args[1], " "); !strings.Contains(got, "-n team-beta") {
		t.Fatalf("RayJob args = %q", got)
	}
}

func TestWriteRunListExposesHistoryStateInTableAndJSON(t *testing.T) {
	snapshot := runs.Snapshot{
		Namespace:         "team-alpha",
		HistoryState:      "history-unavailable",
		HistoryDiagnostic: "durable run history query failed",
		Total:             1,
		Runs:              []runs.Run{{Name: "live", Kind: "Job", Status: "Running", Age: "1m"}},
	}
	var table, warnings bytes.Buffer
	if err := writeRunList(&table, &warnings, "team-alpha", "table", snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "History: history-unavailable") || !strings.Contains(table.String(), "live") {
		t.Fatalf("table output = %q", table.String())
	}
	if !strings.Contains(warnings.String(), "durable run history query failed") {
		t.Fatalf("warning output = %q", warnings.String())
	}

	var raw bytes.Buffer
	if err := writeRunList(&raw, &warnings, "team-alpha", "json", snapshot); err != nil {
		t.Fatal(err)
	}
	var got struct {
		HistoryState string `json:"historyState"`
		Namespace    string `json:"namespace"`
	}
	if err := json.Unmarshal(raw.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HistoryState != "history-unavailable" || got.Namespace != "team-alpha" {
		t.Fatalf("JSON snapshot = %+v", got)
	}
}

func TestRunListRegistersDurableHistoryFlags(t *testing.T) {
	cmd := newRunListCmd()
	for _, name := range []string{
		"kusto-endpoint", "kusto-database", "kusto-query-command", "kusto-query-arg",
		"kusto-table", "kusto-cluster", "kusto-workspace-id", "history-limit", "output", "include-external",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestWriteRunListShowsSourceOnlyForExternalListing(t *testing.T) {
	snapshot := runs.Snapshot{
		HistoryState: "live-only",
		Total:        2,
		Runs: []runs.Run{
			{Name: "managed", Kind: "Job", Status: "Running", Age: "1m", Source: "tau"},
			{Name: "raw", Kind: "Job", Status: "Pending", Age: "2m", Source: "external"},
		},
	}
	var table, warnings bytes.Buffer
	if err := writeRunList(&table, &warnings, "tau-default", "table", snapshot); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SOURCE", "tau", "external"} {
		if !strings.Contains(table.String(), want) {
			t.Fatalf("table %q missing %q", table.String(), want)
		}
	}
}
